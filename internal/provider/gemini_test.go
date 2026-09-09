package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func geminiDelta(text string) string {
	return fmt.Sprintf(
		`data: {"candidates":[{"content":{"parts":[{"text":%q}],"role":"model"}}]}`+"\n\n", text)
}

const geminiStop = `data: {"candidates":[{"content":{"parts":[{"text":""}]},"finishReason":"STOP"}],` +
	`"usageMetadata":{"promptTokenCount":31,"candidatesTokenCount":17}}` + "\n\n"

func TestGeminiStreamsText(t *testing.T) {
	srv := sseServer(t, geminiDelta("Thanks for "), geminiDelta("the update."), geminiStop)
	defer srv.Close()

	p := NewGemini(srv.Client(), srv.URL, "test-key", "gemini-flash-latest")
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
	if usage == nil || usage.InputTokens != 31 || usage.OutputTokens != 17 {
		t.Errorf("got usage %+v, want {31 17}", usage)
	}
}

// Google's docs describe two shapes for the same counts. Accepting only one
// silently reports zero tokens against the other.
func TestGeminiAcceptsBothUsageShapes(t *testing.T) {
	tests := []struct {
		name  string
		usage string
	}{
		{"REST reference shape", `{"promptTokenCount":31,"candidatesTokenCount":17}`},
		{"thinking guide shape", `{"total_input_tokens":31,"total_output_tokens":17}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stop := fmt.Sprintf(
				`data: {"candidates":[{"content":{"parts":[{"text":""}]},"finishReason":"STOP"}],"usageMetadata":%s}`+"\n\n",
				tt.usage)
			srv := sseServer(t, geminiDelta("hi"), stop)
			defer srv.Close()

			p := NewGemini(srv.Client(), srv.URL, "k", "m")
			deltas, _ := p.Stream(context.Background(), Request{User: "x"})
			_, usage, _ := collect(t, deltas)

			if usage == nil || usage.InputTokens != 31 || usage.OutputTokens != 17 {
				t.Errorf("got %+v, want {31 17}", usage)
			}
		})
	}
}

// The trap that matters. Gemini's reasoning arrives in the ordinary parts
// array, so emitting it would paste the model's thinking into the document.
// The two markers come from two different Google docs; both must be honoured.
func TestGeminiDropsThoughtParts(t *testing.T) {
	tests := []struct {
		name  string
		parts string
	}{
		{
			name:  "thought boolean, per the REST reference",
			parts: `[{"text":"Let me consider the tone...","thought":true},{"text":"Thanks."}]`,
		},
		{
			name:  "type field, per the thinking guide",
			parts: `[{"text":"Let me consider the tone...","type":"thought"},{"text":"Thanks."}]`,
		},
		{
			name:  "a thought part after the answer",
			parts: `[{"text":"Thanks."},{"text":"On reflection...","thought":true}]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := sseServer(t,
				fmt.Sprintf(`data: {"candidates":[{"content":{"parts":%s}}]}`+"\n\n", tt.parts),
				geminiStop,
			)
			defer srv.Close()

			p := NewGemini(srv.Client(), srv.URL, "k", "m")
			deltas, _ := p.Stream(context.Background(), Request{User: "x"})
			text, _, _ := collect(t, deltas)

			if text != "Thanks." {
				t.Errorf("got %q, want %q — reasoning must never reach the document", text, "Thanks.")
			}
		})
	}
}

