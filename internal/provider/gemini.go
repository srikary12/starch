package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
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

	// Set once the endpoint rejects thinkingLevel, so the cost of discovering
	// that is paid a single time per session rather than per rewrite.
	noThinkingLevel atomic.Bool
}

// GeminiDefaultBaseURL is the public endpoint, including the version segment.
const GeminiDefaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"

func NewGemini(client *http.Client, baseURL, apiKey, model string) *Gemini {
	normalized := NormalizeGeminiBaseURL(baseURL)
	if normalized == "" {
		normalized = GeminiDefaultBaseURL
	}
	return &Gemini{
		client:  client,
		baseURL: normalized,
		apiKey:  apiKey,
		model:   model,
	}
}

// NormalizeGeminiBaseURL trims a pasted endpoint back to the API root.
//
// Every example in Google's documentation is a complete request URL —
// `.../v1beta/models/gemini-flash-latest:generateContent` — so that is what
// lands in an "endpoint" field, and appending our own path to it produced a
// nonsense URL and a 404 that read as "unknown model". Treating the full URL
// as valid input is not leniency; it is the form users actually have.
//
// Any query string is dropped, which also discards a `?key=` some examples
// use. That is deliberate: the key belongs in the Keychain and goes out as a
// header, and silently carrying one in from a settings field would put it
// somewhere neither the app nor the user is tracking.
func NormalizeGeminiBaseURL(raw string) string {
	s := strings.TrimSpace(raw)
	if i := strings.IndexByte(s, '?'); i >= 0 {
		s = s[:i]
	}
	// Everything from /models/ onwards is per-request, not part of the root.
	if i := strings.Index(s, "/models/"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimRight(s, "/")

	// A bare host — no path at all — cannot work: the API requires a version
	// segment. Supply the one this client is written against rather than let
	// it 404. Anything with a path is left exactly as given, because a custom
	// gateway's path is not ours to rewrite.
	if parsed, err := url.Parse(s); err == nil && parsed.Host != "" && parsed.Path == "" {
		s += "/v1beta"
	}
	return s
}

func (g *Gemini) Name() string { return "Google AI Studio" }

// geminiMinimumThinking is what to ask for when the session names no level.
// "low" rather than "minimal": gemini-2.5-flash rejects minimal, and low is
// the floor every current model accepts.
const geminiMinimumThinking = "low"

func (g *Gemini) Stream(ctx context.Context, req Request) (<-chan Delta, error) {
	// Older models take thinkingConfig.thinkingBudget instead and reject this
	// field outright. Rather than maintain a model table that would rot, ask
	// once and remember the answer for this session.
	if g.noThinkingLevel.Load() {
		return g.stream(ctx, req, false)
	}

	deltas, err := g.stream(ctx, req, true)
	if err != nil && geminiRejectedThinkingLevel(err) {
		g.noThinkingLevel.Store(true)
		return g.stream(ctx, req, false)
	}
	return deltas, err
}

// geminiRejectedThinkingLevel reports whether the endpoint refused the field
// itself, as opposed to failing for a reason retrying would not fix.
func geminiRejectedThinkingLevel(err error) bool {
	var provErr *Error
	if !errors.As(err, &provErr) || provErr.Kind != KindRequest {
		return false
	}
	lower := strings.ToLower(provErr.Message)
	return strings.Contains(lower, "thinking") || strings.Contains(lower, "unknown name")
}

func (g *Gemini) stream(ctx context.Context, req Request, withThinkingLevel bool) (<-chan Delta, error) {
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

	generation := map[string]any{"maxOutputTokens": maxTokens}
	if withThinkingLevel {
		// Rewriting a sentence is not a reasoning task, and current Gemini
		// models think by default — which is the difference between a rewrite
		// arriving in half a second and in two. So unlike the other providers
		// this one always asks for a level: a session that names none still
		// gets the floor rather than the model's own default.
		level := req.Effort
		if level == "" {
			level = geminiMinimumThinking
		}
		generation["thinkingLevel"] = level
	}

	body := map[string]any{
		"contents": []map[string]any{
			{"role": "user", "parts": []map[string]any{{"text": req.User}}},
		},
		"generationConfig": generation,
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
		ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
		TotalInputTokens     int `json:"total_input_tokens"`
		TotalOutputTokens    int `json:"total_output_tokens"`
		TotalThoughtTokens   int `json:"total_thought_tokens"`
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
			usage.ThoughtTokens = max(u.ThoughtsTokenCount, u.TotalThoughtTokens)
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
