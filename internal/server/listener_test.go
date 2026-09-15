package server

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListenPermissions(t *testing.T) {
	path := tempSocket(t)
	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	// The socket itself must be user-only. net.Listen applies the umask, so
	// this asserts the explicit chmod actually happened.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %04o, want 0600", perm)
	}

	// The containing directory must be traversable only by the owner; that is
	// what actually closes the umask race on the socket inode.
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("directory mode = %04o, want 0700", perm)
	}
}

func TestListenTightensLooseDirectory(t *testing.T) {
	path := tempSocket(t)
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("directory mode = %04o, want Listen to tighten it to 0700", perm)
	}
}

func TestListenRemovesStaleSocket(t *testing.T) {
	path := tempSocket(t)

	// Bind and close without unlinking, the way a SIGKILLed daemon leaves things.
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("seeding socket: %v", err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stale socket was not left behind: %v", err)
	}

	got, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	got.Close()
}

func TestListenRefusesLiveSocket(t *testing.T) {
	path := tempSocket(t)
	first, err := Listen(path)
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	defer first.Close()

	_, err = Listen(path)
	if err == nil {
		t.Fatal("second Listen succeeded, want it to refuse a live socket")
	}
	if !strings.Contains(err.Error(), "already listening") {
		t.Errorf("error = %v, want it to mention another listener", err)
	}
}

func TestListenRefusesNonSocketFile(t *testing.T) {
	path := tempSocket(t)
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Listen(path); err == nil {
		t.Fatal("Listen clobbered a regular file, want an error")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("Listen removed a regular file it should have left alone: %v", err)
	}
}

func TestListenRejectsOverlongPath(t *testing.T) {
	path := filepath.Join(os.TempDir(), strings.Repeat("x", maxSocketPathLen), "d.sock")

	_, err := Listen(path)
	if err == nil {
		t.Fatal("Listen accepted an overlong path")
	}
	// The point of the pre-check is a readable message instead of bind's
	// "invalid argument".
	if !strings.Contains(err.Error(), "limit is") {
		t.Errorf("error = %v, want it to explain the length limit", err)
	}
}

func TestListenUnlinksOnClose(t *testing.T) {
	path := tempSocket(t)
	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("socket still present after Close (err = %v)", err)
	}
}

// A POSIX mode does nothing on Windows — syscall.Chmod there only toggles
// FILE_ATTRIBUTE_READONLY — and it returns nil either way. Calling it and
// treating that nil as success would have the daemon report a socket secured
// that it never touched, which is worse than not trying: a false guarantee is
// not a weaker guarantee.
func TestRestrictToOwner(t *testing.T) {
	tests := []struct {
		goos      string
		wantCalls int
	}{
		{"darwin", 1},
		{"linux", 1},
		// Not "chmod fails on Windows" — it succeeds, having done nothing.
		{"windows", 0},
	}

	for _, tc := range tests {
		t.Run(tc.goos, func(t *testing.T) {
			var calls int
			var gotPath string
			var gotMode fs.FileMode
			chmod := func(path string, mode fs.FileMode) error {
				calls++
				gotPath, gotMode = path, mode
				return nil
			}

			if err := restrictToOwner(tc.goos, "/tmp/x/d.sock", 0o600, chmod); err != nil {
				t.Fatalf("restrictToOwner: %v", err)
			}
			if calls != tc.wantCalls {
				t.Fatalf("chmod called %d times, want %d", calls, tc.wantCalls)
			}
			if tc.wantCalls > 0 {
				if gotPath != "/tmp/x/d.sock" {
					t.Errorf("chmod path = %q", gotPath)
				}
				if gotMode != 0o600 {
					t.Errorf("chmod mode = %o, want 600", gotMode)
				}
			}
		})
	}
}

// A chmod that genuinely fails must still fail the bind. The socket would
// otherwise be left readable by every other account on the machine.
func TestRestrictToOwnerReportsARealFailure(t *testing.T) {
	boom := errors.New("read-only filesystem")
	chmod := func(string, fs.FileMode) error { return boom }

	if err := restrictToOwner("linux", "/tmp/x/d.sock", 0o600, chmod); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap %v", err, boom)
	}
}
