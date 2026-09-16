//go:build !windows

package daemon

// systemEnvironment is the floor the daemon needs from the operating system
// regardless of who started it.
//
// A fixed PATH rather than the user's: the daemon executes nothing, so the only
// thing inheriting a PATH could change is which library or helper some
// dependency finds, and that is not a decision a shell alias should get to make
// for a process holding an API key.
func systemEnvironment() []string {
	return []string{"PATH=/usr/bin:/bin"}
}

// inheritedNames are the variables passed through from the user's session.
//
// These are exactly what internal/config/paths.go consults to find the support
// and runtime directories: HOME and XDG_CONFIG_HOME via os.UserConfigDir, and
// XDG_RUNTIME_DIR for the socket. Nothing else is passed, so nothing else can
// change the daemon's behaviour.
func inheritedNames() []string {
	return []string{"HOME", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR"}
}
