package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testToken = "test-handshake-token"

// tempSocket returns a short-lived socket path.
//
// t.TempDir is deliberately not used: on macOS it returns a path under
// /var/folders/<...>/T/<TestName>/<n>, and once a long test name is appended
// that routinely blows past the 104-byte sun_path limit. The failure mode is a
// bare "invalid argument" from bind, which is a miserable thing to debug.
func tempSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "st")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	path := filepath.Join(dir, "d.sock")
	if len(path) > maxSocketPathLen {
		t.Skipf("TMPDIR yields a %d-byte socket path, over the %d-byte limit", len(path), maxSocketPathLen)
	}
	return path
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startDaemon runs a Server on a real Unix socket and returns a client bound
// to it plus a channel carrying Serve's eventual return value.
func startDaemon(t *testing.T, opts Options) (*http.Client, string, *Server, <-chan error) {
	t.Helper()
	if opts.Token == "" {
		opts.Token = testToken
	}
	if opts.Logger == nil {
		opts.Logger = quietLogger()
	}
	if opts.PresetDir == "" {
		// Never the real support directory: a test run must not rewrite the
		// developer's own presets.json.
		opts.PresetDir = t.TempDir()
	}

	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	path := tempSocket(t)
	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	// finished is separate from done because a test may consume done itself
	// (TestIdleExit does); cleanup needs a signal it cannot race for.
	finished := make(chan struct{})
	go func() {
		done <- srv.Serve(ctx, ln)
		close(finished)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return within 5s of cancellation")
		}
		_ = ln.Close()
	})

	return unixClient(path), path, srv, done
}

func unixClient(path string) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", path)
			},
		},
	}
}

// do issues a request to the daemon. An empty token omits the header entirely.
func do(t *testing.T, c *http.Client, method, path, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, "http://starchd"+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decodeError(t *testing.T, resp *http.Response) APIError {
	t.Helper()
	var body ErrorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding error envelope: %v", err)
	}
	return body.Error
}

func TestHealthzOverUnixSocket(t *testing.T) {
	c, _, _, _ := startDaemon(t, Options{Version: "1.2.3"})

	resp := do(t, c, http.MethodGet, "/healthz", testToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}

	var h Health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatalf("decoding health: %v", err)
	}
	if h.Status != "ok" {
		t.Errorf("Status = %q, want ok", h.Status)
	}
	if h.Version != "1.2.3" {
		t.Errorf("Version = %q, want 1.2.3", h.Version)
	}
	if h.APIVersion != APIVersion {
		t.Errorf("APIVersion = %q, want %q", h.APIVersion, APIVersion)
	}
	if h.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", h.PID, os.Getpid())
	}
}

func TestAuth(t *testing.T) {
	c, _, _, _ := startDaemon(t, Options{})

	tests := []struct {
		name   string
		header string
		want   int
	}{
		{"correct token", "Bearer " + testToken, http.StatusOK},
		{"scheme is case-insensitive", "bearer " + testToken, http.StatusOK},
		{"no header", "", http.StatusUnauthorized},
		{"wrong token", "Bearer wrong-token", http.StatusUnauthorized},
		{"empty token", "Bearer ", http.StatusUnauthorized},
		{"token as prefix of real one", "Bearer " + testToken[:5], http.StatusUnauthorized},
		{"token with trailing junk", "Bearer " + testToken + "x", http.StatusUnauthorized},
		{"wrong scheme", "Basic " + testToken, http.StatusUnauthorized},
		{"bare token, no scheme", testToken, http.StatusUnauthorized},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, "http://starchd/healthz", nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			resp, err := c.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if tc.want == http.StatusUnauthorized {
				if code := decodeError(t, resp).Code; code != ErrUnauthorized {
					t.Errorf("code = %q, want %q", code, ErrUnauthorized)
				}
			}
		})
	}
}

