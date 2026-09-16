// The Windows Credential Manager implementation.
//
// Generic credentials, one per provider, encrypted at rest by DPAPI under the
// logged-on user's key and readable by nobody else. Credential Manager is part
// of the OS, so unlike the Linux side there is no "no keyring is running" case
// to explain and no service to install.
//
// The functions live in advapi32 and are reached through lazy DLL loading, so
// this stays pure Go with CGO_ENABLED=0 like the rest of the shell.
package secret

import (
	"errors"
	"fmt"
	"unsafe"

	"github.com/srikary12/starch/internal/brand"
	"golang.org/x/sys/windows"
)

var (
	advapi32        = windows.NewLazySystemDLL("advapi32.dll")
	procCredWriteW  = advapi32.NewProc("CredWriteW")
	procCredReadW   = advapi32.NewProc("CredReadW")
	procCredDeleteW = advapi32.NewProc("CredDeleteW")
	procCredFree    = advapi32.NewProc("CredFree")
)

const (
	credTypeGeneric = 1
	// Local machine rather than enterprise: an API key should not follow the
	// user onto every machine they log into, which is what roaming persistence
	// would do.
	credPersistLocalMachine = 2
	// CRED_MAX_CREDENTIAL_BLOB_SIZE. Far above any API key, but a value over it
	// fails inside the API with a message about nothing in particular.
	credMaxBlobSize = 2560
)

// credentialW is CREDENTIALW. Field order is the wire layout and must not be
// rearranged.
type credentialW struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

// Store is a handle on Credential Manager.
//
// Empty, deliberately: there is no session to open and nothing to keep alive,
// unlike the Secret Service. It exists so both platforms present the same
// surface to the code above.
type Store struct{}

// Open returns a store. It cannot fail on Windows — Credential Manager is part
// of the OS — and returns an error only so the signature matches the other
// platform's, where the keyring genuinely may not be running.
func Open() (*Store, error) { return &Store{}, nil }

// Close releases nothing, for the same reason Open cannot fail.
func (s *Store) Close() error { return nil }

// target is the name this credential appears under in Credential Manager. It
// is what the user sees in the Windows control panel, so it names the product
// rather than just the account.
func target(account string) string { return brand.Name + "/" + account }

// Set stores or replaces the secret for account.
func (s *Store) Set(account, value string) error {
	blob := []byte(value)
	if len(blob) > credMaxBlobSize {
		return fmt.Errorf("that key is %d bytes; Credential Manager accepts %d",
			len(blob), credMaxBlobSize)
	}

	name, err := windows.UTF16PtrFromString(target(account))
	if err != nil {
		return fmt.Errorf("%s is not a usable credential name: %w", target(account), err)
	}
	// The user name field is shown in the control panel next to the entry. It
	// is not a credential of ours, so it names what the entry is for.
	user, err := windows.UTF16PtrFromString(account)
	if err != nil {
		return fmt.Errorf("%s is not a usable account name: %w", account, err)
	}

	credential := credentialW{
		Type:               credTypeGeneric,
		TargetName:         name,
		CredentialBlobSize: uint32(len(blob)),
		Persist:            credPersistLocalMachine,
		UserName:           user,
	}
	if len(blob) > 0 {
		credential.CredentialBlob = &blob[0]
	}

	// CredWriteW overwrites an existing credential of the same type and target,
	// so re-entering a key replaces the entry rather than adding another.
	result, _, err := procCredWriteW.Call(uintptr(unsafe.Pointer(&credential)), 0)
	if result == 0 {
		return fmt.Errorf("storing the key in Credential Manager: %w", err)
	}
	return nil
}

// Get returns the secret for account. The boolean reports whether one exists,
// which is different from an empty one.
func (s *Store) Get(account string) (string, bool, error) {
	credential, free, err := read(account)
	if err != nil || credential == nil {
		return "", false, err
	}
	defer free()

	if credential.CredentialBlobSize == 0 || credential.CredentialBlob == nil {
		return "", true, nil
	}
	blob := unsafe.Slice(credential.CredentialBlob, credential.CredentialBlobSize)
	return string(blob), true, nil
}

// Has reports whether a secret exists for account.
//
// Implemented by reading it, which on the Linux side would be the wrong thing
// to do — reading there can summon a password dialog, so that implementation
// searches instead. Windows has no such prompt: Credential Manager decrypts
// with the logged-on user's key and never asks anything. There is also no API
// that tests existence without returning the blob, so reading is both harmless
// and the only option.
func (s *Store) Has(account string) (bool, error) {
	credential, free, err := read(account)
	if err != nil {
		return false, err
	}
	if credential == nil {
		return false, nil
	}
	free()
	return true, nil
}

// Delete removes the secret for account. Absent is not an error.
func (s *Store) Delete(account string) error {
	name, err := windows.UTF16PtrFromString(target(account))
	if err != nil {
		return fmt.Errorf("%s is not a usable credential name: %w", target(account), err)
	}

	result, _, err := procCredDeleteW.Call(
		uintptr(unsafe.Pointer(name)), credTypeGeneric, 0,
	)
	if result == 0 {
		if errors.Is(err, windows.ERROR_NOT_FOUND) {
			// How a settings command clears a key whose state it cannot know.
			return nil
		}
		return fmt.Errorf("removing the key from Credential Manager: %w", err)
	}
	return nil
}

// read fetches one credential, returning nil when there is none.
//
// The caller must call free once it has finished with the returned pointer: the
// API allocates the buffer and CredFree releases it.
func read(account string) (*credentialW, func(), error) {
	name, err := windows.UTF16PtrFromString(target(account))
	if err != nil {
		return nil, nil, fmt.Errorf("%s is not a usable credential name: %w", target(account), err)
	}

	var raw *credentialW
	result, _, err := procCredReadW.Call(
		uintptr(unsafe.Pointer(name)),
		credTypeGeneric,
		0,
		uintptr(unsafe.Pointer(&raw)),
	)
	if result == 0 {
		if errors.Is(err, windows.ERROR_NOT_FOUND) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("reading the key from Credential Manager: %w", err)
	}
	return raw, func() { procCredFree.Call(uintptr(unsafe.Pointer(raw))) }, nil
}
