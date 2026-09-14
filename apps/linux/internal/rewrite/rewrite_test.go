package rewrite

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srikary12/starch/apps/linux/internal/client"
	"github.com/srikary12/starch/apps/linux/internal/settings"
)

// daemonStub is a starchd that only knows about sessions and rewrites.
type daemonStub struct {
	mu       sync.Mutex
	sessions []client.SessionRequest
	rewrites int
	// dropSession makes the next rewrite answer 428, the way a daemon that
	// restarted or idled out does.
	dropSession bool
}

func (d *daemonStub) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/session":
			var req client.SessionRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decoding session: %v", err)
			}
			d.mu.Lock()
			d.sessions = append(d.sessions, req)
			d.dropSession = false
			d.mu.Unlock()
			_ = json.NewEncoder(w).Encode(client.SessionResponse{
				OK: true, Provider: req.Provider, Model: req.Model, Endpoint: req.BaseURL,
			})

		case "/v1/rewrite":
			d.mu.Lock()
			d.rewrites++
			drop := d.dropSession
			d.mu.Unlock()

			if drop {
				w.WriteHeader(http.StatusPreconditionRequired)
				_, _ = w.Write([]byte(`{"error":{"code":"no_session","message":"No provider is configured yet."}}`))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = w.Write([]byte("data: {\"delta\":\"Thanks\"}\n\ndata: {\"done\":true,\"full\":\"Thanks\"}\n\n"))

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func serve(t *testing.T, handler http.HandlerFunc) *client.Client {
	t.Helper()

	dir, err := os.MkdirTemp("", "st")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	path := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return client.New(path, "token")
}

func config() Config {
	prefs := settings.Default()
	prefs.ThinkingEffort = "low"
	return Config{Preferences: prefs, APIKey: "sk-test"}
}

func TestEnsureSessionSendsThePreferences(t *testing.T) {
	stub := &daemonStub{}
	c := serve(t, stub.handler(t))
	cfg := config()

	if err := EnsureSession(context.Background(), c, cfg); err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(stub.sessions))
	}
	sent := stub.sessions[0]
	if sent.Provider != cfg.Preferences.Provider || sent.Model != cfg.Preferences.Model {
		t.Errorf("sent %+v", sent)
	}
	if sent.APIKey != "sk-test" {
		t.Errorf("api key = %q", sent.APIKey)
	}
	if sent.ThinkingEffort != "low" {
		t.Errorf("effort = %q", sent.ThinkingEffort)
	}
}

// Nothing configured is not a provider failure, and telling someone their
// endpoint is unreachable when they have not chosen one is worse than useless.
func TestEnsureSessionWithNothingConfigured(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("the daemon was called with no provider configured")
	})

	err := EnsureSession(context.Background(), c, Config{})
	if err == nil || !strings.Contains(err.Error(), "starch config") {
		t.Fatalf("err = %v, want one naming the command that fixes it", err)
	}
}

// The daemon restarting or idling out takes its session with it. That must be
// invisible: credentials are re-sent and the rewrite retried once.
func TestARewriteSurvivesTheSessionBeingLost(t *testing.T) {
	stub := &daemonStub{dropSession: true}
	c := serve(t, stub.handler(t))

	var got strings.Builder
	result, err := Run(context.Background(), c, config(),
		client.RewriteRequest{Text: "thx", Preset: "professional"},
		func(d string) { got.WriteString(d) })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Full != "Thanks" {
		t.Errorf("Full = %q", result.Full)
	}
	if got.String() != "Thanks" {
		t.Errorf("deltas = %q", got.String())
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.sessions) != 1 {
		t.Errorf("got %d sessions, want exactly one re-established", len(stub.sessions))
	}
	if stub.rewrites != 2 {
		t.Errorf("got %d rewrite attempts, want the first plus one retry", stub.rewrites)
	}
}

// One retry, not a loop: a key the endpoint genuinely rejects must surface.
func TestARewriteIsRetriedOnlyOnce(t *testing.T) {
	stub := &daemonStub{}
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/rewrite" {
			stub.mu.Lock()
			stub.rewrites++
			stub.mu.Unlock()
			w.WriteHeader(http.StatusPreconditionRequired)
			_, _ = w.Write([]byte(`{"error":{"code":"no_session","message":"No provider configured."}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(client.SessionResponse{OK: true})
	})

	_, err := Run(context.Background(), c, config(), client.RewriteRequest{Text: "thx"}, nil)
	if !client.IsNoSession(err) {
		t.Fatalf("err = %v, want the no_session surfaced after the retry", err)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.rewrites != 2 {
		t.Errorf("got %d attempts, want two", stub.rewrites)
	}
}
