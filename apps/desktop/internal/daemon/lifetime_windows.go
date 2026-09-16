package daemon

import (
	"os/exec"
	"path/filepath"

	"github.com/srikary12/starch/apps/desktop/internal/win32"
)

// stopGracefully asks the daemon to exit.
//
// There is no graceful stop on Windows to ask for: Process.Signal there
// implements Kill and returns an error for everything else, so a SIGTERM would
// fail, stall for WaitDelay, and end in the same kill with two wasted seconds
// in front of it. Going straight to it is the honest version.
//
// The cost is the socket file, which the daemon would otherwise have unlinked.
// That is already handled: the next start probes it, finds nothing listening,
// and removes it.
func stopGracefully(cmd *exec.Cmd) error {
	return cmd.Process.Kill()
}

// confine puts the daemon in a job object that dies with this process.
//
// This is load-bearing rather than belt-and-braces. Both of the daemon's usual
// backstops are absent on Windows — there is no graceful signal, and Windows
// does not reparent orphans so its parent-pid watch can never fire — which
// would leave the 30-second idle timeout as the only thing reclaiming a daemon
// holding an API key after a shell crash. The job object reclaims it
// immediately, including when the shell is killed outright.
func confine(cmd *exec.Cmd) error {
	return win32.ConfineToShellLifetime(cmd.Process.Pid)
}

// prepareSocketDir creates the socket's directory restricted to this user.
//
// On Windows this is the shell's job and nobody else's. POSIX modes do nothing
// there, so the daemon cannot enforce its own 0600/0700 and does not pretend
// to; §5 of api/README.md states this as an obligation on a Windows shell, and
// this is where we meet it.
//
// A failure here is fatal to the spawn on purpose. Starting a daemon that will
// hold an API key on a socket we could not restrict is the one outcome worth
// refusing outright — a rewrite that does not happen is recoverable, and a key
// readable by every account on the machine is not.
func prepareSocketDir(socketPath string) error {
	return win32.EnsureOwnerOnlyDir(filepath.Dir(socketPath))
}
