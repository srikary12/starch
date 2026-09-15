package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

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
// Every branch is compiled on every platform and the goos is a parameter, so
// the Linux layout is checked by the test suite on a Mac rather than only by
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
	if goos == "linux" {
		// The spec guarantees an absolute path, user-owned, mode 0700 — which
		// is the directory Listen would otherwise have to create itself. It is
		// genuinely absent in a few real cases (an ssh session with no systemd
		// user instance, a bare container), and the spec's own advice there is
		// to fall back rather than invent a location, so the config directory
		// stands in. Checked for absoluteness because a relative value would
		// put the socket wherever the daemon happened to be started from.
		if dir := getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(dir) {
			return filepath.Join(dir, brand.Slug), nil
		}
	}
	return supportDir(goos, userConfigDir)
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
