package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/srikary12/starch/internal/prompt"
	"github.com/srikary12/starch/internal/provider"
)

// maxSelectionBytes bounds what a shell may submit. Well past any plausible
// selection, small enough that a runaway client cannot make the daemon
// allocate without limit.
const maxSelectionBytes = 1 << 20

type rewriteRequest struct {
	Text   string `json:"text"`
	Preset string `json:"preset"`
	Hint   string `json:"hint"`
}

// sseDelta is an incremental event: {"delta":"..."}.
type sseDelta struct {
	Delta string `json:"delta"`
}

// sseDone is the single terminal event.
type sseDone struct {
	Done  bool            `json:"done"`
	Full  string          `json:"full"`
	Usage *provider.Usage `json:"usage,omitempty"`
}

// sseError ends a stream that has already returned 200. Clients must handle
// these, not just status codes.
type sseError struct {
	Error APIError `json:"error"`
}

func (s *Server) handleRewrite(w http.ResponseWriter, r *http.Request) {
	var req rewriteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSelectionBytes+8<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, ErrBadRequest, "Could not read the rewrite request.")
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeError(w, http.StatusBadRequest, ErrBadRequest, "There is no text to rewrite.")
		return
	}
	if len(req.Text) > maxSelectionBytes {
		writeError(w, http.StatusRequestEntityTooLarge, ErrTooLarge, "That selection is too long.")
		return
	}

	sess := s.currentSession()
	if sess == nil {
		writeError(w, http.StatusPreconditionRequired, ErrNoSession,
			"No provider is configured yet. The app should call /v1/session first.")
		return
	}

	preset := prompt.Find(prompt.Defaults, req.Preset)
	system, user := prompt.Build(preset, req.Text, req.Hint)

	// r.Context() is cancelled when the client disconnects, which is exactly
	// what Escape does. Passing it to the provider is what turns "the overlay
	// closed" into "the generation stopped being billed".
	ctx := r.Context()
	started := time.Now()

	deltas, err := sess.provider.Stream(ctx, provider.Request{
		System: system,
		User:   user,
		Model:  sess.model,
	})
	if err != nil {
		// The stream never started, so a normal error response is still
		// possible — better than a 200 followed by an in-band error.
		if errors.Is(err, ctx.Err()) && ctx.Err() != nil {
			return // client went away before we got anywhere
		}
		status, code, message := describeProviderError(err)
		writeError(w, status, code, message)
		return
	}

	stream, err := newSSEStream(w)
	if err != nil {
		s.log.Error("response writer cannot stream", "error", err)
		writeError(w, http.StatusInternalServerError, ErrInternal, "Cannot stream a response.")
		return
	}

	var (
		full      strings.Builder
		firstByte time.Duration
	)

	for delta := range deltas {
		switch {
		case delta.Err != nil:
			// Already committed to 200, so this has to go in-band.
			status, code, message := describeProviderError(delta.Err)
			_ = status
			s.log.Warn("rewrite failed mid-stream", "code", string(code), "preset", preset.ID)
			stream.send(sseError{Error: APIError{Code: code, Message: message}})
			return

		case delta.Done:
			cleaned := prompt.Clean(full.String(), req.Text)
			stream.send(sseDone{Done: true, Full: cleaned, Usage: delta.Usage})

			// Metadata only: never the selection, never the rewrite.
			// thought_tokens is here because reasoning is invisible in the
			// result but can be most of the wait. Without it a slow rewrite
			// looks like a slow network, and the fix is in the wrong place.
			thoughts := 0
			if delta.Usage != nil {
				thoughts = delta.Usage.ThoughtTokens
			}
			s.log.Info("rewrite complete",
				"preset", preset.ID,
				"chars_in", len(req.Text),
				"chars_out", len(cleaned),
				"first_token_ms", firstByte.Milliseconds(),
				"total_ms", time.Since(started).Milliseconds(),
				"thought_tokens", thoughts,
			)
			return

		default:
			if firstByte == 0 {
				firstByte = time.Since(started)
			}
			full.WriteString(delta.Text)
			if !stream.send(sseDelta{Delta: delta.Text}) {
				// The client is gone. Returning cancels ctx, which aborts the
				// upstream call; draining to the end would keep generating.
				return
			}
		}
	}

	// The channel closed without a terminal delta. Only reachable if the
	// context was cancelled, in which case nobody is listening anyway.
	if ctx.Err() == nil {
		stream.send(sseError{Error: APIError{
			Code:    ErrProviderError,
			Message: "The rewrite ended unexpectedly.",
		}})
	}
}

// describeProviderError maps a provider failure onto the wire vocabulary.
func describeProviderError(err error) (int, ErrorCode, string) {
	var provErr *provider.Error
	if !errors.As(err, &provErr) {
		return http.StatusBadGateway, ErrProviderError, "The rewrite failed."
	}

	status := http.StatusBadGateway
	switch provErr.Kind {
	case provider.KindAuth:
		// 502 would suggest the daemon's problem; this is the user's key.
		status = http.StatusUnauthorized
	case provider.KindRateLimit:
		status = http.StatusTooManyRequests
	case provider.KindTooLarge:
		status = http.StatusRequestEntityTooLarge
	case provider.KindRequest, provider.KindModel:
		status = http.StatusBadRequest
	case provider.KindOverloaded, provider.KindUnreachable:
		status = http.StatusServiceUnavailable
	}
	return status, ErrorCode(provErr.Kind), provErr.Message
}
