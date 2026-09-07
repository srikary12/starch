package provider

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

// Server-sent event parsing.
//
// This uses bufio.Reader with ReadString rather than bufio.Scanner, and that
// choice is load-bearing. Scanner has a 64KB default token limit, and when a
// line exceeds it Scan simply returns false — the stream looks like it ended
// cleanly and the tail of the rewrite is silently dropped. ReadString grows
// its buffer to fit the line instead. A rewrite of a long selection can put a
// whole paragraph in a single delta, so this is a realistic input, not a
// theoretical one.
//
// The other thing worth stating: nothing here may assume an event arrives in
// one Read. Chunk boundaries fall wherever the network puts them, including
// mid-field and mid-UTF-8-sequence. ReadString handles that by definition,
// which is most of why it is the right primitive.

// sseEvent is one dispatched event.
type sseEvent struct {
	// Name is the `event:` field, empty when the stream omits it. Anthropic
	// sets it; most OpenAI-compatible endpoints do not.
	Name string
	// Data is the concatenation of the event's `data:` fields, newline-joined
	// as the specification requires.
	Data string
}

// sseReader decodes an event stream.
type sseReader struct {
	r *bufio.Reader
}

func newSSEReader(r io.Reader) *sseReader {
	// A 16KB starting buffer covers the overwhelming majority of deltas in one
	// fill without being wasteful. ReadString grows past it when it must.
	return &sseReader{r: bufio.NewReaderSize(r, 16<<10)}
}

// Next returns the next event, or io.EOF when the stream ends.
func (s *sseReader) Next() (sseEvent, error) {
	var (
		event   sseEvent
		data    strings.Builder
		started bool
	)

	for {
		raw, err := s.r.ReadString('\n')

		// ReadString can return a partial final line *together with* io.EOF,
		// so the line has to be processed before the error is considered.
		// Getting this backwards drops the last event of any stream that does
		// not end with a newline.
		if raw != "" {
			line := strings.TrimRight(raw, "\r\n")
			switch {
			case line == "":
				// A blank line dispatches. Blank lines before any field are
				// just separators between events and are skipped.
				if started {
					event.Data = data.String()
					return event, nil
				}
			case strings.HasPrefix(line, ":"):
				// A comment. Providers send these as keep-alives; ignoring
				// them is required, not optional.
			default:
				field, value := splitSSEField(line)
				switch field {
				case "event":
					event.Name = value
					started = true
				case "data":
					if data.Len() > 0 {
						data.WriteByte('\n')
					}
					data.WriteString(value)
					started = true
				case "id", "retry":
					// Unused, but must not derail parsing.
					started = true
				}
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) && started {
				// The stream ended without a trailing blank line. Emit what
				// was collected; the caller sees EOF on the next call.
				event.Data = data.String()
				return event, nil
			}
			return sseEvent{}, err
		}
	}
}

// splitSSEField splits "field: value". A line with no colon is a field with an
// empty value, and exactly one leading space after the colon is stripped —
// both per the specification, and both easy to get wrong in ways that only
// show up against one provider.
func splitSSEField(line string) (field, value string) {
	colon := strings.IndexByte(line, ':')
	if colon < 0 {
		return line, ""
	}
	return line[:colon], strings.TrimPrefix(line[colon+1:], " ")
}
