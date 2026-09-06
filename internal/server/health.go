package server

import (
	"net/http"
	"os"

	"github.com/srikary12/starch/internal/brand"
)

// Health is the body of a successful GET /healthz.
type Health struct {
	// Status is "ok" whenever the daemon answers at all. It exists so a shell
	// can distinguish a healthy daemon from a degraded one later without a
	// contract change.
	Status string `json:"status"`
	// Name is the product name, so a shell can confirm it reached the daemon
	// it expected.
	Name string `json:"name"`
	// Version is the daemon build version.
	Version string `json:"version"`
	// APIVersion is the wire contract version this daemon serves.
	APIVersion string `json:"api_version"`
	// PID lets the shell verify the process it spawned is the one answering,
	// and lets it clean up if not.
	PID int `json:"pid"`
	// UptimeMS is milliseconds since daemon start.
	UptimeMS int64 `json:"uptime_ms"`
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, Health{
		Status:     "ok",
		Name:       brand.Name,
		Version:    s.opts.Version,
		APIVersion: APIVersion,
		PID:        os.Getpid(),
		UptimeMS:   s.now().Sub(s.started).Milliseconds(),
	})
}
