package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Every test here runs against an httptest stub. The daemon must be testable
// with no network at all: a suite that reaches a real provider is slow, flaky,
// costs money, and cannot be run by a contributor without a key.

// collect drains a stream into its text, usage and terminal error.
func collect(t *testing.T, deltas <-chan Delta) (string, *Usage, error) {
	t.Helper()
	var (
		text  strings.Builder
		usage *Usage
		err   error
	)
	for d := range deltas {
		switch {
		case d.Err != nil:
			err = d.Err
		case d.Done:
			usage = d.Usage
		default:
			text.WriteString(d.Text)
		}
	}
	return text.String(), usage, err
}

// sseServer serves a canned event stream, flushing each write so the client
// sees it as a stream rather than one buffered body.
func sseServer(t *testing.T, events ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server cannot flush")
			return
		}
		for _, e := range events {
			fmt.Fprint(w, e)
			flusher.Flush()
		}
	}))
}

func anthropicTextEvents(chunks ...string) []string {
	events := []string{
		"event: message_start\n" +
			`data: {"type":"message_start","message":{"usage":{"input_tokens":42,"output_tokens":0}}}` + "\n\n",
		"event: content_block_start\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n",
	}
	for _, c := range chunks {
		events = append(events,
			"event: content_block_delta\n"+
				fmt.Sprintf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, c)+"\n\n")
	}
	events = append(events,
		"event: message_delta\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`+"\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	return events
}

