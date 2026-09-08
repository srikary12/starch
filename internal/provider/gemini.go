package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Gemini speaks the Google AI Studio (Generative Language) REST API.
//
// https://ai.google.dev/api/generate-content
//
// Worth noting for anyone wondering why this exists at all: Google also
// publishes an OpenAI-compatible endpoint at
// https://generativelanguage.googleapis.com/v1beta/openai/, which the
// OpenAICompatible provider drives with no extra code. The native API is here
// because it takes a real system instruction rather than a system message,
// reports token counts consistently, and does not depend on a compatibility
// shim tracking two specifications at once.
type Gemini struct {
	client  *http.Client
	baseURL string
	apiKey  string
	model   string
}

// GeminiDefaultBaseURL is the public endpoint, including the version segment.
const GeminiDefaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"

func NewGemini(client *http.Client, baseURL, apiKey, model string) *Gemini {
	if baseURL == "" {
		baseURL = GeminiDefaultBaseURL
	}
	return &Gemini{
		client:  client,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
	}
}

func (g *Gemini) Name() string { return "Google AI Studio" }

func (g *Gemini) Stream(ctx context.Context, req Request) (<-chan Delta, error) {
	model := req.Model
	if model == "" {
		model = g.model
	}
	if model == "" {
		return nil, &Error{Kind: KindRequest, Message: "No model is configured. Set one in Settings."}
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}

	body := map[string]any{
		"contents": []map[string]any{
			{"role": "user", "parts": []map[string]any{{"text": req.User}}},
		},
		"generationConfig": map[string]any{"maxOutputTokens": maxTokens},
	}
	if req.System != "" {
		// A real system instruction, not a first message. This is the main
		// reason to speak the native API rather than the compatibility shim.
		body["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": req.System}},
		}
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, &Error{Kind: KindRequest, Message: "Could not build the request."}
	}

	httpReq, err := http.NewRequestWithContext(
		ctx, http.MethodPost, g.streamURL(model), bytes.NewReader(encoded),
	)
	if err != nil {
		return nil, &Error{
			Kind:    KindRequest,
			Message: fmt.Sprintf("The endpoint %q is not a usable URL.", g.baseURL),
		}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	// The key goes in a header, never the query string: a URL ends up in proxy
	// logs, crash reports and shell history in a way a header does not.
	httpReq.Header.Set("X-Goog-Api-Key", g.apiKey)

	return runStream(ctx, g.client, httpReq, g.Name(), g.apiKey, newGeminiDecoder())
}

// streamURL builds the streaming endpoint for a model.
//
// `alt=sse` is not optional. Without it the endpoint streams a JSON array
// rather than server-sent events, which parses as one enormous non-event and
// defeats the entire point of streaming.
func (g *Gemini) streamURL(model string) string {
	// Callers may write either "gemini-flash-latest" or the fully qualified
	// "models/gemini-flash-latest"; both appear in Google's own docs.
	name := strings.TrimPrefix(model, "models/")
	return fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse", g.baseURL, name)
}

// geminiChunk is the subset of a streamed chunk this needs.
//
// Several fields are duplicated across naming conventions on purpose. Google's
// own documentation currently describes two shapes for the same data —
// camelCase `usageMetadata.promptTokenCount` in the REST reference, snake_case
// `total_input_tokens` in the thinking guide — and likewise two markers for a
// reasoning part. Accepting both costs a few struct fields; guessing wrong
// costs either token counts or, far worse, the model's reasoning pasted into
// the user's document.
type geminiPart struct {
	Text string `json:"text"`
	// Reasoning markers. `thought` is the REST reference's; `type` carrying
	// "thought" is the thinking guide's.
	Thought bool   `json:"thought"`
	Type    string `json:"type"`
}

// isThought reports whether a part carries reasoning rather than output.
func (p geminiPart) isThought() bool {
	return p.Thought || p.Type == "thought"
}

type geminiChunk struct {
	Candidates []struct {
		Content struct {
			Parts []geminiPart `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`

	UsageMetadata *struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalInputTokens     int `json:"total_input_tokens"`
		TotalOutputTokens    int `json:"total_output_tokens"`
	} `json:"usageMetadata"`

	// Set when the *prompt* was refused, as opposed to the generation being
	// cut short partway.
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`

	Error *struct {
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func newGeminiDecoder() decodeFunc {
	var usage Usage

	return func(event sseEvent) (chunk, error) {
		data := strings.TrimSpace(event.Data)
		if data == "" {
			return chunk{}, nil
		}

		var parsed geminiChunk
		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			// One unreadable chunk costs that chunk, not the rewrite.
			return chunk{}, nil
		}

		if parsed.Error != nil {
			return chunk{}, geminiStreamError(parsed.Error.Status, parsed.Error.Message)
		}

		if fb := parsed.PromptFeedback; fb != nil && fb.BlockReason != "" {
			return chunk{}, &Error{
				Kind:    KindRequest,
				Message: "Google declined to rewrite that selection (" + strings.ToLower(fb.BlockReason) + ").",
			}
		}

		if u := parsed.UsageMetadata; u != nil {
			// Whichever naming the endpoint used; zero from the other.
			usage.InputTokens = max(u.PromptTokenCount, u.TotalInputTokens)
			usage.OutputTokens = max(u.CandidatesTokenCount, u.TotalOutputTokens)
		}

		var text strings.Builder
		var finish string
		for _, candidate := range parsed.Candidates {
			if candidate.FinishReason != "" {
				finish = candidate.FinishReason
			}
			for _, part := range candidate.Content.Parts {
				// Reasoning must never become output. Same trap as Anthropic's
				// thinking_delta: it arrives in the ordinary content channel
				// and would be pasted straight into the user's document.
				if part.isThought() {
					continue
				}
				text.WriteString(part.Text)
			}
		}

		switch finish {
		case "":
			return chunk{Text: text.String()}, nil

		case "STOP", "MAX_TOKENS":
			// MAX_TOKENS is a truncated but genuine rewrite; the overlay shows
			// it and the user decides. Silently discarding it would be worse.
			final := usage
			return chunk{Text: text.String(), Done: true, Usage: &final}, nil

		default:
			// SAFETY, RECITATION, PROHIBITED_CONTENT and friends. The
			// generation stopped partway, so reporting success would hand the
			// user half a sentence to paste over their text.
			return chunk{}, &Error{
				Kind: KindRequest,
				Message: fmt.Sprintf(
					"Google stopped the rewrite early (%s).", strings.ToLower(finish),
				),
			}
		}
	}
}

func geminiStreamError(status, message string) *Error {
	kind, retryable := KindUpstream, true
	switch status {
	case "UNAUTHENTICATED", "PERMISSION_DENIED":
		kind, retryable = KindAuth, false
	case "RESOURCE_EXHAUSTED":
		kind = KindRateLimit
	case "INVALID_ARGUMENT", "FAILED_PRECONDITION":
		kind, retryable = KindRequest, false
	case "NOT_FOUND":
		kind, retryable = KindModel, false
	case "UNAVAILABLE":
		kind = KindOverloaded
	}
	return &Error{
		Kind:      kind,
		Retryable: retryable,
		Message:   withDetail("Google stopped the stream.", tidyDetail(message)),
	}
}
