package client

import (
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
)

// chunkedReader hands out at most size bytes per Read.
//
// The Swift shell's framing tests replay every fixture at six chunk sizes
// including one byte at a time, because parsing correctly regardless of read
// boundaries is the entire job and the failure mode otherwise is silent
// truncation. The same reasoning applies here.
type chunkedReader struct {
	data []byte
	size int
	pos  int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := r.size
	if n > len(p) {
		n = len(p)
	}
	if r.pos+n > len(r.data) {
		n = len(r.data) - r.pos
	}
	copy(p, r.data[r.pos:r.pos+n])
	r.pos += n
	return n, nil
}

var chunkSizes = []int{1, 2, 3, 7, 64, 1 << 20}

func decodeAtEveryChunkSize(t *testing.T, stream string, check func(t *testing.T, deltas []string, result RewriteResult, err error)) {
	t.Helper()
	for _, size := range chunkSizes {
		var deltas []string
		result, err := decodeStream(
			&chunkedReader{data: []byte(stream), size: size},
			func(d string) { deltas = append(deltas, d) },
		)
		t.Run(name(size), func(t *testing.T) { check(t, deltas, result, err) })
	}
}

func name(size int) string {
	switch size {
	case 1:
		return "one byte at a time"
	case 1 << 20:
		return "all at once"
	default:
		return "chunks of " + strconv.Itoa(size)
	}
}

func TestDecodeStream(t *testing.T) {
	const stream = "data: {\"delta\":\"Thanks for \"}\n\n" +
		"data: {\"delta\":\"the update\"}\n\n" +
		"data: {\"done\":true,\"full\":\"Thanks for the update\",\"usage\":{\"input_tokens\":42,\"output_tokens\":9}}\n\n"

	decodeAtEveryChunkSize(t, stream, func(t *testing.T, deltas []string, result RewriteResult, err error) {
		if err != nil {
			t.Fatalf("decodeStream: %v", err)
		}
		if got := strings.Join(deltas, ""); got != "Thanks for the update" {
			t.Errorf("deltas joined = %q", got)
		}
		if len(deltas) != 2 {
			t.Errorf("got %d deltas, want 2", len(deltas))
		}
		if result.Full != "Thanks for the update" {
			t.Errorf("Full = %q", result.Full)
		}
		if result.Usage.InputTokens != 42 || result.Usage.OutputTokens != 9 {
			t.Errorf("Usage = %+v", result.Usage)
		}
	})
}

// The daemon does not send CRLF, but the SSE framing allows it and a proxy or
// a future rewrite of the encoder could introduce it.
func TestDecodeStreamAcceptsCRLF(t *testing.T) {
	stream := "data: {\"delta\":\"hi\"}\r\n\r\ndata: {\"done\":true,\"full\":\"hi\"}\r\n\r\n"
	decodeAtEveryChunkSize(t, stream, func(t *testing.T, deltas []string, result RewriteResult, err error) {
		if err != nil {
			t.Fatalf("decodeStream: %v", err)
		}
		if result.Full != "hi" {
			t.Errorf("Full = %q", result.Full)
		}
	})
}

// An error after the first delta cannot be a status code: the 200 is long
// gone. A shell that only handled status codes would show a truncated rewrite
// as if it were finished.
func TestDecodeStreamSurfacesAnInBandError(t *testing.T) {
	stream := "data: {\"delta\":\"Thanks\"}\n\n" +
		"data: {\"error\":{\"code\":\"rate_limited\",\"message\":\"Slow down.\"}}\n\n"

	decodeAtEveryChunkSize(t, stream, func(t *testing.T, deltas []string, result RewriteResult, err error) {
		if Code(err) != CodeRateLimited {
			t.Fatalf("code = %q, want %q (err %v)", Code(err), CodeRateLimited, err)
		}
		if err.Error() != "Slow down." {
			t.Errorf("message = %q, want the provider's own words", err.Error())
		}
		if result.Full != "Thanks" {
			t.Errorf("partial = %q, want what had arrived", result.Full)
		}
	})
}

// A stream cut off mid-rewrite must not look like a finished one, because the
// caller would offer half a sentence as a replacement.
func TestTruncatedStreamIsNotSuccess(t *testing.T) {
	stream := "data: {\"delta\":\"Thanks for \"}\n\ndata: {\"delta\":\"the up"

	decodeAtEveryChunkSize(t, stream, func(t *testing.T, deltas []string, result RewriteResult, err error) {
		if !errors.Is(err, ErrIncompleteStream) {
			t.Fatalf("err = %v, want ErrIncompleteStream", err)
		}
		if result.Full != "Thanks for " {
			t.Errorf("partial = %q", result.Full)
		}
	})
}

// A 200 that ends without ever sending anything is the same failure.
func TestEmptyStreamIsNotSuccess(t *testing.T) {
	decodeAtEveryChunkSize(t, "", func(t *testing.T, deltas []string, result RewriteResult, err error) {
		if !errors.Is(err, ErrIncompleteStream) {
			t.Fatalf("err = %v, want ErrIncompleteStream", err)
		}
	})
}

// Framing the shell must tolerate but the daemon does not currently emit:
// comments, unknown fields, a final event with no trailing blank line, and a
// data payload split across two data: lines.
func TestDecodeStreamToleratesTheRestOfTheSSESpec(t *testing.T) {
	stream := ": keep-alive\n\n" +
		"event: message\ndata: {\"delta\":\"a\"}\n\n" +
		"id: 7\ndata: {\"delta\":\"b\"}\n\n" +
		"data: {\"done\":true,\n" +
		"data: \"full\":\"ab\"}\n"

	decodeAtEveryChunkSize(t, stream, func(t *testing.T, deltas []string, result RewriteResult, err error) {
		if err != nil {
			t.Fatalf("decodeStream: %v", err)
		}
		if result.Full != "ab" {
			t.Errorf("Full = %q, want %q", result.Full, "ab")
		}
		if len(deltas) != 2 {
			t.Errorf("got %d deltas, want 2", len(deltas))
		}
	})
}

// The daemon uses bufio.Reader rather than Scanner for exactly this: a long
// rewrite in one event must not be truncated at 64 KB.
func TestDecodeStreamHandlesAnEventLargerThanAnyScannerLimit(t *testing.T) {
	huge := strings.Repeat("word ", 40_000) // ~200 KB, well past Scanner's cap
	stream := "data: {\"done\":true,\"full\":\"" + huge + "\"}\n\n"

	result, err := decodeStream(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatalf("decodeStream: %v", err)
	}
	if result.Full != huge {
		t.Errorf("got %d bytes back, want %d", len(result.Full), len(huge))
	}
}
