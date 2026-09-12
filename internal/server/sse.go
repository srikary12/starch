package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// sseStream writes the daemon's side of an event stream.
//
// The framing is the one documented in api/README.md §6: one JSON object per
// `data:` line, blank line between events. Deliberately minimal — the shells
// that consume this are written in other languages, and every feature of the
// SSE spec used here is one more thing each of them has to implement.
type sseStream struct {
	w       http.ResponseWriter
	flusher http.Flusher
	enc     *json.Encoder
}

func newSSEStream(w http.ResponseWriter) (*sseStream, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("response writer does not support flushing")
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	// Nothing sits between the daemon and the shell, but a proxy that buffered
	// this would turn a streaming rewrite into a single late blob, which is
	// the entire failure mode the product exists to avoid.
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	return &sseStream{w: w, flusher: flusher, enc: json.NewEncoder(w)}, nil
}

// send writes one event and flushes. It reports whether the write succeeded;
// a false return means the client is gone and the caller should stop.
func (s *sseStream) send(payload any) bool {
	if _, err := s.w.Write([]byte("data: ")); err != nil {
		return false
	}
	// Encode appends its own newline, which closes the data line. The blank
	// line after it terminates the event.
	if err := s.enc.Encode(payload); err != nil {
		slog.Debug("encoding sse event", "error", err)
		return false
	}
	if _, err := s.w.Write([]byte("\n")); err != nil {
		return false
	}
	s.flusher.Flush()
	return true
}
