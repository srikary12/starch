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
