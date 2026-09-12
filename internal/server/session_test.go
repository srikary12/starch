package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// startSession posts a session and returns the response body.
func startSession(t *testing.T, c *http.Client, req sessionRequest) (*http.Response, sessionResponse) {
	t.Helper()
	body, _ := json.Marshal(req)
	resp := doBody(t, c, http.MethodPost, "/v1/session", testToken, body)
	t.Cleanup(func() { resp.Body.Close() })

	var decoded sessionResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
			t.Fatalf("decoding session response: %v", err)
		}
	}
	return resp, decoded
}

// The whole path: a level chosen in a settings window has to survive the
// session and land in the upstream request for every rewrite that follows.
func TestThinkingEffortReachesTheProvider(t *testing.T) {
	var gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		for _, e := range []string{anthropicDelta("Thanks."), anthropicStop} {
			w.Write([]byte(e))
		}
	}))
	defer up.Close()

	c, _, _, _ := startDaemon(t, Options{})
	resp, decoded := startSession(t, c, sessionRequest{
		Provider:       ProviderAnthropic,
		Model:          "claude-sonnet-5",
		BaseURL:        up.URL,
		APIKey:         "sk-test-key",
		ThinkingEffort: "high",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session status = %d", resp.StatusCode)
	}
	// Echoed back so a shell can confirm what the daemon accepted, the same
	// way it confirms the resolved endpoint.
	if decoded.ThinkingEffort != "high" {
		t.Errorf("echoed effort = %q, want high", decoded.ThinkingEffort)
	}

	body, _ := json.Marshal(rewriteRequest{Text: "hello", Preset: "concise"})
	rewrite := doBody(t, c, http.MethodPost, "/v1/rewrite", testToken, body)
	defer rewrite.Body.Close()
	readFrames(t, rewrite.Body)

	if !strings.Contains(gotBody, `"effort":"high"`) {
		t.Errorf("the session's effort never reached the provider:\n%s", gotBody)
	}
}

// A session that names no level must not start sending one. Anything else
// would change every existing user's requests on upgrade.
func TestNoThinkingEffortSendsNone(t *testing.T) {
	var gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(anthropicStop))
	}))
	defer up.Close()

	c, _, _, _ := startDaemon(t, Options{})
	configureSession(t, c, up.URL)

	body, _ := json.Marshal(rewriteRequest{Text: "hello"})
	resp := doBody(t, c, http.MethodPost, "/v1/rewrite", testToken, body)
	defer resp.Body.Close()
	readFrames(t, resp.Body)

	if strings.Contains(gotBody, "output_config") {
		t.Errorf("an unasked-for effort appeared in the request:\n%s", gotBody)
	}
}

func TestSessionRejectsAnUnusableEffort(t *testing.T) {
	tests := []struct {
		name   string
		effort string
		want   int
	}{
		{"empty is fine", "", http.StatusOK},
		{"a known level", "low", http.StatusOK},
		// Accepted on purpose: the catalog is the authority on which levels a
		// model takes, and it changes faster than this daemon is rebuilt.
		{"a level this build has never heard of", "ludicrous", http.StatusOK},
		{"whitespace is trimmed, not rejected", "  low  ", http.StatusOK},
		{"uppercase", "LOW", http.StatusBadRequest},
		{"punctuation", "low;drop", http.StatusBadRequest},
		{"a whole sentence", strings.Repeat("a", 17), http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _, _, _ := startDaemon(t, Options{})
			resp, _ := startSession(t, c, sessionRequest{
				Provider:       ProviderAnthropic,
				Model:          "claude-sonnet-5",
				APIKey:         "sk-test-key",
				ThinkingEffort: tt.effort,
			})
			if resp.StatusCode != tt.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}
}
