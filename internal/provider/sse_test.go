package provider

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// chunkedReader hands out a fixed number of bytes per Read, so a test can
// force chunk boundaries to land anywhere — mid-field, mid-line, between the
// \r and the \n. Real streams split wherever the network decides, and every
// SSE bug worth having starts with an implementation that assumed otherwise.
type chunkedReader struct {
	data []byte
	size int
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := min(min(c.size, len(p)), len(c.data))
	copy(p, c.data[:n])
	c.data = c.data[n:]
	return n, nil
}

// readAll drains a reader into a slice of events.
func readAll(t *testing.T, r *sseReader) []sseEvent {
	t.Helper()
	var events []sseEvent
	for {
		ev, err := r.Next()
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		events = append(events, ev)
		if len(events) > 1000 {
			t.Fatal("runaway parse: more than 1000 events")
		}
	}
}

func TestSSEParsing(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []sseEvent
	}{
		{
			name:  "single event with a name",
			input: "event: content_block_delta\ndata: {\"a\":1}\n\n",
			want:  []sseEvent{{Name: "content_block_delta", Data: `{"a":1}`}},
		},
		{
			name:  "data only, no event name",
			input: "data: {\"a\":1}\n\n",
			want:  []sseEvent{{Data: `{"a":1}`}},
		},
		{
			name:  "two events",
			input: "data: one\n\ndata: two\n\n",
			want:  []sseEvent{{Data: "one"}, {Data: "two"}},
		},
		{
			name:  "multi-line data is newline-joined",
			input: "data: line one\ndata: line two\n\n",
			want:  []sseEvent{{Data: "line one\nline two"}},
		},
		{
			name:  "comments and keep-alives are ignored",
			input: ": keep-alive\ndata: payload\n\n",
			want:  []sseEvent{{Data: "payload"}},
		},
		{
			name:  "a bare comment dispatches nothing",
			input: ": ping\n\n: ping\n\ndata: real\n\n",
			want:  []sseEvent{{Data: "real"}},
		},
		{
			name:  "CRLF line endings",
			input: "event: delta\r\ndata: payload\r\n\r\n",
			want:  []sseEvent{{Name: "delta", Data: "payload"}},
		},
		{
			name:  "no space after the colon",
			input: "data:payload\n\n",
			want:  []sseEvent{{Data: "payload"}},
		},
		{
			name:  "only the first space after the colon is stripped",
			input: "data:  leading\n\n",
			want:  []sseEvent{{Data: " leading"}},
		},
		{
			name:  "a value containing colons survives intact",
			input: "data: {\"url\":\"https://example.com:443/x\"}\n\n",
			want:  []sseEvent{{Data: `{"url":"https://example.com:443/x"}`}},
		},
		{
			name:  "id and retry do not derail parsing",
			input: "id: 42\nretry: 1000\ndata: payload\n\n",
			want:  []sseEvent{{Data: "payload"}},
		},
		{
			// Providers end streams inconsistently. Dropping the final event
			// because the trailing blank line is missing loses the terminal
			// message_stop, and with it the usage numbers.
			name:  "a final event without a trailing blank line is still emitted",
			input: "data: last\n",
			want:  []sseEvent{{Data: "last"}},
		},
		{
			name:  "a final event with no trailing newline at all is still emitted",
			input: "data: last",
			want:  []sseEvent{{Data: "last"}},
		},
		{
			name:  "empty data field",
			input: "data:\n\n",
			want:  []sseEvent{{Data: ""}},
		},
		{
			name:  "blank lines between events are skipped",
			input: "\n\n\ndata: payload\n\n\n\n",
			want:  []sseEvent{{Data: "payload"}},
		},
		{
			name:  "an empty stream yields nothing",
			input: "",
			want:  nil,
		},
		{
			name:  "unicode survives",
			input: "data: naïve — 日本語 🎉\n\n",
			want:  []sseEvent{{Data: "naïve — 日本語 🎉"}},
		},
	}

	// Every fixture is replayed at a range of chunk sizes. One byte at a time
	// is the important one: it puts a Read boundary between every character,
	// including inside multi-byte runes and between \r and \n.
	chunkSizes := []int{1, 2, 3, 7, 64, 1 << 20}

	for _, tt := range tests {
		for _, size := range chunkSizes {
			t.Run(tt.name+"/chunk="+itoa(size), func(t *testing.T) {
				r := newSSEReader(&chunkedReader{data: []byte(tt.input), size: size})
				got := readAll(t, r)

				if len(got) != len(tt.want) {
					t.Fatalf("got %d events, want %d\ngot:  %#v\nwant: %#v",
						len(got), len(tt.want), got, tt.want)
				}
				for i := range got {
					if got[i] != tt.want[i] {
						t.Errorf("event %d:\n got %#v\nwant %#v", i, got[i], tt.want[i])
					}
				}
			})
		}
	}
}

// The specific reason this package does not use bufio.Scanner. Scanner's
// default token limit is 64KB; past it Scan returns false with no error, the
// stream looks like it ended cleanly, and the rest of the rewrite vanishes.
// A long selection genuinely produces deltas this size.
func TestSSEHandlesLinesFarBeyondScannerLimit(t *testing.T) {
	for _, size := range []int{64 << 10, 256 << 10, 1 << 20} {
		t.Run(itoa(size), func(t *testing.T) {
			payload := strings.Repeat("x", size)
			input := "data: " + payload + "\n\n"

			// Read in small chunks so the line is also split across many reads.
			r := newSSEReader(&chunkedReader{data: []byte(input), size: 4096})
			events := readAll(t, r)

			if len(events) != 1 {
				t.Fatalf("got %d events, want 1", len(events))
			}
			if len(events[0].Data) != size {
				t.Errorf("got %d bytes of data, want %d — this is the silent truncation Scanner causes",
					len(events[0].Data), size)
			}
			if events[0].Data != payload {
				t.Error("payload was corrupted, not merely truncated")
			}
		})
	}
}

// A realistic Anthropic stream, replayed byte by byte.
func TestSSEAnthropicShapeSplitEverywhere(t *testing.T) {
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":42,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Thanks for "}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"the update."}}` + "\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}` + "\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	r := newSSEReader(&chunkedReader{data: []byte(stream), size: 1})
	events := readAll(t, r)

	wantNames := []string{
		"message_start", "content_block_start", "ping",
		"content_block_delta", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}
	if len(events) != len(wantNames) {
		t.Fatalf("got %d events, want %d", len(events), len(wantNames))
	}
	for i, want := range wantNames {
		if events[i].Name != want {
			t.Errorf("event %d: got name %q, want %q", i, events[i].Name, want)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
