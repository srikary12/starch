// Package provider talks to model endpoints and turns their streams into a
// single shape the rest of the daemon can consume.
//
// Two implementations cover nearly everything people use: Anthropic's Messages
// API, and anything that speaks OpenAI-compatible chat completions — OpenAI
// itself, OpenRouter, Groq, Together, Ollama and LM Studio. Local models are
// not an afterthought here: they are the answer for anyone who will not send
// work messages to a third party.
//
// Nothing in this package knows about macOS, and nothing in it writes to disk.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Provider streams a rewrite from a model endpoint.
type Provider interface {
	// Stream begins a generation. The returned channel is closed after a
	// delta with Done or Err set; callers must drain it or cancel ctx.
	//
	// Cancelling ctx must abort the upstream HTTP request, not merely stop
	// reading from it. The user pressing Escape should stop the generation
	// being billed, not just stop it being displayed.
	Stream(ctx context.Context, req Request) (<-chan Delta, error)

	// Name identifies the provider in logs. Never includes credentials.
	Name() string
}

// Request is one rewrite, already assembled into a prompt.
type Request struct {
	// System is the instruction block. Sent as a top-level system prompt where
	// the provider has one, and as a leading system message where it does not.
	System string
	// User is the text to rewrite, plus any assembled few-shot context.
	User string
	// Model overrides the session's configured model when non-empty.
	Model string
	// MaxTokens bounds the reply. Zero means DefaultMaxTokens.
	MaxTokens int
}

// DefaultMaxTokens is deliberately modest: this rewrites a selection, and a
// reply longer than the input is nearly always the model having misunderstood
// the task rather than something worth waiting for.
const DefaultMaxTokens = 2048

// Usage reports token counts when the provider gives them.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Delta is one increment of a stream. Exactly one delta with Done or Err set
// arrives, last, before the channel closes.
type Delta struct {
	// Text is a chunk of generated output. Empty on a terminal delta.
	Text string
	// Done marks a successful end of stream.
	Done bool
	// Usage is set on the terminal delta when the provider reported it.
	Usage *Usage
	// Err is set if the stream failed. Terminal.
	Err error
}

// Kind classifies a failure so the shell can react without string matching.
// These values appear on the wire; see api/README.md.
type Kind string

const (
	// KindAuth: the key was rejected. Not retryable without user action.
	KindAuth Kind = "provider_auth"
	// KindRateLimit: too many requests. Retryable after a wait.
	KindRateLimit Kind = "rate_limited"
	// KindOverloaded: the provider is busy. Retryable.
	KindOverloaded Kind = "provider_overloaded"
	// KindModel: the model name was not recognised by the endpoint.
	KindModel Kind = "model_not_found"
	// KindTooLarge: the selection exceeds what the model will accept.
	KindTooLarge Kind = "selection_too_large"
	// KindRequest: the daemon built a request the provider rejected. A bug
	// here is ours, not the user's.
	KindRequest Kind = "provider_request"
	// KindUnreachable: could not connect. Usually a local endpoint that is
	// not running, or no network.
	KindUnreachable Kind = "provider_unreachable"
	// KindUpstream: the provider failed in a way we cannot classify.
	KindUpstream Kind = "provider_error"
)

// Error is a provider failure with a message fit to show a user.
//
// The Message field is displayed in the overlay, so it must be readable and
// must never contain the API key, the request body, or the user's text.
type Error struct {
	Kind Kind
	// Message is shown to the user. Plain language, actionable where possible.
	Message string
	// Status is the HTTP status, or zero if the failure was not an HTTP reply.
	Status int
	// Retryable reports whether trying again unchanged could plausibly work.
	Retryable bool
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s (%d): %s", e.Kind, e.Status, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}

// classify maps an HTTP status onto a user-facing error.
//
// detail is the provider's own message where one could be parsed out. It is
// appended only for statuses where the provider's wording adds something a
// user can act on — for an auth failure "invalid x-api-key" tells them nothing
// they do not already know from the fact that it failed.
func classify(status int, providerName, detail string) *Error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &Error{
			Kind:    KindAuth,
			Status:  status,
			Message: fmt.Sprintf("%s rejected your API key. Check it in Settings.", providerName),
		}
	case status == http.StatusNotFound:
		return &Error{
			Kind:    KindModel,
			Status:  status,
			Message: withDetail(fmt.Sprintf("%s does not recognise that model.", providerName), detail),
		}
	case status == http.StatusRequestEntityTooLarge:
		return &Error{
			Kind:    KindTooLarge,
			Status:  status,
			Message: "That selection is too long for this model.",
		}
	case status == http.StatusTooManyRequests:
		return &Error{
			Kind:      KindRateLimit,
			Status:    status,
			Retryable: true,
			Message:   fmt.Sprintf("%s is rate limiting you. Wait a moment and try again.", providerName),
		}
	case status == http.StatusServiceUnavailable || status == 529:
		// 503 is the standard "come back later"; 529 is Anthropic's own
		// overloaded status and is not in net/http's constants. Both mean the
		// same thing to a user, and "overloaded" is more accurate than "server
		// error" for either.
		return &Error{
			Kind:      KindOverloaded,
			Status:    status,
			Retryable: true,
			Message:   fmt.Sprintf("%s is overloaded. Try again in a moment.", providerName),
		}
	case status >= 500:
		return &Error{
			Kind:      KindUpstream,
			Status:    status,
			Retryable: true,
			Message:   withDetail(fmt.Sprintf("%s had a server error.", providerName), detail),
		}
	case status >= 400:
		return &Error{
			Kind:    KindRequest,
			Status:  status,
			Message: withDetail(fmt.Sprintf("%s rejected the request.", providerName), detail),
		}
	}
	return nil
}

func withDetail(base, detail string) string {
	if detail == "" {
		return base
	}
	return base + " " + detail
}

// NewHTTPClient builds the client every provider shares.
//
// This is the reason the daemon is worth having. A long-lived process keeps
// connections warm, so every rewrite after the first skips the TLS handshake —
// 100-200ms, a real fraction of a 500ms budget. Getting these settings wrong
// silently gives that back: Go's default MaxIdleConnsPerHost is 2, and the
// default IdleConnTimeout will drop a connection between two rewrites a minute
// apart, which is exactly the usage pattern here.
func NewHTTPClient() *http.Client {
	return &http.Client{
		// No Client.Timeout: it bounds the whole exchange including the body,
		// which for a stream means killing a long generation mid-flight.
		// Deadlines belong on the context. ResponseHeaderTimeout below bounds
		// the part that actually needs bounding.
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:   true,
			MaxIdleConns:        32,
			MaxIdleConnsPerHost: 8,
			// Long enough to still be warm for the next rewrite in a session.
			IdleConnTimeout:       5 * time.Minute,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			// Bounds time-to-first-byte without touching the stream body.
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}
}

// wrapTransportError turns a connection-level failure into something a user
// can act on. "connection refused" against localhost almost always means the
// local model server is not running, which is worth saying outright.
// Callers must handle context cancellation before calling this — a cancelled
// request is the user pressing Escape, not a failure worth reporting.
func wrapTransportError(err error, host string) *Error {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &Error{
			Kind:      KindUnreachable,
			Retryable: true,
			Message:   fmt.Sprintf("Timed out reaching %s.", host),
		}
	}
	return &Error{
		Kind:      KindUnreachable,
		Retryable: true,
		Message:   fmt.Sprintf("Could not reach %s. Is it running, and is the endpoint right?", host),
	}
}
