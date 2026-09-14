package client

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
)

const testToken = "handshake-token"

// serve starts an HTTP server on a Unix socket and returns a client for it.
//
// t.TempDir() is deliberately not used: it builds a path from the test name,
// which routinely blows past the 104-byte sun_path limit and fails with a bare
// "invalid argument" that says nothing about why.
func serve(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()

	dir, err := os.MkdirTemp("", "st")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	path := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listening on %s: %v", path, err)
	}

	srv := &http.Server{Handler: handler}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		wg.Wait()
	})

	return New(path, testToken)
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encoding response: %v", err)
	}
}

func TestHealth(t *testing.T) {
	var gotAuth, gotPath string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		writeJSON(t, w, 200, Health{
			Status: "ok", Name: "Starch", Version: "0.2.1",
			APIVersion: APIVersion, PID: 4711, UptimeMS: 128,
		})
	})

	health, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.PID != 4711 || health.Version != "0.2.1" {
		t.Errorf("health = %+v", health)
	}
	if gotAuth != "Bearer "+testToken {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotPath != "/healthz" {
		t.Errorf("path = %q", gotPath)
	}
}

// A daemon speaking a contract we do not know is refused rather than guessed
// at. In practice this means a stale binary left in an old install.
func TestHealthRefusesAnUnknownContractVersion(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, 200, Health{Status: "ok", APIVersion: "v9"})
	})

	if _, err := c.Health(context.Background()); err == nil {
		t.Fatal("Health succeeded against a v9 daemon, want a refusal")
	} else if !strings.Contains(err.Error(), "v9") {
		t.Errorf("error = %v, want it to name the version", err)
	}
}

func TestErrorEnvelopeIsDecoded(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"Bad token."}}`))
	})

	_, err := c.Presets(context.Background())
	if Code(err) != CodeUnauthorized {
		t.Fatalf("code = %q, want %q (err %v)", Code(err), CodeUnauthorized, err)
	}
	if err.Error() != "Bad token." {
		t.Errorf("message = %q, want the daemon's own wording", err.Error())
	}
}

// A failure with no envelope still has to produce something showable. Losing
// the error entirely would read as success to every caller.
func TestErrorWithoutAnEnvelopeStillReports(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>nope</html>"))
	})

	_, err := c.Presets(context.Background())
	if err == nil {
		t.Fatal("a 502 decoded as success")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error = %v, want it to mention the status", err)
	}
}

func TestModelsDecodesTheCatalog(t *testing.T) {
	const body = `{"source":"builtin","providers":[{"id":"gemini","name":"Google AI Studio",
	"requires_key":true,"endpoints":[{"url":"https://example.test/v1beta","name":"Google AI Studio",
	"default_model":"gemini-flash-latest","models":[{"id":"gemini-3.8-flash","name":"Gemini 3.8 Flash",
	"thinking":{"levels":["low","medium","high"],"default":"low"}}]}]}]}`

	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(body))
	})

	cat, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(cat.Providers) != 1 || cat.Providers[0].ID != "gemini" {
		t.Fatalf("providers = %+v", cat.Providers)
	}
	model := cat.Providers[0].Endpoints[0].Models[0]
	if model.Thinking == nil || model.Thinking.Default != "low" {
		t.Errorf("thinking = %+v", model.Thinking)
	}
}

// An endpoint whose models cannot be known ahead of time once emitted `null`
// here and emptied every picker in the macOS shell. Go decodes that to a nil
// slice rather than failing, but a shell that ranged over it without checking
// would still be wrong, so this pins the behaviour.
func TestNullModelsDecodesToAnEmptyList(t *testing.T) {
	const body = `{"source":"builtin","providers":[{"id":"openai_compatible","name":"OpenAI-compatible",
	"requires_key":false,"endpoints":[{"url":"http://localhost:11434/v1","name":"Ollama","models":null,
	"note":"Whatever you have pulled."}]}]}`

	c := serve(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })

	cat, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	endpoint := cat.Providers[0].Endpoints[0]
	if len(endpoint.Models) != 0 {
		t.Errorf("models = %+v, want none", endpoint.Models)
	}
	if endpoint.Note == "" {
		t.Error("an endpoint with no models must carry a note saying why")
	}
}

func TestSessionOmitsAnEmptyBaseURL(t *testing.T) {
	var body map[string]any
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		writeJSON(t, w, 200, SessionResponse{
			OK: true, Provider: "anthropic", Model: "claude-sonnet-5",
			Endpoint: "https://api.anthropic.com",
		})
	})

	resp, err := c.Session(context.Background(), SessionRequest{
		Provider: "anthropic", Model: "claude-sonnet-5", APIKey: "sk-test",
	})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if resp.Endpoint != "https://api.anthropic.com" {
		t.Errorf("endpoint = %q, want the daemon's resolved default echoed back", resp.Endpoint)
	}
	if _, present := body["base_url"]; present {
		t.Error("base_url was sent empty; the daemon's default would be overridden by it")
	}
	if body["api_key"] != "sk-test" {
		t.Errorf("api_key = %v", body["api_key"])
	}
}

func TestRewriteStreamsOverTheSocket(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		for _, event := range []string{
			`{"delta":"Thanks for "}`,
			`{"delta":"the update"}`,
			`{"done":true,"full":"Thanks for the update"}`,
		} {
			_, _ = w.Write([]byte("data: " + event + "\n\n"))
			flusher.Flush()
		}
	})

	var deltas []string
	result, err := c.Rewrite(context.Background(),
		RewriteRequest{Text: "thx for the update", Preset: "professional"},
		func(d string) { deltas = append(deltas, d) },
	)
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if result.Full != "Thanks for the update" {
		t.Errorf("Full = %q", result.Full)
	}
	if len(deltas) != 2 {
		t.Errorf("got %d deltas, want them as they arrived", len(deltas))
	}
}

// Escape has to stop the generation, not just stop reading it. Cancelling the
// context closes the connection, which is what the contract requires; the
// server observing that close is the part worth asserting.
func TestCancellingARewriteClosesTheConnection(t *testing.T) {
	closed := make(chan struct{})
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"delta\":\"Thanks\"}\n\n"))
		flusher.Flush()
		<-r.Context().Done()
		close(closed)
	})

	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan struct{})
	var once sync.Once

	done := make(chan error, 1)
	go func() {
		_, err := c.Rewrite(ctx, RewriteRequest{Text: "thx"}, func(string) {
			once.Do(func() { close(first) })
		})
		done <- err
	}()

	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("no delta arrived")
	}
	cancel()

	select {
	case err := <-done:
		if !strings.Contains(err.Error(), "context canceled") {
			t.Errorf("error = %v, want the cancellation reported as itself", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Rewrite did not return after cancellation")
	}

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the server never saw the connection close, so the provider call would run on")
	}
}

// Errors knowable before the first token are real status codes, so a shell can
// act on them without having consumed a stream that never began.
func TestRewriteWithoutASessionIsAStatusCode(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPreconditionRequired)
		_, _ = w.Write([]byte(`{"error":{"code":"no_session","message":"No provider is configured yet."}}`))
	})

	_, err := c.Rewrite(context.Background(), RewriteRequest{Text: "hi"}, nil)
	if !IsNoSession(err) {
		t.Fatalf("err = %v, want a no_session the shell can retry after", err)
	}
}