func TestGeminiRequestShape(t *testing.T) {
	var gotPath, gotQuery, gotKey, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotKey = r.Header.Get("X-Goog-Api-Key")
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, geminiStop)
	}))
	defer srv.Close()

	p := NewGemini(srv.Client(), srv.URL+"/v1beta", "sk-goog", "gemini-flash-latest")
	deltas, err := p.Stream(context.Background(), Request{System: "be concise", User: "hello"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	collect(t, deltas)

	if want := "/v1beta/models/gemini-flash-latest:streamGenerateContent"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	// Without alt=sse the endpoint streams a JSON array, not events.
	if gotQuery != "alt=sse" {
		t.Errorf("query = %q, want alt=sse", gotQuery)
	}
	if gotKey != "sk-goog" {
		t.Errorf("X-Goog-Api-Key = %q", gotKey)
	}
	// A real system instruction, not a system message.
	if !strings.Contains(gotBody, `"systemInstruction"`) {
		t.Errorf("body carries no systemInstruction: %s", gotBody)
	}
	if !strings.Contains(gotBody, "be concise") {
		t.Errorf("system text missing from body: %s", gotBody)
	}
}

// The key must never reach the query string, where it would end up in proxy
// logs and shell history.
func TestGeminiKeyIsNotInTheURL(t *testing.T) {
	var rawURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawURL = r.URL.String()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, geminiStop)
	}))
	defer srv.Close()

	const key = "AIzaSy-SECRET-key-value"
	p := NewGemini(srv.Client(), srv.URL, key, "m")
	deltas, _ := p.Stream(context.Background(), Request{User: "x"})
	collect(t, deltas)

	if strings.Contains(rawURL, key) {
		t.Errorf("the API key is in the request URL: %s", rawURL)
	}
}

func TestGeminiModelNameAcceptsBothForms(t *testing.T) {
	for _, model := range []string{"gemini-flash-latest", "models/gemini-flash-latest"} {
		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, geminiStop)
		}))

		p := NewGemini(srv.Client(), srv.URL+"/v1beta", "k", model)
		deltas, _ := p.Stream(context.Background(), Request{User: "x"})
		collect(t, deltas)
		srv.Close()

		if want := "/v1beta/models/gemini-flash-latest:streamGenerateContent"; gotPath != want {
			t.Errorf("model %q gave path %q, want %q", model, gotPath, want)
		}
	}
}

// A generation cut short by a safety stop is half a sentence. Reporting it as
// success would offer the user that half sentence to paste over their text.
func TestGeminiSafetyStopIsAnError(t *testing.T) {
	srv := sseServer(t,
		geminiDelta("Thanks for the "),
		`data: {"candidates":[{"content":{"parts":[]},"finishReason":"SAFETY"}]}`+"\n\n",
	)
	defer srv.Close()

	p := NewGemini(srv.Client(), srv.URL, "k", "m")
	deltas, _ := p.Stream(context.Background(), Request{User: "x"})
	_, _, streamErr := collect(t, deltas)

	if streamErr == nil {
		t.Fatal("a safety stop was reported as success")
	}
	if !strings.Contains(streamErr.Error(), "safety") {
		t.Errorf("message does not say why: %v", streamErr)
	}
}

// A truncated rewrite is still a rewrite; the overlay shows it and the user
// decides. Discarding it would be worse than showing it.
func TestGeminiMaxTokensIsNotAnError(t *testing.T) {
	srv := sseServer(t,
		geminiDelta("Thanks for the upd"),
		`data: {"candidates":[{"content":{"parts":[]},"finishReason":"MAX_TOKENS"}]}`+"\n\n",
	)
	defer srv.Close()

	p := NewGemini(srv.Client(), srv.URL, "k", "m")
	deltas, _ := p.Stream(context.Background(), Request{User: "x"})
	text, _, streamErr := collect(t, deltas)

	if streamErr != nil {
		t.Errorf("truncation reported as failure: %v", streamErr)
	}
	if text != "Thanks for the upd" {
		t.Errorf("got %q", text)
	}
}

func TestGeminiBlockedPrompt(t *testing.T) {
	srv := sseServer(t, `data: {"promptFeedback":{"blockReason":"SAFETY"}}`+"\n\n")
	defer srv.Close()

	p := NewGemini(srv.Client(), srv.URL, "k", "m")
	deltas, _ := p.Stream(context.Background(), Request{User: "x"})
	_, _, streamErr := collect(t, deltas)

	if streamErr == nil {
		t.Fatal("a blocked prompt was reported as success")
	}
	var provErr *Error
	if !errors.As(streamErr, &provErr) || provErr.Kind != KindRequest {
		t.Errorf("got %v", streamErr)
	}
}

