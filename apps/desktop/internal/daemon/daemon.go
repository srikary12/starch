// Package daemon owns the lifetime of the starchd child process.
//
// The daemon is spawned when the shell starts, not on first use: cold-starting
// a process while someone waits on an overlay spends latency the product does
// not have. It is also the process holding the API key, so it must never
// outlive the shell — starchd has its own orphan and idle backstops, but those
// are backstops, not the mechanism.
package daemon

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/srikary12/starch/apps/desktop/internal/client"
	"github.com/srikary12/starch/internal/brand"
)

// HeartbeatInterval is how often the shell pings /healthz. It doubles as the
// heartbeat resetting the daemon's idle-exit timer, so it must stay well
// inside IdleTimeout.
const HeartbeatInterval = 5 * time.Second

// IdleTimeout is how long the daemon survives without hearing from us.
const IdleTimeout = 30 * time.Second

// readyDeadline is how long a freshly spawned daemon gets to answer before its
// silence is reported. Polling is fast during this window so the status
// reaches "connected" promptly rather than after a whole heartbeat.
const readyDeadline = 3 * time.Second

// backoff is the restart schedule, capped. Reset by a successful heartbeat.
var backoff = []time.Duration{
	250 * time.Millisecond, 500 * time.Millisecond,
	time.Second, 2 * time.Second, 5 * time.Second, 15 * time.Second,
}

// State is the coarse condition of the daemon, for anything that displays it.
type State int

const (
	// StateStopped means no daemon is running and none is being started.
	StateStopped State = iota
	// StateStarting means a daemon has been spawned but has not answered yet.
	StateStarting
	// StateRunning means the daemon answered and is the one we spawned.
	StateRunning
	// StateMismatch means something answered that is not our daemon, or not a
	// contract version we speak. Never handed an API key.
	StateMismatch
	// StateFailed means the daemon exited, or stopped answering.
	StateFailed
)

func (s State) String() string {
	switch s {
	case StateStopped:
		return "stopped"
	case StateStarting:
		return "starting"
	case StateRunning:
		return "running"
	case StateMismatch:
		return "mismatch"
	case StateFailed:
		return "failed"
	}
	return "unknown"
}

// Status is what the tray icon and `starch config` report.
type Status struct {
	State  State
	Health client.Health
	// Detail is the daemon's own account of a failure where there is one.
	// "another daemon is already listening on …" is far more use than
	// "exited with status 1".
	Detail string
}

// Healthy reports whether a rewrite can be attempted right now.
func (s Status) Healthy() bool { return s.State == StateRunning }

// Options configures a Supervisor.
type Options struct {
	// Executable is the path to starchd.
	Executable string
	// SocketPath is where the daemon is told to listen.
	SocketPath string
	// Debug turns on the daemon's verbose timing logs.
	Debug bool
	// OnStatus is called whenever the status changes, from the supervisor's
	// own goroutine. It must not block.
	OnStatus func(Status)
	// Logger receives the daemon's stderr. Defaults to slog.Default().
	Logger *slog.Logger

	// HeartbeatInterval overrides HeartbeatInterval. Tests use this.
	HeartbeatInterval time.Duration
	// IdleTimeout overrides IdleTimeout.
	IdleTimeout time.Duration
}

// Supervisor spawns starchd, watches it, and restarts it if it dies.
type Supervisor struct {
	opts Options
	log  *slog.Logger

	mu             sync.Mutex
	status         Status
	client         *client.Client
	restartAttempt int
	lastStderr     string
}

