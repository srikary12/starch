package daemon

import "os"

// systemEnvironment is the floor the daemon needs from the operating system
// regardless of who started it.
//
// SystemRoot is not optional on Windows the way it looks: a process started
// without it can fail inside Winsock and the CRT, in ways that surface far from
// the cause. PATH is derived from it rather than inherited for the same reason
// the Unix side pins one — the daemon executes nothing, so the only thing an
// inherited PATH could change is which DLL some dependency loads, which is not
// a decision a user's shell should get to make for a process holding an API
// key.
func systemEnvironment() []string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		// A machine this is untrue on is one where nothing else would work
		// either, but an empty PATH would be a stranger failure than a wrong
		// one.
		root = `C:\Windows`
	}
	return []string{
		"SystemRoot=" + root,
		"PATH=" + root + `\system32;` + root,
	}
}

// inheritedNames are the variables passed through from the user's session.
//
// APPDATA is the one this list was originally missing, and the omission was
// invisible on the machine the code was written on. os.UserConfigDir reads it
// and nothing else on Windows, so the daemon started, failed to resolve its own
// support directory, and exited -- six times, through every backoff the
// supervisor had -- while the shell, which had the full environment, printed a
// perfectly correct settings path a line earlier. LOCALAPPDATA is where the
// socket goes (paths.go picks Local over Roaming deliberately: a roaming
// profile must not sync a socket to a file server), and USERPROFILE is what
// os.UserHomeDir falls back to.
func inheritedNames() []string {
	return []string{"APPDATA", "LOCALAPPDATA", "USERPROFILE", "TEMP", "TMP"}
}