func TestGeminiErrorStatusMapping(t *testing.T) {
	tests := []struct {
		status   int
		body     string
		wantKind Kind
	}{
		{401, `{"error":{"code":401,"status":"UNAUTHENTICATED","message":"API key not valid"}}`, KindAuth},
		{403, `{"error":{"code":403,"status":"PERMISSION_DENIED","message":"denied"}}`, KindAuth},
		{404, `{"error":{"code":404,"status":"NOT_FOUND","message":"model not found"}}`, KindModel},
		{429, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"quota"}}`, KindRateLimit},
		{400, `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"bad"}}`, KindRequest},
		{503, `{"error":{"code":503,"status":"UNAVAILABLE","message":"overloaded"}}`, KindOverloaded},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()

			p := NewGemini(srv.Client(), srv.URL, "k", "m")
			_, err := p.Stream(context.Background(), Request{User: "x"})
			if err == nil {
				t.Fatal("expected an error")
			}
			var provErr *Error
			if !errors.As(err, &provErr) {
				t.Fatalf("got %T", err)
			}
			if provErr.Kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", provErr.Kind, tt.wantKind)
			}
			if provErr.Message == "" {
				t.Error("empty message")
			}
		})
	}
}

func TestGeminiErrorMessagesNeverContainTheKey(t *testing.T) {
	const key = "AIzaSy-SUPERSECRET-abc123def456"

	for _, status := range []int{400, 401, 403, 404, 429, 500} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"error":{"message":"API key not valid: %s"}}`, key)
		}))

		p := NewGemini(srv.Client(), srv.URL, key, "m")
		_, err := p.Stream(context.Background(), Request{User: "x"})
		srv.Close()

		if err == nil {
			t.Fatalf("status %d: expected an error", status)
		}
		if strings.Contains(err.Error(), key) {
			t.Errorf("status %d: the API key leaked: %s", status, err.Error())
		}
	}
}

// Every example in Google's docs is a complete request URL, so that is what
// people paste into an endpoint field. Appending our own path to it produced a
// 404 that surfaced as "does not recognise that model" — a real report.
func TestNormalizeGeminiBaseURL(t *testing.T) {
	const root = "https://generativelanguage.googleapis.com/v1beta"

	tests := []struct{ name, in, want string }{
		{"already the root", root, root},
		{"trailing slash", root + "/", root},
		{
			name: "the full URL from the docs",
			in:   root + "/models/gemini-flash-latest:generateContent",
			want: root,
		},
		{
			name: "the streaming URL",
			in:   root + "/models/gemini-flash-latest:streamGenerateContent",
			want: root,
		},
		{
			name: "with alt=sse already on it",
			in:   root + "/models/gemini-2.5-flash:streamGenerateContent?alt=sse",
			want: root,
		},
		{
			// Some examples authenticate this way. The key must not survive
			// into a stored setting.
			name: "a key in the query string is dropped",
			in:   root + "/models/gemini-flash-latest:generateContent?key=AIzaSy-SECRET",
			want: root,
		},
		{
			name: "a bare host gains the version this client speaks",
			in:   "https://generativelanguage.googleapis.com",
			want: root,
		},
		{"a v1 root is left alone", "https://example.test/v1", "https://example.test/v1"},
		{"whitespace", "  " + root + "  ", root},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeGeminiBaseURL(tt.in); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// The end-to-end version: a pasted full URL must produce a working request.
func TestGeminiAcceptsAPastedFullURL(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, geminiStop)
	}))
	defer srv.Close()

	pasted := srv.URL + "/v1beta/models/gemini-flash-latest:generateContent"
	p := NewGemini(srv.Client(), pasted, "k", "gemini-flash-latest")
	deltas, err := p.Stream(context.Background(), Request{User: "x"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	collect(t, deltas)

	if want := "/v1beta/models/gemini-flash-latest:streamGenerateContent"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if gotQuery != "alt=sse" {
		t.Errorf("query = %q", gotQuery)
	}
}

// The key must not be smuggled in through the endpoint field either.
func TestGeminiDropsAKeyPastedInTheEndpoint(t *testing.T) {
	const leaked = "AIzaSy-FROM-THE-URL"
	var rawURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawURL = r.URL.String()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, geminiStop)
	}))
	defer srv.Close()

	p := NewGemini(srv.Client(), srv.URL+"/v1beta/models/m:generateContent?key="+leaked, "real-key", "m")
	deltas, _ := p.Stream(context.Background(), Request{User: "x"})
	collect(t, deltas)

	if strings.Contains(rawURL, leaked) {
		t.Errorf("a key pasted into the endpoint field reached the request URL: %s", rawURL)
	}
}

