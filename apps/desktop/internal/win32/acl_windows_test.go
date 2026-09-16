package win32

import (
	"os"
	"path/filepath"
	"testing"
)

// These are the only honest check on this code. EnsureOwnerOnlyDir is syscalls
// all the way down, so it cannot be tested on the machine it was written on —
// it is written on a Mac. Reading the list back on a real Windows host is the
// verification, and it needs no desktop session, which is the one thing CI can
// offer here.

func TestEnsureOwnerOnlyDirNamesOnlyThisUser(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "starch")

	if err := EnsureOwnerOnlyDir(dir); err != nil {
		t.Fatalf("EnsureOwnerOnlyDir: %v", err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("Stat(%s): %v", dir, err)
	}

	self, err := CurrentUserSID()
	if err != nil {
		t.Fatalf("CurrentUserSID: %v", err)
	}
	access, err := ReadDirAccess(dir)
	if err != nil {
		t.Fatalf("ReadDirAccess: %v", err)
	}
	if !access.OwnerOnly(self) {
		t.Fatalf("access = %+v, want only %s and no inherited entries", access, self)
	}
}

// The number of entries is not the number of trustees, and the two paths into
// EnsureOwnerOnlyDir genuinely differ on it: CreateDirectory stores the list
// verbatim, while applying it to an existing directory makes Windows split one
// inheritable generic-rights entry into an effective entry and an inherit-only
// one. Both name this user alone, which is the property that matters and the
// property OwnerOnly asks about.
//
// This pins the split down rather than leaving it as a comment, because the
// first version of this code asserted the entry count and failed on a directory
// that was correctly locked down.
func TestBothPathsNameOneTrustee(t *testing.T) {
	self, err := CurrentUserSID()
	if err != nil {
		t.Fatalf("CurrentUserSID: %v", err)
	}

	created := filepath.Join(t.TempDir(), "starch")
	if err := EnsureOwnerOnlyDir(created); err != nil {
		t.Fatalf("EnsureOwnerOnlyDir: %v", err)
	}

	tightened := filepath.Join(t.TempDir(), "starch")
	if err := os.MkdirAll(tightened, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := EnsureOwnerOnlyDir(tightened); err != nil {
		t.Fatalf("EnsureOwnerOnlyDir: %v", err)
	}

	for _, dir := range []string{created, tightened} {
		access, err := ReadDirAccess(dir)
		if err != nil {
			t.Fatalf("ReadDirAccess(%s): %v", dir, err)
		}
		if !access.OwnerOnly(self) {
			t.Errorf("%s: access = %+v, want only %s", dir, access, self)
		}
		if access.Entries < 1 {
			t.Errorf("%s: no entries at all, which grants nothing to anyone", dir)
		}
	}
}

// The case that matters more: a directory left by an earlier build, or made by
// hand, carrying whatever its parent hands down. Trusting what is already there
// is how a socket ends up reachable by a group somebody added years ago, so
// this tightens unconditionally — the same reasoning as the daemon's
// unconditional chmod on the other two platforms.
func TestEnsureOwnerOnlyDirTightensWhatItFinds(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "starch")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	inherited, err := ReadDirAccess(dir)
	if err != nil {
		t.Fatalf("ReadDirAccess: %v", err)
	}
	if inherited.Protected {
		t.Skip("this runner's temp directory already blocks inheritance, so there is nothing to tighten")
	}

	if err := EnsureOwnerOnlyDir(dir); err != nil {
		t.Fatalf("EnsureOwnerOnlyDir: %v", err)
	}

	self, err := CurrentUserSID()
	if err != nil {
		t.Fatalf("CurrentUserSID: %v", err)
	}
	access, err := ReadDirAccess(dir)
	if err != nil {
		t.Fatalf("ReadDirAccess: %v", err)
	}
	if !access.OwnerOnly(self) {
		t.Fatalf("access = %+v after tightening, want only %s; it was %+v before",
			access, self, inherited)
	}
}

// Calling it twice must be as good as calling it once. The shell calls it
// before every spawn, so this is the common path rather than an edge case.
func TestEnsureOwnerOnlyDirIsRepeatable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "starch")

	for i := range 3 {
		if err := EnsureOwnerOnlyDir(dir); err != nil {
			t.Fatalf("EnsureOwnerOnlyDir, call %d: %v", i+1, err)
		}
	}

	self, err := CurrentUserSID()
	if err != nil {
		t.Fatalf("CurrentUserSID: %v", err)
	}
	access, err := ReadDirAccess(dir)
	if err != nil {
		t.Fatalf("ReadDirAccess: %v", err)
	}
	if !access.OwnerOnly(self) {
		t.Fatalf("access = %+v, want only %s", access, self)
	}
}
