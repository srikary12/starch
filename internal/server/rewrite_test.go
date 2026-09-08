package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These exercise the daemon end to end over a real Unix socket, against a
// stubbed provider. No network, no key, no cost — a contributor without an API
// key must still be able to run the suite.

// upstream stands in for a model endpoint.
type upstream struct {
	*httptest.Server
	requests atomic.Int64
	// cancelled is closed when the upstream sees the client disconnect.
	cancelled chan struct{}
}

// newUpstream serves the given SSE events, one flush each.
func newUpstream(t *testing.T, events ...string) *upstream {
	t.Helper()
	u := &upstream{cancelled: make(chan struct{})}
	var closeOnce atomic.Bool

	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, e := range events {
			select {
			case <-r.Context().Done():
				if closeOnce.CompareAndSwap(false, true) {
					close(u.cancelled)
				}
				return
			default:
			}
			fmt.Fprint(w, e)
			flusher.Flush()
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func anthropicDelta(text string) string {
	return "event: content_block_delta\n" +
		fmt.Sprintf(`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":%q}}`, text) + "\n\n"
}

const anthropicStop = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

// configureSession points the daemon at an upstream.
func configureSession(t *testing.T, c *http.Client, endpoint string) {
	t.Helper()
	body, _ := json.Marshal(sessionRequest{
		Provider: ProviderAnthropic,
		Model:    "claude-sonnet-5",
		BaseURL:  endpoint,
		APIKey:   "sk-test-key",
	})
	resp := doBody(t, c, http.MethodPost, "/v1/session", testToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("session: status %d: %s", resp.StatusCode, raw)
	}
}

// sseFrame is one decoded event from the daemon.
type sseFrame struct {
	Delta string `json:"delta"`
	Done  bool   `json:"done"`
	Full  string `json:"full"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *APIError `json:"error"`
}

// readFrames decodes the daemon's stream.
func readFrames(t *testing.T, body io.Reader) []sseFrame {
	t.Helper()
	var frames []sseFrame
	scanner := bufio.NewReader(body)
	for {
		line, err := scanner.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimRight(line, "\r\n")
			if after, ok := strings.CutPrefix(trimmed, "data: "); ok {
				var f sseFrame
				if err := json.Unmarshal([]byte(after), &f); err != nil {
					t.Fatalf("undecodable frame %q: %v", after, err)
				}
				frames = append(frames, f)
			}
		}
		if err != nil {
			return frames
		}
	}
}

func TestRewriteStreamsAndTerminates(t *testing.T) {
	up := newUpstream(t,
		anthropicDelta("Thanks for "),
		anthropicDelta("the update."),
		anthropicStop,
	)
	c, _ := startTestServer(t)
	configureSession(t, c, up.URL)

	body, _ := json.Marshal(rewriteRequest{Text: "thanks for teh update", Preset: "professional"})
	resp := doBody(t, c, http.MethodPost, "/v1/rewrite", testToken, body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}

	frames := readFrames(t, resp.Body)
	if len(frames) < 2 {
		t.Fatalf("got %d frames, want deltas plus a terminal event", len(frames))
	}

	var text strings.Builder
	for _, f := range frames[:len(frames)-1] {
		text.WriteString(f.Delta)
	}
	if got := text.String(); got != "Thanks for the update." {
		t.Errorf("concatenated deltas = %q", got)
	}

	final := frames[len(frames)-1]
	if !final.Done {
		t.Fatal("stream did not end with a done event")
	}
	if final.Full != "Thanks for the update." {
		t.Errorf("full = %q", final.Full)
	}
}

// The contract says exactly one terminal event, and that clients should prefer
// `full` over their own concatenation.
func TestRewriteSendsExactlyOneTerminalEvent(t *testing.T) {
	up := newUpstream(t, anthropicDelta("a"), anthropicDelta("b"), anthropicStop)
	c, _ := startTestServer(t)
	configureSession(t, c, up.URL)

	body, _ := json.Marshal(rewriteRequest{Text: "x"})
	resp := doBody(t, c, http.MethodPost, "/v1/rewrite", testToken, body)
	defer resp.Body.Close()

	frames := readFrames(t, resp.Body)
	done := 0
	for _, f := range frames {
		if f.Done {
			done++
		}
	}
	if done != 1 {
		t.Errorf("got %d terminal events, want exactly 1", done)
	}
	if !frames[len(frames)-1].Done {
		t.Error("the terminal event is not last")
	}
}

func TestRewriteRequiresASession(t *testing.T) {
	c, _ := startTestServer(t)

	body, _ := json.Marshal(rewriteRequest{Text: "hello"})
	resp := doBody(t, c, http.MethodPost, "/v1/rewrite", testToken, body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPreconditionRequired {
		t.Errorf("status = %d, want 428", resp.StatusCode)
	}
	var envelope ErrorBody
	json.NewDecoder(resp.Body).Decode(&envelope)
	if envelope.Error.Code != ErrNoSession {
		t.Errorf("code = %q, want %q", envelope.Error.Code, ErrNoSession)
	}
}

func TestRewriteRejectsEmptyText(t *testing.T) {
	c, _ := startTestServer(t)
	up := newUpstream(t, anthropicStop)
	configureSession(t, c, up.URL)

	for _, text := range []string{"", "   ", "\n\t "} {
		body, _ := json.Marshal(rewriteRequest{Text: text})
		resp := doBody(t, c, http.MethodPost, "/v1/rewrite", testToken, body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("text %q: status = %d, want 400", text, resp.StatusCode)
		}
	}
	if up.requests.Load() != 0 {
		t.Error("an empty selection reached the provider — that is a billed request for nothing")
	}
}

// A failure knowable before any output should be a real status code, not a
// 200 followed by an in-band error: the shell can then show it without having
// consumed a stream that never started.
func TestRewriteReportsAuthFailureAsAStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	}))
	defer upstream.Close()

	c, _ := startTestServer(t)
	configureSession(t, c, upstream.URL)

	body, _ := json.Marshal(rewriteRequest{Text: "hello"})
	resp := doBody(t, c, http.MethodPost, "/v1/rewrite", testToken, body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	var envelope ErrorBody
	json.NewDecoder(resp.Body).Decode(&envelope)
	if envelope.Error.Code != "provider_auth" {
		t.Errorf("code = %q, want provider_auth", envelope.Error.Code)
	}
	if !strings.Contains(envelope.Error.Message, "Settings") {
		t.Errorf("message is not actionable: %q", envelope.Error.Message)
	}
	if strings.Contains(envelope.Error.Message, "sk-test-key") {
		t.Error("the API key leaked into the error message")
	}
}

// An error after output has started has to arrive in-band, because the 200 is
// already sent.
func TestRewriteReportsMidStreamErrorInBand(t *testing.T) {
	up := newUpstream(t,
		anthropicDelta("partial"),
		"event: error\n"+`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`+"\n\n",
	)
	c, _ := startTestServer(t)
	configureSession(t, c, up.URL)

	body, _ := json.Marshal(rewriteRequest{Text: "hello"})
	resp := doBody(t, c, http.MethodPost, "/v1/rewrite", testToken, body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the stream had already started", resp.StatusCode)
	}

	frames := readFrames(t, resp.Body)
	last := frames[len(frames)-1]
	if last.Error == nil {
		t.Fatal("no in-band error frame")
	}
	if last.Error.Code != "provider_overloaded" {
		t.Errorf("code = %q", last.Error.Code)
	}
}

// The contract's cancellation requirement, all the way through: the shell
// disconnects, and the upstream generation stops being billed.
func TestClientDisconnectCancelsUpstream(t *testing.T) {
	slow := &upstream{cancelled: make(chan struct{})}
	var closeOnce atomic.Bool
	slow.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprint(w, anthropicDelta("first"))
		flusher.Flush()
		for {
			select {
			case <-r.Context().Done():
				if closeOnce.CompareAndSwap(false, true) {
					close(slow.cancelled)
				}
				return
			case <-time.After(5 * time.Millisecond):
				fmt.Fprint(w, anthropicDelta("x"))
				flusher.Flush()
			}
		}
	}))
	defer slow.Close()

	c, _ := startTestServer(t)
	configureSession(t, c, slow.URL)

	ctx, cancel := context.WithCancel(context.Background())
	body, _ := json.Marshal(rewriteRequest{Text: "hello"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://starchd/v1/rewrite", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)

	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	// Wait for the first delta so the stream is genuinely underway.
	buf := make([]byte, 1)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("reading first byte: %v", err)
	}

	// This is Escape.
	cancel()
	resp.Body.Close()

	select {
	case <-slow.cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream kept generating after the client disconnected — the user is still being billed")
	}
}

func TestSessionNeverEchoesTheKey(t *testing.T) {
	c, _ := startTestServer(t)

	const key = "sk-ant-SECRET-value-123456"
	body, _ := json.Marshal(sessionRequest{
		Provider: ProviderAnthropic,
		Model:    "claude-sonnet-5",
		APIKey:   key,
	})
	resp := doBody(t, c, http.MethodPost, "/v1/session", testToken, body)
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), key) {
		t.Errorf("the session response echoes the API key: %s", raw)
	}

	var parsed sessionResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if !parsed.OK || parsed.Model != "claude-sonnet-5" {
		t.Errorf("unexpected response: %+v", parsed)
	}
	// A default endpoint should be resolved and reported back.
	if parsed.Endpoint == "" {
		t.Error("no endpoint reported; the shell cannot confirm what was resolved")
	}
}

func TestSessionValidation(t *testing.T) {
	tests := []struct {
		name string
		req  sessionRequest
	}{
		{"unknown provider", sessionRequest{Provider: "palm", Model: "m"}},
		{"missing model", sessionRequest{Provider: ProviderAnthropic}},
		{"openai-compatible without an endpoint", sessionRequest{Provider: ProviderOpenAICompatible, Model: "m"}},
	}
	c, _ := startTestServer(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.req)
			resp := doBody(t, c, http.MethodPost, "/v1/session", testToken, body)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
			var envelope ErrorBody
			json.NewDecoder(resp.Body).Decode(&envelope)
			if envelope.Error.Message == "" {
				t.Error("no message for the user")
			}
		})
	}
}

// A second session must take effect, or changing provider in Settings would
// silently keep using the old one.
func TestSessionCanBeReplaced(t *testing.T) {
	first := newUpstream(t, anthropicDelta("first"), anthropicStop)
	second := newUpstream(t, anthropicDelta("second"), anthropicStop)

	c, _ := startTestServer(t)
	configureSession(t, c, first.URL)
	configureSession(t, c, second.URL)

	body, _ := json.Marshal(rewriteRequest{Text: "x"})
	resp := doBody(t, c, http.MethodPost, "/v1/rewrite", testToken, body)
	defer resp.Body.Close()
	frames := readFrames(t, resp.Body)

	if got := frames[len(frames)-1].Full; got != "second" {
		t.Errorf("full = %q, want the replacement session's upstream", got)
	}
	if first.requests.Load() != 0 {
		t.Error("the replaced session's endpoint was still used")
	}
}

// A long stream must not look idle. The idle timer is what stops an orphaned
// daemon holding a key, so it has to count an in-flight stream as activity.
func TestStreamingCountsAsActivity(t *testing.T) {
	up := newUpstream(t, anthropicDelta("a"), anthropicStop)
	c, srv := startTestServer(t)
	configureSession(t, c, up.URL)

	body, _ := json.Marshal(rewriteRequest{Text: "x"})
	resp := doBody(t, c, http.MethodPost, "/v1/rewrite", testToken, body)
	readFrames(t, resp.Body)
	resp.Body.Close()

	if srv.inFlight.Load() != 0 {
		t.Errorf("in-flight count leaked: %d", srv.inFlight.Load())
	}
}

// Gemini goes through the same routing, so it needs the same end-to-end proof:
// the daemon builds a valid request, decodes the stream, and terminates.
func TestRewriteViaGemini(t *testing.T) {
	var gotQuery, gotKeyHeader string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		gotKeyHeader = r.Header.Get("X-Goog-Api-Key")
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprint(w, `data: {"candidates":[{"content":{"parts":[{"text":"Thanks for the update."}]}}]}`+"\n\n")
		flusher.Flush()
		fmt.Fprint(w, `data: {"candidates":[{"content":{"parts":[{"text":""}]},"finishReason":"STOP"}],`+
			`"usageMetadata":{"promptTokenCount":31,"candidatesTokenCount":17}}`+"\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	c, _ := startTestServer(t)
	body, _ := json.Marshal(sessionRequest{
		Provider: ProviderGemini,
		Model:    "gemini-flash-latest",
		BaseURL:  upstream.URL,
		APIKey:   "AIzaSy-test",
	})
	resp := doBody(t, c, http.MethodPost, "/v1/session", testToken, body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session: %d", resp.StatusCode)
	}

	body, _ = json.Marshal(rewriteRequest{Text: "thanks for teh update", Preset: "professional"})
	resp = doBody(t, c, http.MethodPost, "/v1/rewrite", testToken, body)
	defer resp.Body.Close()

	frames := readFrames(t, resp.Body)
	final := frames[len(frames)-1]
	if !final.Done {
		t.Fatal("no terminal event")
	}
	if final.Full != "Thanks for the update." {
		t.Errorf("full = %q", final.Full)
	}
	if final.Usage == nil || final.Usage.InputTokens != 31 {
		t.Errorf("usage = %+v", final.Usage)
	}
	// Without alt=sse the endpoint streams a JSON array instead of events.
	if gotQuery != "alt=sse" {
		t.Errorf("query = %q, want alt=sse", gotQuery)
	}
	if gotKeyHeader != "AIzaSy-test" {
		t.Errorf("key header = %q", gotKeyHeader)
	}
}

func TestSessionRejectsUnknownProviderNamingTheValidOnes(t *testing.T) {
	c, _ := startTestServer(t)
	body, _ := json.Marshal(sessionRequest{Provider: "palm", Model: "m"})
	resp := doBody(t, c, http.MethodPost, "/v1/session", testToken, body)
	defer resp.Body.Close()

	var envelope ErrorBody
	json.NewDecoder(resp.Body).Decode(&envelope)
	for _, name := range []string{"anthropic", "gemini", "openai_compatible"} {
		if !strings.Contains(envelope.Error.Message, name) {
			t.Errorf("message does not name %q: %q", name, envelope.Error.Message)
		}
	}
}

// A pasted full request URL — the form every Google example shows — must be
// normalised, and the echo must report what was actually resolved rather than
// the raw paste.
func TestSessionNormalisesAPastedGeminiURL(t *testing.T) {
	c, _ := startTestServer(t)

	body, _ := json.Marshal(sessionRequest{
		Provider: ProviderGemini,
		Model:    "gemini-flash-latest",
		BaseURL:  "https://generativelanguage.googleapis.com/v1beta/models/gemini-flash-latest:generateContent",
		APIKey:   "AIzaSy-test",
	})
	resp := doBody(t, c, http.MethodPost, "/v1/session", testToken, body)
	defer resp.Body.Close()

	var parsed sessionResponse
	json.NewDecoder(resp.Body).Decode(&parsed)

	const want = "https://generativelanguage.googleapis.com/v1beta"
	if parsed.Endpoint != want {
		t.Errorf("endpoint = %q, want %q", parsed.Endpoint, want)
	}
}