// Rewriting a sentence is not a reasoning task, and current Gemini models
// think by default — the difference between half a second and two.
func TestGeminiAsksForTheLowestThinkingLevel(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, geminiStop)
	}))
	defer srv.Close()

	p := NewGemini(srv.Client(), srv.URL+"/v1beta", "k", "gemini-flash-latest")
	deltas, _ := p.Stream(context.Background(), Request{User: "x"})
	collect(t, deltas)

	if !strings.Contains(gotBody, `"thinkingLevel":"low"`) {
		t.Errorf("no thinkingLevel in the request: %s", gotBody)
	}
}

// Older models take thinkingConfig.thinkingBudget and reject thinkingLevel
// outright. Rather than keep a model table that would rot, ask once and
// remember — but the first rewrite must still succeed.
func TestGeminiFallsBackWhenThinkingLevelIsRejected(t *testing.T) {
	var attempts int
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		bodies = append(bodies, string(buf))

		if strings.Contains(bodies[len(bodies)-1], "thinkingLevel") {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"status":"INVALID_ARGUMENT",`+
				`"message":"Unknown name \"thinkingLevel\" at 'generation_config'"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, geminiDelta("Thanks."), geminiStop)
	}))
	defer srv.Close()

	p := NewGemini(srv.Client(), srv.URL+"/v1beta", "k", "old-model")

	deltas, err := p.Stream(context.Background(), Request{User: "x"})
	if err != nil {
		t.Fatalf("first rewrite failed instead of falling back: %v", err)
	}
	text, _, _ := collect(t, deltas)
	if text != "Thanks." {
		t.Errorf("got %q", text)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (rejected, then retried without)", attempts)
	}

	// The cost of discovering this is paid once, not per rewrite.
	deltas, err = p.Stream(context.Background(), Request{User: "y"})
	if err != nil {
		t.Fatalf("second rewrite: %v", err)
	}
	collect(t, deltas)
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3 — the rejection should be remembered", attempts)
	}
	if strings.Contains(bodies[len(bodies)-1], "thinkingLevel") {
		t.Error("still sending thinkingLevel after it was rejected")
	}
}

// A rejection for some other reason must not be mistaken for the field being
// unsupported, or every bad request silently doubles.
func TestGeminiDoesNotRetryUnrelatedRejections(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"status":"INVALID_ARGUMENT","message":"contents is required"}}`)
	}))
	defer srv.Close()

	p := NewGemini(srv.Client(), srv.URL+"/v1beta", "k", "m")
	if _, err := p.Stream(context.Background(), Request{User: "x"}); err == nil {
		t.Fatal("expected an error")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
}

func TestGeminiReportsThoughtTokens(t *testing.T) {
	for _, field := range []string{"thoughtsTokenCount", "total_thought_tokens"} {
		stop := fmt.Sprintf(
			`data: {"candidates":[{"content":{"parts":[{"text":""}]},"finishReason":"STOP"}],`+
				`"usageMetadata":{"promptTokenCount":31,"candidatesTokenCount":17,%q:412}}`+"\n\n", field)
		srv := sseServer(t, geminiDelta("hi"), stop)

		p := NewGemini(srv.Client(), srv.URL+"/v1beta", "k", "m")
		deltas, _ := p.Stream(context.Background(), Request{User: "x"})
		_, usage, _ := collect(t, deltas)
		srv.Close()

		if usage == nil || usage.ThoughtTokens != 412 {
			t.Errorf("%s: thought tokens = %+v, want 412", field, usage)
		}
	}
}
