package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The supervisor hands the daemon a deliberately narrow environment, so a test
// cannot signal a helper process through one. The executable's own name is the
// channel instead: TestMain re-executes this binary as a stand-in daemon when
// it is invoked under a "fake-" name, and each name is a different behaviour.
func TestMain(m *testing.M) {
	// Trimmed because Windows will not execute a file without one of the
	// extensions in %PATHEXT%, so the stand-in has to be called fake-crash.exe
	// there while still answering to fake-crash here.
	switch strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") {
	case "fake-healthy":
		fakeDaemon(false)
	case "fake-imposter":
		fakeDaemon(true)
	case "fake-crash":
		// The daemon prints its reason bare on stderr before giving up, and
		// the shell is expected to relay that rather than "exited with 1".
		fmt.Fprintln(os.Stderr, "starchd: another daemon is already listening on /run/user/1000/starch/starchd.sock")
		os.Exit(1)
	default:
		os.Exit(m.Run())
	}
}

// fakeDaemon serves just enough of the contract for the supervisor: /healthz,
// the handshake token, and a socket unlinked on SIGTERM. With imposter set it
// reports someone else's pid, which the shell must refuse.
func fakeDaemon(imposter bool) {
	socket := os.Getenv("STARCH_SOCKET")
	token := os.Getenv("STARCH_TOKEN")
	if socket == "" || token == "" {
		fmt.Fprintln(os.Stderr, "starchd: STARCH_SOCKET and STARCH_TOKEN are required")
		os.Exit(2)
	}

	ln, err := net.Listen("unix", socket)
	if err != nil {
		fmt.Fprintln(os.Stderr, "starchd: "+err.Error())
		os.Exit(1)
	}

	pid := os.Getpid()
	if imposter {
		pid = 999999
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"Bad token."}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "name": "Starch", "version": "0.0.0-test",
			"api_version": "v1", "pid": pid, "uptime_ms": 1,
		})
	})}

	// A structured line, the way the real daemon logs when all is well. The
	// shell must not mistake it for a failure report.
	fmt.Fprintln(os.Stderr, "time=now level=INFO msg=listening socket="+socket)

	go func() { _ = srv.Serve(ln) }()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	<-stop
	_ = srv.Close()
	_ = os.Remove(socket)
	os.Exit(0)
}

// fake links this test binary under name, so running it exercises the matching
// branch of TestMain.
func fake(t *testing.T, name string) (executable, socket string) {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	// Short by construction: t.TempDir() builds a path from the test name and
	// routinely blows past the 104-byte sun_path limit.
	dir, err := os.MkdirTemp("", "st")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	executable = filepath.Join(dir, name+exeSuffix)
	if err := standIn(self, executable); err != nil {
		t.Fatalf("installing the stand-in daemon: %v", err)
	}
	return executable, filepath.Join(dir, "d.sock")
}

// exeSuffix is what Windows insists on before it will execute a file at all.
var exeSuffix = func() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}()

// assertDaemonIsGone checks that shutting the supervisor down really took the
// daemon with it. One left holding an API key is a bug, not an inconvenience.
//
// How that looks differs by platform, and the difference is the design rather
// than a wrinkle. On Unix the daemon is signalled, so it unlinks its own socket
// on the way out and the file's absence is the evidence. On Windows there is no
// signal to send — os/exec_windows.go implements Process.Signal for Kill alone
// — so the daemon is always terminated and the socket file always survives it.
// Asserting the file is gone there would be asserting that a design decision
// taken deliberately in 4bd6d15 had not been taken.
//
// What must be true on both is that nothing is still serving on it, which is
// also the condition clearStaleSocket uses to decide it may clear the file at
// the next start.
func assertDaemonIsGone(t *testing.T, socket string) {
	t.Helper()

	conn, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err == nil {
		conn.Close()
		t.Error("something is still listening on the socket after shutdown")
	}

	if runtime.GOOS == "windows" {
		return
	}
	if _, err := os.Stat(socket); err == nil {
		t.Error("the socket outlived the daemon, so the next start has to clear it")
	}
}

// standIn puts a runnable copy of this test binary at path.
//
// A symlink everywhere would be cheaper, but creating one on Windows needs
// SeCreateSymbolicLinkPrivilege — held by an elevated shell or a machine in
// developer mode, and by nothing else — so it is exactly the sort of thing that
// works for whoever wrote it and fails for everyone else. A copy needs no
// privilege and behaves identically once exec'd, which is all these tests want
// from it.
func standIn(self, path string) error {
	if runtime.GOOS != "windows" {
		return os.Symlink(self, path)
	}

	source, err := os.Open(self)
	if err != nil {
		return err
	}
	defer source.Close()

	destination, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return err
	}
	if _, err := io.Copy(destination, source); err != nil {
		destination.Close()
		return err
	}
	return destination.Close()
}

// watcher collects status changes so a test can wait for one.
type watcher struct {
	mu      sync.Mutex
	seen    []Status
	changed chan struct{}
}

func newWatcher() *watcher { return &watcher{changed: make(chan struct{}, 64)} }

func (w *watcher) record(s Status) {
	w.mu.Lock()
	w.seen = append(w.seen, s)
	w.mu.Unlock()
	select {
	case w.changed <- struct{}{}:
	default:
	}
}