// New returns a supervisor for the daemon at opts.Executable.
func New(opts Options) (*Supervisor, error) {
	if opts.Executable == "" {
		return nil, errors.New("no path to " + brand.Daemon)
	}
	if opts.SocketPath == "" {
		return nil, errors.New("no socket path")
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = HeartbeatInterval
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = IdleTimeout
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Supervisor{opts: opts, log: log}, nil
}

// Client returns a client bound to the running daemon, or nil before a
// successful spawn.
func (s *Supervisor) Client() *client.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

// Status returns the current status.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// WaitHealthy blocks until the daemon answers a heartbeat, or ctx is done.
//
// On timeout it reports the daemon's own last complaint rather than "deadline
// exceeded", because "another daemon is already listening" is actionable and
// a timeout is not.
func (s *Supervisor) WaitHealthy(ctx context.Context) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		if s.Status().Healthy() {
			return nil
		}
		select {
		case <-ctx.Done():
			if detail := s.Status().Detail; detail != "" {
				return errors.New(detail)
			}
			return fmt.Errorf("%s did not start: %w", brand.Daemon, ctx.Err())
		case <-ticker.C:
		}
	}
}

// Run supervises the daemon until ctx is cancelled, then terminates it and
// waits for it to unlink its socket. It only returns an error if the daemon
// could never be started at all.
func (s *Supervisor) Run(ctx context.Context) error {
	defer s.setStatus(Status{State: StateStopped})

	for {
		if ctx.Err() != nil {
			return nil
		}

		err := s.runOnce(ctx)
		if ctx.Err() != nil {
			// Cancellation is the normal exit path: the shell is quitting and
			// the child has already been signalled.
			return nil
		}

		detail := s.failureDetail(err)
		s.log.Error("daemon stopped", "detail", detail)
		s.setStatus(Status{State: StateFailed, Detail: detail})

		s.mu.Lock()
		attempt := s.restartAttempt
		s.restartAttempt++
		s.mu.Unlock()

		delay := backoff[min(attempt, len(backoff)-1)]
		s.log.Info("restarting daemon", "attempt", attempt+1, "in", delay)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

// runOnce spawns one daemon and heartbeats it until it exits or ctx is done.
func (s *Supervisor) runOnce(ctx context.Context) error {
	token, err := newToken()
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.client = client.New(s.opts.SocketPath, token)
	s.lastStderr = ""
	s.mu.Unlock()
	s.setStatus(Status{State: StateStarting})

	cmd := exec.CommandContext(ctx, s.opts.Executable)
	cmd.Env = s.environment(token)
	// SIGTERM rather than the default SIGKILL: the daemon unlinks its socket
	// on the way out, and a killed one leaves a file the next start has to
	// probe and clear. WaitDelay is the backstop for one that ignores it.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 2 * time.Second
	// The child inherits our process group on purpose: killing the shell's
	// group from a terminal should take the daemon with it.
	cmd.Stdout = nil
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("wiring up %s stderr: %w", brand.Daemon, err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launching %s: %w", brand.Daemon, err)
	}
	s.log.Info("spawned daemon", "pid", cmd.Process.Pid, "socket", s.opts.SocketPath)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.forwardLog(stderr)
	}()

	beat, stopBeating := context.WithCancel(ctx)
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.heartbeat(beat, cmd.Process.Pid)
	}()

	err = cmd.Wait()
	stopBeating()
	wg.Wait()

	s.mu.Lock()
	s.client = nil
	s.mu.Unlock()
	return err
}

// environment is deliberately small: the daemon needs nothing from the user's
// shell, and a narrow environment is one less way for a stray variable to
// change its behaviour. The XDG variables are passed through because the
// daemon resolves its own presets path from them.
func (s *Supervisor) environment(token string) []string {
	env := []string{
		"PATH=/usr/bin:/bin",
		brand.EnvPrefix + "TOKEN=" + token,
		brand.EnvPrefix + "SOCKET=" + s.opts.SocketPath,
		brand.EnvPrefix + "IDLE_TIMEOUT=" + s.opts.IdleTimeout.String(),
	}
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR"} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	if s.opts.Debug {
		env = append(env, brand.EnvPrefix+"DEBUG=1")
	}
	return env
}

