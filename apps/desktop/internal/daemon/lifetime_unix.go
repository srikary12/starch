//go:build !windows

package daemon

import (
	"os/exec"
	"syscall"
)

// stopGracefully asks the daemon to exit in the way that lets it clean up.
//
// SIGTERM rather than the default SIGKILL: the daemon unlinks its socket on the
// way out, and a killed one leaves a file the next start has to probe and
// clear. WaitDelay in the caller is the backstop for one that ignores it.
func stopGracefully(cmd *exec.Cmd) error {
	return cmd.Process.Signal(syscall.SIGTERM)
}

// confine ties the daemon's lifetime to the shell's at the OS level.
//
// Nothing to do here. An orphaned child is reparented on Unix, so the daemon's
// own parent-pid watch notices within two seconds, and the shell sends SIGTERM
// on the way out anyway.
func confine(*exec.Cmd) error { return nil }

// prepareSocketDir restricts the directory the socket will be created in.
//
// Nothing to do here either: the daemon chmods the directory and the socket
// itself, which it can do because POSIX modes mean something on this platform.
func prepareSocketDir(string) error { return nil }
