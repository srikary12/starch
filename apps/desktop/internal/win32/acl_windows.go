package win32

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// EnsureOwnerOnlyDir creates path with an access control list naming only the
// user this process runs as, and re-applies that list if the directory is
// already there.
//
// This is the Windows half of "the socket is restricted to your account", and
// on Windows it is the shell's job rather than the daemon's: POSIX modes do
// nothing there, so starchd cannot enforce its own 0600/0700 and deliberately
// does not pretend to. §5 of api/README.md states this as an obligation on any
// Windows shell, and this is that obligation being met.
//
// The list is *protected*, meaning inherited entries are dropped rather than
// merged. That is the part worth being deliberate about: %LocalAppData%
// normally passes down a user-only ACL, so inheriting would usually be fine —
// and "usually" is not a thing a guarantee about an API key can rest on. A
// machine with a broadened profile ACL, a roaming policy, or an administrator
// who added a group would otherwise hand those entries straight to our socket.
func EnsureOwnerOnlyDir(path string) error {
	acl, err := ownerOnlyACL()
	if err != nil {
		return err
	}

	wide, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("%s is not a usable path: %w", path, err)
	}

	descriptor, err := windows.NewSecurityDescriptor()
	if err != nil {
		return fmt.Errorf("building a security descriptor: %w", err)
	}
	if err := descriptor.SetDACL(acl, true, false); err != nil {
		return fmt.Errorf("attaching the access list: %w", err)
	}
	// Refuse inherited entries. Without this the list below is merged with
	// whatever the parent hands down.
	if err := descriptor.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED); err != nil {
		return fmt.Errorf("protecting the access list: %w", err)
	}

	attributes := windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}

	err = windows.CreateDirectory(wide, &attributes)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, windows.ERROR_ALREADY_EXISTS):
		// Created by an earlier version, by hand, or by a build that did not
		// do this. Tighten it rather than trusting what is there — the same
		// reasoning as the unconditional chmod on the other two platforms.
		return applyOwnerOnlyACL(path, acl)
	default:
		return fmt.Errorf("creating %s: %w", path, err)
	}
}

// applyOwnerOnlyACL replaces an existing directory's access list.
func applyOwnerOnlyACL(path string, acl *windows.ACL) error {
	err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil,
	)
	if err != nil {
		return fmt.Errorf("securing %s: %w", path, err)
	}
	return nil
}

// ownerOnlyACL builds an access list granting this process's user full control
// and naming nobody else.
//
// Full control rather than something narrower because the daemon has to create,
// bind and delete a socket inside this directory, and the shell has to be able
// to clean up after a daemon that was killed. Narrowing the rights would not
// reduce exposure: the entry names one SID either way.
func ownerOnlyACL() (*windows.ACL, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("looking up the current user: %w", err)
	}

	entries := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		// The socket is created inside this directory by the daemon, so the
		// entry has to reach the contents as well as the directory itself.
		Inheritance: windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}

	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return nil, fmt.Errorf("building the access list: %w", err)
	}
	return acl, nil
}

// DirAccess describes who can reach a directory, for tests and for
// `starch config` to report rather than assert.
type DirAccess struct {
	// Trustees are the SIDs named in the directory's access list, as strings.
	Trustees []string
	// Protected reports whether inherited entries are refused.
	Protected bool
}

// OwnerOnly reports whether the access list names this user and nobody else.
func (d DirAccess) OwnerOnly(self string) bool {
	return d.Protected && len(d.Trustees) == 1 && d.Trustees[0] == self
}

// CurrentUserSID returns this process's user SID in string form.
func CurrentUserSID() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("looking up the current user: %w", err)
	}
	return user.User.Sid.String(), nil
}

// ReadDirAccess reads back a directory's access list.
//
// Its reason for existing is that EnsureOwnerOnlyDir is unverifiable from the
// machine this is written on: it is syscalls all the way down, so the only
// honest check is to set the list on a real Windows host and read it back.
// That is what the test does, and it needs no desktop session, which is the
// one thing CI can give us here.
func ReadDirAccess(path string) (DirAccess, error) {
	descriptor, err := windows.GetNamedSecurityInfo(
		path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return DirAccess{}, fmt.Errorf("reading the access list on %s: %w", path, err)
	}

	acl, _, err := descriptor.DACL()
	if err != nil {
		return DirAccess{}, fmt.Errorf("reading the access list on %s: %w", path, err)
	}

	control, _, err := descriptor.Control()
	if err != nil {
		return DirAccess{}, fmt.Errorf("reading the control bits on %s: %w", path, err)
	}

	access := DirAccess{
		Protected: control&windows.SE_DACL_PROTECTED != 0,
	}
	if acl == nil {
		// A nil DACL is not an empty one: it grants everyone everything.
		return access, nil
	}

	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			return DirAccess{}, fmt.Errorf("reading entry %d of %s: %w", i, path, err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		access.Trustees = append(access.Trustees, sid.String())
	}
	return access, nil
}