// heartbeat polls /healthz, fast until the daemon answers and then on the
// interval, which is also what keeps the daemon's idle timer from firing.
func (s *Supervisor) heartbeat(ctx context.Context, pid int) {
	deadline := time.Now().Add(readyDeadline)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return
		}
		if s.checkHealth(ctx, pid, true) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	// Either it answered, or three seconds passed and its silence is worth
	// reporting. Either way the loop below takes over.
	if !s.Status().Healthy() {
		s.checkHealth(ctx, pid, false)
	}

	ticker := time.NewTicker(s.opts.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkHealth(ctx, pid, false)
		}
	}
}

// checkHealth pings the daemon and updates the status. quiet suppresses the
// failure report during the startup window, where a refused connection just
// means the socket is not bound yet.
func (s *Supervisor) checkHealth(ctx context.Context, pid int, quiet bool) bool {
	c := s.Client()
	if c == nil {
		return false
	}

	ping, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	health, err := c.Health(ping)
	if err != nil {
		if !quiet && ctx.Err() == nil {
			s.setStatus(Status{State: StateFailed, Detail: err.Error()})
		}
		return false
	}

	// The socket is a filesystem rendezvous. Confirm the process answering is
	// the one we spawned before trusting it with an API key.
	if health.PID != pid {
		s.setStatus(Status{
			State:  StateMismatch,
			Detail: fmt.Sprintf("Another %s (pid %d) is using the socket.", brand.Daemon, health.PID),
		})
		return false
	}

	s.mu.Lock()
	s.restartAttempt = 0
	s.mu.Unlock()
	s.setStatus(Status{State: StateRunning, Health: health})
	return true
}

// forwardLog relays the daemon's stderr into the shell's log.
//
// The daemon never logs user text or key material, which is what makes it safe
// to relay verbatim. Keep it that way on the Go side.
func (s *Supervisor) forwardLog(r io.Reader) {
	// bufio.Reader, not Scanner: a long line should be relayed, not silently
	// cut at Scanner's default limit.
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadString('\n')
		if trimmed := strings.TrimRight(line, "\r\n"); trimmed != "" {
			s.log.Info("starchd: " + trimmed)
			// A fatal startup error is printed bare ("starchd: ..."), while
			// normal operation logs structured key=value lines. Keeping only
			// the bare ones avoids reporting a routine "listening" line as a
			// failure.
			if !strings.Contains(trimmed, "level=") {
				s.mu.Lock()
				s.lastStderr = trimmed
				s.mu.Unlock()
			}
		}
		if err != nil {
			return
		}
	}
}

// failureDetail prefers the daemon's own account of why it gave up.
func (s *Supervisor) failureDetail(err error) string {
	s.mu.Lock()
	reported := s.lastStderr
	s.mu.Unlock()

	if reported != "" {
		return reported
	}
	if err != nil {
		return err.Error()
	}
	return brand.Daemon + " exited unexpectedly"
}

func (s *Supervisor) setStatus(status Status) {
	s.mu.Lock()
	changed := s.status.State != status.State ||
		s.status.Detail != status.Detail ||
		s.status.Health.PID != status.Health.PID ||
		s.status.Health.Version != status.Health.Version
	s.status = status
	s.mu.Unlock()

	if changed && s.opts.OnStatus != nil {
		s.opts.OnStatus(status)
	}
}

// newToken returns 256 bits from the system CSPRNG, base64url without padding.
func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating a handshake token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// Locate finds the starchd binary.
//
// Next to the shell first: that is how an install lays them out, and it is the
// only arrangement where a half-upgraded system cannot pair a new shell with
// an old daemon. PATH is the fallback for a development tree.
func Locate() (string, error) {
	if override := os.Getenv(brand.EnvPrefix + "DAEMON"); override != "" {
		return override, nil
	}

	if self, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(self); err == nil {
			self = resolved
		}
		candidate := filepath.Join(filepath.Dir(self), brand.Daemon)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}

	if found, err := exec.LookPath(brand.Daemon); err == nil {
		return found, nil
	}
	return "", fmt.Errorf("could not find %s next to the shell or on PATH", brand.Daemon)
}
