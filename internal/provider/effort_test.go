package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Each provider spells the same setting differently, and getting one of them
// wrong is invisible: the request succeeds, the model thinks at its own
// default, and the only symptom is a rewrite that takes three seconds instead
// of one. These pin the field name per provider.

// bodyRecorder serves the given events and records the request body.
func bodyRecorder(t *testing.T, events ...string) (*httptest.Server, *string) {
	t.Helper()
	got := new(string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		*got = string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			fmt.Fprint(w, e)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func TestEachProviderSendsEffortInItsOwnField(t *testing.T) {
	tests := []struct {
		name   string
		build  func(client *http.Client, url string) Provider
		events []string
		want   string
	}{
		{
			name: "anthropic uses output_config.effort",
			build: func(c *http.Client, url string) Provider {
				return NewAnthropic(c, url, "k", "claude-sonnet-5")
			},
			events: []string{anthropicStopEvent},
			want:   `"output_config":{"effort":"medium"}`,
		},
		{
			name: "gemini uses generationConfig.thinkingLevel",
			build: func(c *http.Client, url string) Provider {
				return NewGemini(c, url+"/v1beta", "k", "gemini-flash-latest")
			},
			events: []string{geminiStop},
			want:   `"thinkingLevel":"medium"`,
		},
		{
			name: "openai-compatible uses reasoning_effort",
			build: func(c *http.Client, url string) Provider {
				return NewOpenAICompatible(c, url+"/v1", "k", "gpt-5.6-luna")
			},
			events: []string{"data: [DONE]\n\n"},
			want:   `"reasoning_effort":"medium"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, got := bodyRecorder(t, tt.events...)

			p := tt.build(srv.Client(), srv.URL)
			deltas, err := p.Stream(context.Background(), Request{User: "x", Effort: "medium"})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			collect(t, deltas)

			if !strings.Contains(*got, tt.want) {
				t.Errorf("request body does not carry %s:\n%s", tt.want, *got)
			}
		})
	}
}

// A user who never touched the control must not have a new field appear in
// their requests: an endpoint that predates it would reject the lot, turning a
// working setup into a broken one on upgrade. Gemini is the exception and has
// its own test — those models think by default, so the floor is always asked
// for.
func TestNoEffortMeansNoField(t *testing.T) {
	tests := []struct {
		name     string
		build    func(client *http.Client, url string) Provider
		events   []string
		unwanted string
	}{
		{
			name: "anthropic",
			build: func(c *http.Client, url string) Provider {
				return NewAnthropic(c, url, "k", "claude-haiku-4-5")
			},
			events:   []string{anthropicStopEvent},
			unwanted: "output_config",
		},
		{
			name: "openai-compatible",
			build: func(c *http.Client, url string) Provider {
				return NewOpenAICompatible(c, url+"/v1", "k", "local-model")
			},
			events:   []string{"data: [DONE]\n\n"},
			unwanted: "reasoning_effort",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, got := bodyRecorder(t, tt.events...)

			p := tt.build(srv.Client(), srv.URL)
			deltas, err := p.Stream(context.Background(), Request{User: "x"})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			collect(t, deltas)

			if strings.Contains(*got, tt.unwanted) {
				t.Errorf("sent %s without being asked to:\n%s", tt.unwanted, *got)
			}
		})
	}
}

// The session's level has to win over the built-in floor, or the picker is
// decoration.
func TestGeminiPrefersTheSessionLevelOverItsFloor(t *testing.T) {
	srv, got := bodyRecorder(t, geminiStop)

	p := NewGemini(srv.Client(), srv.URL+"/v1beta", "k", "gemini-3.6-flash")
	deltas, err := p.Stream(context.Background(), Request{User: "x", Effort: "minimal"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	collect(t, deltas)

	if !strings.Contains(*got, `"thinkingLevel":"minimal"`) {
		t.Errorf("the session level did not reach the request:\n%s", *got)
	}
}

const anthropicStopEvent = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
