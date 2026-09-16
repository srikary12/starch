package daemon

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// The daemon resolves its own support and runtime directories from the
// environment the supervisor hands it, and it is the only thing it gets. If a
// variable that resolution depends on is missing, starchd does not misbehave —
// it fails to start, retries through every backoff, and gives up, while the
// shell that spawned it reports paths of its own quite correctly because it
// still has the full environment.
//
// That is what happened on Windows: os.UserConfigDir reads %AppData% and
// nothing else there, and %AppData% was not on the list. So this names, per
// platform, what internal/config/paths.go actually consults. It duplicates that
// knowledge deliberately — the duplication is what turns trimming the list into
// a failing test rather than a broken daemon.
func TestTheDaemonGetsWhatItNeedsToFindItsOwnDirectories(t *testing.T) {
	required := map[string][]string{
		// os.UserConfigDir, then the socket directory.
		"windows": {"APPDATA", "LOCALAPPDATA"},
		"darwin":  {"HOME"},
		"linux":   {"HOME"},
	}[runtime.GOOS]
	if len(required) == 0 {
		t.Skipf("no directory requirements recorded for %s", runtime.GOOS)
	}

	for _, name := range required {
		t.Setenv(name, `X:\set-by-the-test`)
	}

	sup, err := New(Options{Executable: "starchd", SocketPath: "/tmp/st/d.sock"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sup.opts.IdleTimeout = 30 * time.Second
	env := sup.environment("token")

	for _, name := range required {
		if !hasVar(env, name) {
			t.Errorf("%s is not passed to the daemon; it cannot resolve its own "+
				"directories without it and will fail to start", name)
		}
	}
}

// A narrow environment is a security property, not a tidiness one: it is one
// less way for a stray variable to change the behaviour of a process holding an
// API key. So the list is an allowlist, and this checks it stayed one.
func TestTheDaemonInheritsNothingElse(t *testing.T) {
	t.Setenv("LD_PRELOAD", "/tmp/evil.so")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	t.Setenv("SSLKEYLOGFILE", "/tmp/keys.log")

	sup, err := New(Options{Executable: "starchd", SocketPath: "/tmp/st/d.sock"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	env := sup.environment("token")

	for _, name := range []string{"LD_PRELOAD", "HTTPS_PROXY", "SSLKEYLOGFILE"} {
		if hasVar(env, name) {
			t.Errorf("%s reached the daemon; the environment is meant to be an allowlist", name)
		}
	}
}

func hasVar(env []string, name string) bool {
	for _, entry := range env {
		if strings.HasPrefix(entry, name+"=") {
			return true
		}
	}
	return false
}