// Every failure must use the JSON envelope. A shell that has to sniff for
// plain-text bodies will get it wrong.
func TestErrorsAreAlwaysJSON(t *testing.T) {
	c, _, _, _ := startDaemon(t, Options{})

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantCode   ErrorCode
		wantAllow  string
	}{
		{"unknown path", http.MethodGet, "/nope", http.StatusNotFound, ErrNotFound, ""},
		// Documented in the contract, lands in M3/M4. A shell built against
		// the contract must get the JSON envelope here, not Go's plain-text
		// default, so it can tell "not built yet" from "wrong URL".
		{"contract route not yet served", http.MethodGet, "/v1/accept", http.StatusNotFound, ErrNotFound, ""},
		{"wrong method on healthz", http.MethodPost, "/healthz", http.StatusMethodNotAllowed, ErrMethodNotAllowed, http.MethodGet},
		{"wrong method on rewrite", http.MethodGet, "/v1/rewrite", http.StatusMethodNotAllowed, ErrMethodNotAllowed, http.MethodPost},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, c, tc.method, tc.path, testToken)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if got := decodeError(t, resp).Code; got != tc.wantCode {
				t.Errorf("code = %q, want %q", got, tc.wantCode)
			}
			// A 405 must name the method that would work, or a shell author
			// is left guessing at the contract.
			if tc.wantAllow != "" {
				if allow := resp.Header.Get("Allow"); allow != tc.wantAllow {
					t.Errorf("Allow = %q, want %q", allow, tc.wantAllow)
				}
			}
		})
	}
}

// Auth must run before routing, so an unauthenticated caller cannot map the
// daemon's route table by watching 404s and 401s diverge.
func TestUnknownRouteStillRequiresAuth(t *testing.T) {
	c, _, _, _ := startDaemon(t, Options{})

	resp := do(t, c, http.MethodGet, "/nope", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestNewRequiresToken(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New with no token succeeded, want error")
	}
}

func TestServeReturnsNilOnContextCancel(t *testing.T) {
	srv, err := New(Options{Token: testToken, Logger: quietLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := Listen(tempSocket(t))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}
}

func TestIdleExit(t *testing.T) {
	_, _, _, done := startDaemon(t, Options{IdleTimeout: 100 * time.Millisecond})

	select {
	case err := <-done:
		if !errors.Is(err, ErrIdle) {
			t.Fatalf("Serve = %v, want ErrIdle", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not exit after going idle")
	}
}

func TestRequestsPostponeIdleExit(t *testing.T) {
	const idle = 300 * time.Millisecond
	c, _, _, done := startDaemon(t, Options{IdleTimeout: idle})

	// Heartbeat across more than one idle window, as the shell does.
	deadline := time.Now().Add(idle * 2)
	for time.Now().Before(deadline) {
		resp := do(t, c, http.MethodGet, "/healthz", testToken)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("heartbeat status = %d", resp.StatusCode)
		}
		select {
		case err := <-done:
			t.Fatalf("daemon exited while being heartbeated: %v", err)
		default:
		}
		time.Sleep(idle / 4)
	}
}

// A long-running SSE stream must not look idle just because it started before
// the window opened.
func TestIdleSignalWaitsForInFlightRequests(t *testing.T) {
	srv, err := New(Options{Token: testToken, IdleTimeout: 50 * time.Millisecond, Logger: quietLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv.inFlight.Add(1) // stand in for a rewrite stream still writing deltas
	idle := srv.idleSignal(ctx)

	select {
	case <-idle:
		t.Fatal("idle fired while a request was in flight")
	case <-time.After(300 * time.Millisecond):
	}

	srv.inFlight.Add(-1)
	select {
	case <-idle:
	case <-time.After(2 * time.Second):
		t.Fatal("idle did not fire after the request finished")
	}
}

func TestZeroIdleTimeoutNeverExits(t *testing.T) {
	srv, err := New(Options{Token: testToken, IdleTimeout: 0, Logger: quietLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	select {
	case <-srv.idleSignal(ctx):
		t.Fatal("idle fired with the timeout disabled")
	case <-time.After(200 * time.Millisecond):
	}
}

// startTestServer is the common case: a running daemon plus its Server, for
// tests that need to inspect internal counters.
func startTestServer(t *testing.T) (*http.Client, *Server) {
	t.Helper()
	c, _, srv, _ := startDaemon(t, Options{})
	return c, srv
}

// doBody issues a request with a JSON body.
func doBody(t *testing.T, c *http.Client, method, path, token string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, "http://starchd"+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}
