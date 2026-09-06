package server

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// maxSocketPathLen is the portable ceiling on sun_path. Darwin allows 104
// bytes and Linux 108, both including the NUL terminator. Exceeding it makes
// bind fail with a bare "invalid argument", so check up front and say why.
const maxSocketPathLen = 103

// Listen binds a Unix domain socket at path, restricted to the current user.
//
// The parent directory is created with mode 0700 and the socket is chmodded to
// 0600. Both matter: net.Listen applies the process umask when it creates the
// socket inode, so there is a brief window where the socket is more permissive
// than intended. Binding inside a 0700 directory closes that window, because
// traversal is denied to everyone else regardless of the socket's own mode.
//
// A stale socket left behind by a crashed daemon is removed, but only after
// probing it: if something is still listening, Listen fails rather than
// stealing the address from a live daemon.
func Listen(path string) (net.Listener, error) {
	if len(path) > maxSocketPathLen {
		return nil, fmt.Errorf("socket path is %d bytes, limit is %d: %s", len(path), maxSocketPathLen, path)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}
	// MkdirAll is a no-op on an existing directory, including one created with
	// looser permissions by an older build, so tighten it unconditionally.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("securing %s: %w", dir, err)
	}

	if err := clearStaleSocket(path); err != nil {
		return nil, err
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("binding %s: %w", path, err)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("securing %s: %w", path, err)
	}

	return ln, nil
}

// clearStaleSocket removes path if it is a socket with nothing listening.
func clearStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspecting %s: %w", path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket, refusing to remove it", path)
	}

	conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err == nil {
		conn.Close()
		return fmt.Errorf("another daemon is already listening on %s", path)
	}

	if err := os.Remove(path); err != nil {
		return fmt.Errorf("removing stale socket %s: %w", path, err)
	}
	return nil
}