func (w *watcher) states() []State {
	w.mu.Lock()
	defer w.mu.Unlock()
	states := make([]State, len(w.seen))
	for i, s := range w.seen {
		states[i] = s.State
	}
	return states
}

// await waits for a status matching want, and returns it.
func (w *watcher) await(t *testing.T, what string, want func(Status) bool) Status {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		w.mu.Lock()
		for _, s := range w.seen {
			if want(s) {
				w.mu.Unlock()
				return s
			}
		}
		w.mu.Unlock()

		select {
		case <-w.changed:
		case <-deadline:
			t.Fatalf("timed out waiting for %s; saw %v", what, w.states())
		}
	}
}

func run(t *testing.T, opts Options) (*Supervisor, *watcher, context.CancelFunc, *sync.WaitGroup) {
	t.Helper()

	w := newWatcher()
	opts.OnStatus = w.record
	opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if opts.HeartbeatInterval == 0 {
		opts.HeartbeatInterval = 100 * time.Millisecond
	}

	sup, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := sup.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	t.Cleanup(func() { cancel(); wg.Wait() })
	return sup, w, cancel, &wg
}

func TestSupervisorReachesRunning(t *testing.T) {
	executable, socket := fake(t, "fake-healthy")
	sup, w, cancel, wg := run(t, Options{Executable: executable, SocketPath: socket})

	status := w.await(t, "a healthy daemon", func(s Status) bool { return s.State == StateRunning })
	if status.Health.Version != "0.0.0-test" {
		t.Errorf("version = %q", status.Health.Version)
	}
	if !status.Healthy() {
		t.Error("Healthy() = false for a running daemon")
	}
	if sup.Client() == nil {
		t.Error("Client() = nil while running")
	}

	// Quitting must take the daemon with it: one left holding an API key is a
	// bug, not an inconvenience.
	cancel()
	wg.Wait()

	assertDaemonIsGone(t, socket)
	if sup.Status().State != StateStopped {
		t.Errorf("status after shutdown = %v, want stopped", sup.Status().State)
	}
}

// A daemon that refuses to start says exactly why. That sentence is what the
// user should see, not "exited with status 1".
func TestFailureReportsTheDaemonsOwnReason(t *testing.T) {
	executable, socket := fake(t, "fake-crash")
	_, w, _, _ := run(t, Options{Executable: executable, SocketPath: socket})

	status := w.await(t, "a failure", func(s Status) bool { return s.State == StateFailed })
	if !strings.Contains(status.Detail, "already listening") {
		t.Errorf("detail = %q, want the daemon's own words", status.Detail)
	}
	if strings.Contains(status.Detail, "level=") {
		t.Errorf("detail = %q, want the bare error line rather than a routine log line", status.Detail)
	}
}

// A crash must be retried, or one transient failure ends the session.
func TestACrashedDaemonIsRestarted(t *testing.T) {
	executable, socket := fake(t, "fake-crash")
	_, w, _, _ := run(t, Options{Executable: executable, SocketPath: socket})

	w.await(t, "a failure", func(s Status) bool { return s.State == StateFailed })

	starts := 0
	deadline := time.After(15 * time.Second)
	for starts < 2 {
		select {
		case <-w.changed:
		case <-deadline:
			t.Fatalf("saw %d starts, want the daemon retried; states %v", starts, w.states())
		}
		starts = 0
		for _, s := range w.states() {
			if s == StateStarting {
				starts++
			}
		}
	}
}

// The socket is a filesystem rendezvous, so something else can be sitting on
// it. Handing that process an API key is the failure this prevents.
func TestAnImposterOnTheSocketIsNeverTrusted(t *testing.T) {
	executable, socket := fake(t, "fake-imposter")
	_, w, _, _ := run(t, Options{Executable: executable, SocketPath: socket})

	status := w.await(t, "a mismatch", func(s Status) bool { return s.State == StateMismatch })
	if !strings.Contains(status.Detail, "999999") {
		t.Errorf("detail = %q, want it to name the process that answered", status.Detail)
	}
	for _, s := range w.states() {
		if s == StateRunning {
			t.Fatal("reported running against a daemon we did not spawn")
		}
	}
}

func TestTokensAreFreshAndURLSafe(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		token, err := newToken()
		if err != nil {
			t.Fatalf("newToken: %v", err)
		}
		// 256 bits, base64url, unpadded.
		if len(token) != 43 {
			t.Fatalf("token %q is %d chars, want 43", token, len(token))
		}
		if strings.ContainsAny(token, "+/=") {
			t.Errorf("token %q is not URL-safe; it travels in a header", token)
		}
		if seen[token] {
			t.Fatalf("token %q repeated", token)
		}
		seen[token] = true
	}
}

func TestNewRejectsAnIncompleteConfiguration(t *testing.T) {
	if _, err := New(Options{SocketPath: "/tmp/x.sock"}); err == nil {
		t.Error("New succeeded with no executable")
	}
	if _, err := New(Options{Executable: "/bin/true"}); err == nil {
		t.Error("New succeeded with no socket path")
	}
}

func TestLocateHonoursTheOverride(t *testing.T) {
	t.Setenv("STARCH_DAEMON", "/opt/somewhere/starchd")
	got, err := Locate()
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got != "/opt/somewhere/starchd" {
		t.Errorf("Locate = %q", got)
	}
}
