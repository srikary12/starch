//go:build !windows

package server

import (
	"os"
	"path/filepath"
	"testing"
)

// The mode assertions live here rather than in listener_test.go because they
// are only meaningful where POSIX modes are. On Windows os.Stat synthesizes a
// mode out of FILE_ATTRIBUTE_READONLY — every directory reads 0777 and every
// socket 0666, no matter what — so running these there would not be a weaker
// check, it would be a check of nothing. listener_windows_test.go covers what
// that platform can actually promise.

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
