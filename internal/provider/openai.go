package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// OpenAICompatible speaks the chat completions API.
//
// One implementation covers OpenAI, OpenRouter, Groq, Together, Ollama and LM
// Studio, because they all settled on the same request and stream shapes. The
// local ones matter most: they are the answer for anyone who will not send
// work messages to a third party.
type OpenAICompatible struct {
	client  *http.Client
	baseURL string
	apiKey  string
	model   string
}

// NewOpenAICompatible builds a provider.
//
// baseURL is required and is expected to include whatever version prefix the
// endpoint uses — "https://api.openai.com/v1", "http://localhost:11434/v1" for
// Ollama, "http://localhost:1234/v1" for LM Studio. That is the form every one
// of these services documents, so taking it verbatim avoids guessing.
func NewOpenAICompatible(client *http.Client, baseURL, apiKey, model string) *OpenAICompatible {
	return &OpenAICompatible{
		client:  client,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
	}
}

func (o *OpenAICompatible) Name() string {
	// Named after the endpoint rather than the protocol, so an error message
	// says "localhost:11434 does not recognise that model" rather than
	// something the user cannot map back to anything they configured.
	if host := hostOf(o.baseURL); host != "" {
		return host
	}
	return "The endpoint"
}

func (o *OpenAICompatible) Stream(ctx context.Context, req Request) (<-chan Delta, error) {
	if o.baseURL == "" {
		return nil, &Error{
			Kind:    KindRequest,
			Message: "No endpoint is configured. Set one in Settings.",
		}
	}

	model := req.Model
	if model == "" {
		model = o.model
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}

	messages := make([]map[string]any, 0, 2)
	if req.System != "" {
		messages = append(messages, map[string]any{"role": "system", "content": req.System})
	}
	messages = append(messages, map[string]any{"role": "user", "content": req.User})

	body := map[string]any{
		"model":      model,
		"stream":     true,
		"messages":   messages,
		"max_tokens": maxTokens,
		// Ask for token counts in the terminal chunk. OpenAI needs this opt-in;
		// endpoints that do not know the field ignore it, and the ones that
		// reject unknown fields are handled by usage simply staying zero.
		"stream_options": map[string]any{"include_usage": true},
	}
	// Only when the user picked a level. Endpoints vary on this field more
	// than any other — OpenAI takes none through max, Ollama maps it onto its
	// own Think flag and takes three of them, and a gateway in front of a
	// non-reasoning model may reject it outright — so sending it unasked would
	// break plain chat models that work fine today.
	if req.Effort != "" {
		body["reasoning_effort"] = req.Effort
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, &Error{Kind: KindRequest, Message: "Could not build the request."}
	}

	httpReq, err := http.NewRequestWithContext(
		ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(encoded),
	)
	if err != nil {
		return nil, &Error{
			Kind:    KindRequest,
			Message: fmt.Sprintf("The endpoint %q is not a usable URL.", o.baseURL),
		}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	// Local endpoints generally need no key, and sending an empty bearer token
	// makes some of them reject the request outright.
	if o.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)
	}

	return runStream(ctx, o.client, httpReq, o.Name(), o.apiKey, newOpenAIDecoder())
}

// openAIChunk is the subset of a chat completion chunk this needs.
type openAIChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
			// Reasoning models on some gateways stream their reasoning in a
			// parallel field. It is deliberately not read: it must never reach
			// the user's document.
			Reasoning string `json:"reasoning"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func newOpenAIDecoder() decodeFunc {
	var usage Usage
	var sawFinish bool

	return func(event sseEvent) (chunk, error) {
		data := strings.TrimSpace(event.Data)
		if data == "" {
			return chunk{}, nil
		}

		// The protocol's terminal sentinel. It is not JSON, so it has to be
		// matched before any parse attempt.
		if data == "[DONE]" {
			final := usage
			return chunk{Done: true, Usage: &final}, nil
		}

		var parsed openAIChunk
		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			return chunk{}, nil
		}

		if parsed.Error != nil {
			return chunk{}, &Error{
				Kind:      KindUpstream,
				Retryable: true,
				Message:   withDetail("The endpoint stopped the stream.", tidyDetail(parsed.Error.Message)),
			}
		}

		// Usage arrives in its own final chunk when include_usage is honoured,
		// typically alongside an empty choices array.
		if parsed.Usage != nil {
			usage.InputTokens = parsed.Usage.PromptTokens
			usage.OutputTokens = parsed.Usage.CompletionTokens
		}

		if len(parsed.Choices) == 0 {
			// A usage-only chunk after the finish reason ends the stream for
			// endpoints that never send [DONE].
			if sawFinish && parsed.Usage != nil {
				final := usage
				return chunk{Done: true, Usage: &final}, nil
			}
			return chunk{}, nil
		}

		choice := parsed.Choices[0]
		if choice.FinishReason != nil {
			sawFinish = true
		}
		return chunk{Text: choice.Delta.Content}, nil
	}
}

// hostOf pulls the host out of a base URL for use in messages, without
// bringing in net/url error handling for what is a display concern.
func hostOf(rawURL string) string {
	s := rawURL
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?"); i >= 0 {
		s = s[:i]
	}
	return s
}