func TestAnthropicStreamsText(t *testing.T) {
	srv := sseServer(t, anthropicTextEvents("Thanks for ", "the update.")...)
	defer srv.Close()

	p := NewAnthropic(srv.Client(), srv.URL, "test-key", "claude-sonnet-5")
	deltas, err := p.Stream(context.Background(), Request{System: "sys", User: "text"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	text, usage, streamErr := collect(t, deltas)
	if streamErr != nil {
		t.Fatalf("stream error: %v", streamErr)
	}
	if want := "Thanks for the update."; text != want {
		t.Errorf("got %q, want %q", text, want)
	}
	if usage == nil {
		t.Fatal("no usage reported")
	}
	if usage.InputTokens != 42 || usage.OutputTokens != 9 {
		t.Errorf("got usage %+v, want {42 9}", *usage)
	}
}

// With thinking enabled the same event type carries the model's reasoning.
// Emitting it would paste the chain of thought into the user's document.
func TestAnthropicDropsThinkingDeltas(t *testing.T) {
	srv := sseServer(t,
		"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me consider the tone..."}}`+"\n\n",
		"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Thanks for the update."}}`+"\n\n",
		"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"abc123"}}`+"\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	defer srv.Close()

	p := NewAnthropic(srv.Client(), srv.URL, "k", "m")
	deltas, err := p.Stream(context.Background(), Request{User: "x"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	text, _, _ := collect(t, deltas)
	if want := "Thanks for the update."; text != want {
		t.Errorf("got %q, want %q — reasoning must never reach the output", text, want)
	}
}

func TestAnthropicSendsRequiredHeaders(t *testing.T) {
	var gotKey, gotVersion, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
		gotVersion = r.Header.Get("Anthropic-Version")
		gotAccept = r.Header.Get("Accept")
		if r.URL.Path != "/v1/messages" {
			t.Errorf("got path %q, want /v1/messages", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	p := NewAnthropic(srv.Client(), srv.URL, "sk-test", "claude-sonnet-5")
	deltas, err := p.Stream(context.Background(), Request{User: "x"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	collect(t, deltas)

	if gotKey != "sk-test" {
		t.Errorf("X-Api-Key = %q", gotKey)
	}
	if gotVersion != anthropicVersion {
		t.Errorf("Anthropic-Version = %q, want %q", gotVersion, anthropicVersion)
	}
	if gotAccept != "text/event-stream" {
		t.Errorf("Accept = %q", gotAccept)
	}
}

func TestOpenAIStreamsText(t *testing.T) {
	srv := sseServer(t,
		`data: {"choices":[{"delta":{"content":"Thanks for "}}]}`+"\n\n",
		`data: {"choices":[{"delta":{"content":"the update."}}]}`+"\n\n",
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n",
		`data: {"choices":[],"usage":{"prompt_tokens":42,"completion_tokens":9}}`+"\n\n",
		"data: [DONE]\n\n",
	)
	defer srv.Close()

	p := NewOpenAICompatible(srv.Client(), srv.URL+"/v1", "", "qwen2.5")
	deltas, err := p.Stream(context.Background(), Request{System: "sys", User: "text"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	text, usage, streamErr := collect(t, deltas)
	if streamErr != nil {
		t.Fatalf("stream error: %v", streamErr)
	}
	if want := "Thanks for the update."; text != want {
		t.Errorf("got %q, want %q", text, want)
	}
	if usage == nil || usage.InputTokens != 42 || usage.OutputTokens != 9 {
		t.Errorf("got usage %+v, want {42 9}", usage)
	}
}

// [DONE] is not JSON, so it has to be recognised before any parse attempt.
func TestOpenAITerminatesOnDoneSentinel(t *testing.T) {
	srv := sseServer(t,
		`data: {"choices":[{"delta":{"content":"hello"}}]}`+"\n\n",
		"data: [DONE]\n\n",
		// Anything after the sentinel must not be read as output.
		`data: {"choices":[{"delta":{"content":" WORLD"}}]}`+"\n\n",
	)
	defer srv.Close()

	p := NewOpenAICompatible(srv.Client(), srv.URL+"/v1", "", "m")
	deltas, err := p.Stream(context.Background(), Request{User: "x"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	text, _, _ := collect(t, deltas)
	if text != "hello" {
		t.Errorf("got %q, want %q", text, "hello")
	}
}

// A local endpoint that just closes the connection has produced a complete
// response, not a failure.
func TestOpenAIEndOfStreamWithoutSentinel(t *testing.T) {
	srv := sseServer(t, `data: {"choices":[{"delta":{"content":"done"}}]}`+"\n\n")
	defer srv.Close()

	p := NewOpenAICompatible(srv.Client(), srv.URL+"/v1", "", "m")
	deltas, err := p.Stream(context.Background(), Request{User: "x"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	text, _, streamErr := collect(t, deltas)
	if streamErr != nil {
		t.Errorf("unexpected error: %v", streamErr)
	}
	if text != "done" {
		t.Errorf("got %q", text)
	}
}

func TestOpenAIOmitsAuthorizationWhenNoKey(t *testing.T) {
	var hadAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadAuth = r.Header["Authorization"]
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	// Local model servers commonly reject an empty bearer token outright.
	p := NewOpenAICompatible(srv.Client(), srv.URL+"/v1", "", "m")
	deltas, _ := p.Stream(context.Background(), Request{User: "x"})
	collect(t, deltas)

	if hadAuth {
		t.Error("sent an Authorization header with no key configured")
	}
}

func TestErrorClassification(t *testing.T) {
	tests := []struct {
		status    int
		body      string
		wantKind  Kind
		retryable bool
	}{
		{401, `{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`, KindAuth, false},
		{403, `{"error":{"message":"forbidden"}}`, KindAuth, false},
		{404, `{"error":{"message":"model: nope"}}`, KindModel, false},
		{413, `{"error":{"message":"too big"}}`, KindTooLarge, false},
		{429, `{"error":{"message":"slow down"}}`, KindRateLimit, true},
		{500, `{"error":{"message":"boom"}}`, KindUpstream, true},
		{529, `{"error":{"message":"overloaded"}}`, KindOverloaded, true},
		{400, `{"error":{"message":"bad field"}}`, KindRequest, false},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()

			p := NewAnthropic(srv.Client(), srv.URL, "k", "m")
			_, err := p.Stream(context.Background(), Request{User: "x"})
			if err == nil {
				t.Fatal("expected an error")
			}

			var provErr *Error
			if !errors.As(err, &provErr) {
				t.Fatalf("got %T, want *Error", err)
			}
			if provErr.Kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", provErr.Kind, tt.wantKind)
			}
			if provErr.Retryable != tt.retryable {
				t.Errorf("retryable = %v, want %v", provErr.Retryable, tt.retryable)
			}
			if provErr.Message == "" {
				t.Error("empty message — the overlay would show nothing")
			}
		})
	}
}

// The overlay shows Message verbatim, so a leaked key would be on screen and,
// worse, in whatever the user pastes into a bug report.
func TestErrorMessagesNeverContainTheKey(t *testing.T) {
	const key = "sk-ant-SUPERSECRET-abc123"

	for _, status := range []int{400, 401, 403, 404, 429, 500, 529} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			// A provider echoing the key back is the nightmare case.
			fmt.Fprintf(w, `{"error":{"message":"rejected key %s"}}`, key)
		}))

		p := NewAnthropic(srv.Client(), srv.URL, key, "m")
		_, err := p.Stream(context.Background(), Request{User: "x"})
		srv.Close()

		if err == nil {
			t.Fatalf("status %d: expected an error", status)
		}
		if strings.Contains(err.Error(), key) {
			t.Errorf("status %d: error text contains the API key: %s", status, err.Error())
		}
	}
}

func TestUnreachableEndpoint(t *testing.T) {
	// Closed immediately, so the port is almost certainly not listening.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	p := NewOpenAICompatible(NewHTTPClient(), url+"/v1", "", "m")
	_, err := p.Stream(context.Background(), Request{User: "x"})
	if err == nil {
		t.Fatal("expected an error")
	}

	var provErr *Error
	if !errors.As(err, &provErr) {
		t.Fatalf("got %T, want *Error", err)
	}
	if provErr.Kind != KindUnreachable {
		t.Errorf("kind = %q, want %q", provErr.Kind, KindUnreachable)
	}
	// Someone running Ollama needs to be told the server is not up, not shown
	// a wrapped syscall error.
	if !strings.Contains(provErr.Message, "Is it running") {
		t.Errorf("unhelpful message: %q", provErr.Message)
	}
}

// The contract requires that Escape actually stops the upstream generation,
// not merely stops displaying it. Otherwise the user keeps paying for tokens
// they cancelled.
func TestCancellationClosesTheUpstreamRequest(t *testing.T) {
	var (
		bodyClosed atomic.Bool
		serverDone = make(chan struct{})
		sentFirst  = make(chan struct{})
		closeOnce  atomic.Bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(serverDone)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)

		fmt.Fprint(w, "event: content_block_delta\n"+
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"first"}}`+"\n\n")
		flusher.Flush()
		if closeOnce.CompareAndSwap(false, true) {
			close(sentFirst)
		}

		// Keep generating until the client goes away. This is the provider
		// burning the user's tokens.
		for {
			select {
			case <-r.Context().Done():
				bodyClosed.Store(true)
				return
			case <-time.After(5 * time.Millisecond):
				fmt.Fprint(w, "event: content_block_delta\n"+
					`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"x"}}`+"\n\n")
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	p := NewAnthropic(srv.Client(), srv.URL, "k", "m")
	deltas, err := p.Stream(ctx, Request{User: "x"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	<-sentFirst
	cancel()

	// The channel must close rather than block forever.
	select {
	case <-drained(deltas):
	case <-time.After(2 * time.Second):
		t.Fatal("delta channel did not close after cancellation")
	}

	// And the server must observe the disconnect.
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request was not aborted — the generation is still being billed")
	}
	if !bodyClosed.Load() {
		t.Error("server did not see the client disconnect")
	}
}

func drained(ch <-chan Delta) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()
	return done
}

// An error arriving mid-stream, after a 200 has already been sent. Clients
// that only check the status code miss these entirely.
func TestInBandStreamError(t *testing.T) {
	srv := sseServer(t,
		"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"partial"}}`+"\n\n",
		"event: error\n"+
			`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`+"\n\n",
	)
	defer srv.Close()

	p := NewAnthropic(srv.Client(), srv.URL, "k", "m")
	deltas, err := p.Stream(context.Background(), Request{User: "x"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	text, _, streamErr := collect(t, deltas)
	if text != "partial" {
		t.Errorf("got %q, want the text received before the error", text)
	}
	if streamErr == nil {
		t.Fatal("in-band error was swallowed")
	}
	var provErr *Error
	if !errors.As(streamErr, &provErr) || provErr.Kind != KindOverloaded {
		t.Errorf("got %v, want an overloaded error", streamErr)
	}
}

// A garbled event should cost that event, not the whole rewrite.
func TestMalformedEventIsSkipped(t *testing.T) {
	srv := sseServer(t,
		"data: {not json at all\n\n",
		"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"survived"}}`+"\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	defer srv.Close()

	p := NewAnthropic(srv.Client(), srv.URL, "k", "m")
	deltas, _ := p.Stream(context.Background(), Request{User: "x"})
	text, _, streamErr := collect(t, deltas)

	if streamErr != nil {
		t.Errorf("unexpected error: %v", streamErr)
	}
	if text != "survived" {
		t.Errorf("got %q, want %q", text, "survived")
	}
}

// The in-band path reaches the screen too, and the decoder does not hold the
// key — so the pattern pass has to cover it.
func TestInBandErrorMessagesAreScrubbed(t *testing.T) {
	srv := sseServer(t,
		"event: error\n"+
			`data: {"type":"error","error":{"type":"invalid_request_error","message":"bad key sk-ant-LEAKED-9f8e7d6c5b4a"}}`+"\n\n",
	)
	defer srv.Close()

	p := NewAnthropic(srv.Client(), srv.URL, "unrelated-key", "m")
	deltas, err := p.Stream(context.Background(), Request{User: "x"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	_, _, streamErr := collect(t, deltas)
	if streamErr == nil {
		t.Fatal("expected an in-band error")
	}
	if strings.Contains(streamErr.Error(), "LEAKED") {
		t.Errorf("key-shaped token survived into a user-facing message: %s", streamErr.Error())
	}
}
