package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Anthropic speaks the Messages API.
//
// https://docs.anthropic.com/en/api/messages
type Anthropic struct {
	client  *http.Client
	baseURL string
	apiKey  string
	model   string
}

// AnthropicDefaultBaseURL is used when the session does not name one.
const AnthropicDefaultBaseURL = "https://api.anthropic.com"

// anthropicVersion is the dated API contract this client is written against.
// It is required on every request, and pinning it is what stops a future
// server-side change from altering the shapes parsed below.
const anthropicVersion = "2023-06-01"

// NewAnthropic builds a provider. baseURL may be empty for the public API.
func NewAnthropic(client *http.Client, baseURL, apiKey, model string) *Anthropic {
	if baseURL == "" {
		baseURL = AnthropicDefaultBaseURL
	}
	return &Anthropic{
		client:  client,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
	}
}

func (a *Anthropic) Name() string { return "Anthropic" }

func (a *Anthropic) Stream(ctx context.Context, req Request) (<-chan Delta, error) {
	model := req.Model
	if model == "" {
		model = a.model
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}

	body := map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"stream":     true,
		"messages": []map[string]any{
			{"role": "user", "content": req.User},
		},
	}
	if req.System != "" {
		body["system"] = req.System
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, &Error{Kind: KindRequest, Message: "Could not build the request."}
	}

	httpReq, err := http.NewRequestWithContext(
		ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(encoded),
	)
	if err != nil {
		return nil, &Error{
			Kind:    KindRequest,
			Message: fmt.Sprintf("The endpoint %q is not a usable URL.", a.baseURL),
		}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("X-Api-Key", a.apiKey)
	httpReq.Header.Set("Anthropic-Version", anthropicVersion)

	return runStream(ctx, a.client, httpReq, a.Name(), a.apiKey, newAnthropicDecoder())
}

// anthropicEvent is the subset of the Messages API stream this needs.
type anthropicEvent struct {
	Type  string `json:"type"`
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
	Message struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// newAnthropicDecoder builds a stateful decoder for one stream.
//
// State is needed because the pieces arrive separately: input tokens on
// message_start, output tokens on message_delta, and the end of the stream on
// message_stop.
func newAnthropicDecoder() decodeFunc {
	var usage Usage

	return func(event sseEvent) (chunk, error) {
		// The event name and the JSON `type` always agree; the JSON is
		// authoritative because some proxies drop the `event:` line.
		if event.Data == "" {
			return chunk{}, nil
		}

		var parsed anthropicEvent
		if err := json.Unmarshal([]byte(event.Data), &parsed); err != nil {
			// A single malformed event is not worth destroying a rewrite over,
			// and skipping it lets a proxy that injects junk still work.
			return chunk{}, nil
		}

		switch parsed.Type {
		case "message_start":
			usage.InputTokens = parsed.Message.Usage.InputTokens
			usage.OutputTokens = parsed.Message.Usage.OutputTokens

		case "content_block_delta":
			// Only text_delta becomes output. This matters more than it looks:
			// with thinking enabled the same event type also carries
			// thinking_delta, and emitting those would paste the model's
			// reasoning into the user's document.
			if parsed.Delta.Type == "text_delta" {
				return chunk{Text: parsed.Delta.Text}, nil
			}

		case "message_delta":
			if parsed.Usage.OutputTokens > 0 {
				usage.OutputTokens = parsed.Usage.OutputTokens
			}

		case "message_stop":
			final := usage
			return chunk{Done: true, Usage: &final}, nil

		case "error":
			// An in-band error. The HTTP status was already 200, so this is
			// the only signal that anything went wrong.
			return chunk{}, anthropicStreamError(parsed.Error.Type, parsed.Error.Message)
		}

		return chunk{}, nil
	}
}

func anthropicStreamError(errType, message string) *Error {
	kind, retryable := KindUpstream, true
	switch errType {
	case "authentication_error", "permission_error":
		kind, retryable = KindAuth, false
	case "rate_limit_error":
		kind = KindRateLimit
	case "overloaded_error":
		kind = KindOverloaded
	case "invalid_request_error":
		kind, retryable = KindRequest, false
	}
	return &Error{
		Kind:      kind,
		Retryable: retryable,
		Message:   withDetail("Anthropic stopped the stream.", tidyDetail(message)),
	}
}
