package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/srikary12/starch/internal/brand"
)

// Two directories, not one, because the two kinds of file want opposite
// lifetimes.
//
// presets.json is the user's own work: it belongs with their configuration,
// where a backup picks it up and a reboot does not touch it. The socket is the
// reverse — meaningless the moment the process holding it dies. A stale one
// left in a config directory survives reboots, gets swept up by a dotfile
// manager, and on a network home directory cannot be bound at all.
//
// macOS collapses both into ~/Library/Application Support, which is why one
// directory was enough until now. Linux does not, and the XDG base directory
// specification is explicit that sockets belong in $XDG_RUNTIME_DIR — a 0700
// tmpfs the session manager creates at login and wipes at logout, which is
// exactly the lifetime a handshake socket wants.
//
// Windows splits them for a different reason. os.UserConfigDir there is
// %AppData%, which on a domain account roams: a socket file synced to a file
// server and back at next logon is a stale rendezvous the daemon then has to
// probe and clear, and on a slow link it is worse than that. %LocalAppData%
// stays on the machine, which is what a socket wants and what presets.json
// does not.
//
// Every branch is compiled on every platform and the goos is a parameter, so
// both layouts are checked by the test suite on a Mac rather than only by
// whoever next runs CI. That is the reason these take arguments at all.

// SupportDir returns the per-user directory holding presets.json.
func SupportDir() (string, error) {
	return supportDir(runtime.GOOS, os.UserConfigDir)
}

// RuntimeDir returns the per-user directory holding the socket.
func RuntimeDir() (string, error) {
	return runtimeDir(runtime.GOOS, os.Getenv, os.UserConfigDir)
}

// DefaultSocketPath returns the socket path used when the shell does not
// override it with EnvSocket. Shells are expected to set it explicitly; this
// is what `starchd` run by hand uses.
func DefaultSocketPath() (string, error) {
	dir, err := RuntimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, brand.SocketName), nil
}

func supportDir(goos string, userConfigDir func() (string, error)) (string, error) {
	base, err := userConfigDir()
	if err != nil {
		return "", fmt.Errorf("locating user config dir: %w", err)
	}
	return filepath.Join(base, supportDirName(goos)), nil
}

func runtimeDir(goos string, getenv func(string) string, userConfigDir func() (string, error)) (string, error) {
	// Both branches check the variable is absolute before trusting it: a
	// relative value would put the socket wherever the daemon happened to be
	// started from, and both are ordinary environment variables that anything
	// in the session can have mangled.
	//
	// isAbs rather than filepath.IsAbs because that one answers for the host:
	// on a Mac it calls C:\Users\me relative, which would make the Windows
	// branch untestable from the machine it is written on.
	switch goos {
	case "linux":
		// The spec guarantees an absolute path, user-owned, mode 0700 — which
		// is the directory Listen would otherwise have to create itself. It is
		// genuinely absent in a few real cases (an ssh session with no systemd
		// user instance, a bare container), and the spec's own advice there is
		// to fall back rather than invent a location, so the config directory
		// stands in.
		if dir := getenv("XDG_RUNTIME_DIR"); isAbs(goos, dir) {
			return filepath.Join(dir, brand.Slug), nil
		}
	case "windows":
		// Local rather than roaming, so the socket cannot be synced to a file
		// server and restored at next logon. Absent only on a profile that is
		// already broken, where the roaming directory is the honest fallback.
		if dir := getenv("LOCALAPPDATA"); isAbs(goos, dir) {
			return filepath.Join(dir, brand.SupportDirName), nil
		}
	}
	return supportDir(goos, userConfigDir)
}

// isAbs reports whether p is absolute on goos, which is not the same question
// filepath.IsAbs answers — that one answers for the host.
//
// Only enough of the rule to judge an environment variable: a drive-letter root
// or a UNC path on Windows, a leading slash everywhere else. Note that what
// gets joined onto it afterwards still uses the host's separator, so a Windows
// path built on a Mac is only right about which directory was chosen, not about
// how it is spelled.
func isAbs(goos, p string) bool {
	if goos != "windows" {
		return strings.HasPrefix(p, "/")
	}
	if strings.HasPrefix(p, `\\`) {
		return true // UNC, \\server\share
	}
	// C:\ or C:/, a drive letter followed by a colon and a separator. A bare
	// "C:foo" is relative to that drive's current directory, not a root.
	return len(p) >= 3 &&
		((p[0] >= 'a' && p[0] <= 'z') || (p[0] >= 'A' && p[0] <= 'Z')) &&
		p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}

// supportDirName is the directory name to create under the per-user config
// root. Capitalised on macOS and Windows, where it sits among other
// capitalised application names; lowercase on Linux, where everything in
// ~/.config is lowercase and a capitalised entry reads as a bug.
func supportDirName(goos string) string {
	if goos == "linux" {
		return brand.Slug
	}
	return brand.SupportDirName
}
