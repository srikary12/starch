package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// The machinery both providers share: run the request, classify a non-2xx
// reply, then pump decoded events into a channel until the stream ends or the
// context is cancelled.

// chunk is what a provider's decoder makes of one SSE event.
type chunk struct {
	// Text is generated output to append. Empty for bookkeeping events.
	Text string
	// Usage, when the event carried token counts.
	Usage *Usage
	// Done marks the provider's terminal event.
	Done bool
}

// decodeFunc turns one event into a chunk. Implementations are stateful —
// token counts and the terminal marker arrive in different events — so one is
// built per stream rather than shared.
type decodeFunc func(sseEvent) (chunk, error)

// runStream executes req and returns a channel of deltas.
//
// Failures that are knowable before any output — a rejected key, an unknown
// model, an unreachable endpoint — come back as the error return rather than
// through the channel, so the caller can show them without having consumed a
// stream that never started.
func runStream(
	ctx context.Context,
	client *http.Client,
	req *http.Request,
	providerName string,
	secret string,
	decode decodeFunc,
) (<-chan Delta, error) {
	resp, err := client.Do(req)
	if err != nil {
		// A cancelled request is the user pressing Escape. Report it as
		// cancellation so nothing upstream renders it as a failure.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, wrapTransportError(err, req.URL.Host)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, classify(resp.StatusCode, providerName, errorDetail(resp.Body, secret))
	}

	// Buffered so a brief stall in the consumer does not stop us reading from
	// the socket, which is what keeps the provider from throttling us.
	out := make(chan Delta, 32)

	go func() {
		defer close(out)
		// Closing the body is what actually aborts the upstream generation on
		// cancellation. Without it the provider keeps generating, and keeps
		// billing, long after the overlay has gone.
		defer resp.Body.Close()
		pump(ctx, resp.Body, decode, out)
	}()

	return out, nil
}

// pump reads events until the stream ends, the provider reports an error, or
// the context is cancelled.
func pump(ctx context.Context, body io.Reader, decode decodeFunc, out chan<- Delta) {
	reader := newSSEReader(body)

	for {
		// Cheap check so a cancelled stream stops promptly even while events
		// are still arriving.
		if ctx.Err() != nil {
			return
		}

		event, err := reader.Next()
		if err != nil {
			switch {
			case ctx.Err() != nil:
				// Cancelled. The reader failed because we closed the body.
				return
			case errors.Is(err, io.EOF):
				// Some endpoints — several local ones especially — just close
				// the connection instead of sending a terminal event. That is
				// a complete response, not a failure.
				send(ctx, out, Delta{Done: true})
			default:
				send(ctx, out, Delta{Err: &Error{
					Kind:      KindUpstream,
					Retryable: true,
					Message:   "The connection dropped mid-rewrite.",
				}})
			}
			return
		}

		c, err := decode(event)
		if err != nil {
			send(ctx, out, Delta{Err: err})
			return
		}

		if c.Text != "" && !send(ctx, out, Delta{Text: c.Text}) {
			return
		}
		if c.Done {
			send(ctx, out, Delta{Done: true, Usage: c.Usage})
			return
		}
	}
}

// send delivers a delta unless the context is cancelled first. It reports
// whether the delta landed.
func send(ctx context.Context, out chan<- Delta, d Delta) bool {
	select {
	case out <- d:
		return true
	case <-ctx.Done():
		return false
	}
}

// errorDetail pulls a human-readable message out of an error body.
//
// Anthropic and every OpenAI-compatible endpoint use the same envelope shape,
// so one parser covers both. The read is capped: an endpoint that answers an
// error with a huge body should not be able to make us allocate it.
func errorDetail(body io.Reader, secret string) string {
	raw, err := io.ReadAll(io.LimitReader(body, 8<<10))
	if err != nil || len(raw) == 0 {
		return ""
	}

	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
		// Ollama and a few others answer with a bare string field.
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil {
		if msg := envelope.Error.Message; msg != "" {
			return tidyDetail(scrubSecrets(msg, secret))
		}
		if msg := envelope.Message; msg != "" {
			return tidyDetail(scrubSecrets(msg, secret))
		}
	}

	// Not JSON. Fall back to the raw text if it is short enough to be a
	// message rather than a web page.
	text := strings.TrimSpace(string(raw))
	if text == "" || len(text) > 200 || strings.HasPrefix(text, "<") {
		return ""
	}
	return tidyDetail(scrubSecrets(text, secret))
}

// keyLike matches the shape of the common API key prefixes, as a backstop for
// credentials we were not given — an organisation key, a proxy's own token.
var keyLike = regexp.MustCompile(`\b(?:sk|pk|api|key|token)[-_][A-Za-z0-9_\-]{8,}`)

// scrubSecrets removes credentials from text that is about to be shown to the
// user.
//
// A provider that echoes the rejected key back in its error message is not
// hypothetical, and Message goes straight into the overlay — and from there
// into whatever the user pastes into a bug report. The exact-match pass is the
// reliable one; the pattern pass covers keys this process never held.
func scrubSecrets(text, secret string) string {
	// Short values would match far too much; a real key is never this short.
	if len(secret) >= 8 {
		text = strings.ReplaceAll(text, secret, "[redacted]")
	}
	return text
}

// tidyDetail makes a provider message fit to append to one of ours: single
// line, bounded length, ending in a full stop.
func tidyDetail(s string) string {
	// The pattern pass lives here rather than only in scrubSecrets so that it
	// covers every path a provider message can take to the screen, including
	// in-band stream errors where the decoder does not hold the key.
	s = keyLike.ReplaceAllString(s, "[redacted]")
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	const limit = 160
	if len(s) > limit {
		s = strings.TrimSpace(s[:limit]) + "…"
	}
	if s != "" && !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, "…") {
		s += "."
	}
	return s
}
