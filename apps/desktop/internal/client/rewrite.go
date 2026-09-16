package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// RewriteRequest is the body of POST /v1/rewrite.
type RewriteRequest struct {
	Text   string `json:"text"`
	Preset string `json:"preset"`
	Hint   string `json:"hint,omitempty"`
}

// Usage is the token count on the terminal event, when the provider reported one.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// RewriteResult is what the terminal event carried.
type RewriteResult struct {
	Full  string
	Usage Usage
}

// ErrIncompleteStream reports a stream that ended without its terminal event.
//
// Worth its own error: the deltas received so far look like a finished rewrite
// and offering them as one would put half a sentence into someone's document.
var ErrIncompleteStream = errors.New("the helper stopped before finishing the rewrite")

// errMalformedEvent marks a data payload that was not valid JSON. Mid-stream
// that is a protocol failure worth reporting as itself; at end of input it
// means the connection was cut part-way through an event, which is an
// incomplete stream and nothing worse.
var errMalformedEvent = errors.New("malformed stream event")

// Rewrite streams a rewrite, calling onDelta for each fragment as it arrives.
//
// Cancelling ctx closes the connection, which is what the contract requires to
// actually stop the upstream generation — a client that merely stops reading
// has cancelled nothing and is still being billed. That is what Escape does.
func (c *Client) Rewrite(ctx context.Context, req RewriteRequest, onDelta func(string)) (RewriteResult, error) {
	resp, err := c.send(ctx, http.MethodPost, "/"+APIVersion+"/rewrite", req)
	if err != nil {
		return RewriteResult{}, err
	}
	defer resp.Body.Close()

	// Everything knowable before the first token is a real status code, so
	// this catches a missing session, a rejected key or an unreachable
	// endpoint without having consumed a stream that never began.
	if err := errorFor(resp); err != nil {
		return RewriteResult{}, err
	}

	result, err := decodeStream(resp.Body, onDelta)
	if err != nil {
		// A cancelled context surfaces as a read error on the body. Report the
		// cancellation, because "the user pressed Escape" and "the socket
		// broke" call for completely different handling.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, ctxErr
		}
		return result, err
	}
	return result, nil
}

// sseEvent is the union of every payload the daemon puts on a data line.
type sseEvent struct {
	Delta string `json:"delta"`
	Done  bool   `json:"done"`
	Full  string `json:"full"`
	Usage *Usage `json:"usage"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// decodeStream reads text/event-stream framing until the terminal event.
//
// bufio.Reader, not bufio.Scanner: Scanner caps a token at 64 KB by default
// and then stops, silently, which on a long rewrite would truncate the result
// and report success. api/README.md §6 warns shell authors about exactly this.
func decodeStream(r io.Reader, onDelta func(string)) (RewriteResult, error) {
	reader := bufio.NewReader(r)
	var data strings.Builder
	var result RewriteResult
	var deltas strings.Builder

	for {
		line, err := reader.ReadString('\n')
		atEOF := errors.Is(err, io.EOF)
		if err != nil && !atEOF {
			return partial(result, &deltas), err
		}

		if trimmed := strings.TrimRight(line, "\r\n"); trimmed != "" {
			switch {
			case strings.HasPrefix(trimmed, ":"):
				// A comment. Some servers send these as keep-alives.
			case strings.HasPrefix(trimmed, "data:"):
				value := strings.TrimPrefix(trimmed, "data:")
				// Exactly one leading space is framing, not data.
				value = strings.TrimPrefix(value, " ")
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(value)
			default:
				// event:, id: and retry: carry nothing this contract uses.
			}
			if !atEOF {
				continue
			}
			// A last line with no newline after it. Fall through: end of
			// input terminates the event just as a blank line would.
		}

		if data.Len() > 0 {
			done, err := dispatch(data.String(), onDelta, &result, &deltas)
			switch {
			case err != nil && atEOF && errors.Is(err, errMalformedEvent):
				return partial(result, &deltas), ErrIncompleteStream
			case err != nil:
				return partial(result, &deltas), err
			}
			data.Reset()
			if done {
				return result, nil
			}
		}
		if atEOF {
			return partial(result, &deltas), ErrIncompleteStream
		}
	}
}

// dispatch decodes one event's data and reports whether the stream is over.
func dispatch(payload string, onDelta func(string), result *RewriteResult, deltas *strings.Builder) (bool, error) {
	var event sseEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return false, fmt.Errorf("%w: %v", errMalformedEvent, err)
	}

	switch {
	case event.Error != nil:
		// The status was 200 long before this arrived, so an in-band error is
		// the only way the daemon can report a failure that began mid-stream.
		return false, &APIError{Status: http.StatusOK, Code: event.Error.Code, Message: event.Error.Message}
	case event.Done:
		result.Full = event.Full
		if event.Usage != nil {
			result.Usage = *event.Usage
		}
		// Prefer the server's own assembly over ours, as the contract says.
		if result.Full == "" {
			result.Full = deltas.String()
		}
		return true, nil
	case event.Delta != "":
		deltas.WriteString(event.Delta)
		if onDelta != nil {
			onDelta(event.Delta)
		}
	}
	return false, nil
}

// partial returns what arrived before a failure, so a caller can show the
// fragment rather than nothing. It is never offered as a finished rewrite.
func partial(result RewriteResult, deltas *strings.Builder) RewriteResult {
	if result.Full == "" {
		result.Full = deltas.String()
	}
	return result
}
