// Package client speaks the wire contract in api/README.md to a local starchd.
//
// The macOS shell had to hand-write an HTTP/1.1 response parser because its
// networking layer only offers raw sockets. Go does not have that problem:
// net/http over a Unix dialer is the whole transport, which is several hundred
// lines of framing and chunked-encoding edge cases that simply do not exist
// here. The SSE decoder below is the only framing this package owns.
//
// It deliberately does not import internal/server. The shell and the daemon
// agree through the documented contract, and a shell that imported the server
// would link the provider code it must never run — the daemon is the only
// process that holds an API key. Shared *data* models, catalog and brand, are
// a different thing and are imported.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/srikary12/starch/internal/catalog"
)

// APIVersion is the contract version this client speaks. A daemon answering
// with a different one is refused rather than guessed at.
const APIVersion = "v1"

// maxErrorBody bounds what is read from a failed response before giving up on
// finding an error envelope in it.
const maxErrorBody = 64 << 10

// Client talks to one starchd over its Unix domain socket.
type Client struct {
	socketPath string
	token      string
	http       *http.Client
}

// New returns a client for the daemon listening at socketPath, authenticating
// with the handshake token the shell generated at spawn.
func New(socketPath, token string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
		// The contract expects keep-alive: the daemon is a long-lived local
		// process and reconnecting per request would add a syscall round trip
		// to a budget measured in hundreds of milliseconds.
		MaxIdleConns:        4,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		// There is no TLS and no proxy on a Unix socket. Saying so stops the
		// environment's http_proxy from being consulted for a local call.
		Proxy: nil,
	}
	return &Client{
		socketPath: socketPath,
		token:      token,
		// No Client.Timeout: it applies to the whole exchange including the
		// body, which would cut a long rewrite off mid-stream. Per-call
		// deadlines come from the context instead.
		http: &http.Client{Transport: transport},
	}
}

// SocketPath is where this client expects the daemon to be listening.
func (c *Client) SocketPath() string { return c.socketPath }

// APIError is the daemon's error envelope, §3 of api/README.md. Code is stable
// and worth switching on; Message is written to be shown to the user as-is.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s (HTTP %d)", e.Code, e.Status)
	}
	return e.Message
}

// The error codes a shell has a reason to branch on. The rest are shown.
const (
	CodeNoSession           = "no_session"
	CodeUnauthorized        = "unauthorized"
	CodeProviderAuth        = "provider_auth"
	CodeModelNotFound       = "model_not_found"
	CodeSelectionTooLarge   = "selection_too_large"
	CodeRateLimited         = "rate_limited"
	CodeProviderOverloaded  = "provider_overloaded"
	CodeProviderUnreachable = "provider_unreachable"
	CodeProviderError       = "provider_error"
)

// IsNoSession reports whether err is the daemon saying it has no credentials
// yet. The contract's instruction is to POST /v1/session and retry once, not
// to show this to anyone.
func IsNoSession(err error) bool {
	return Code(err) == CodeNoSession
}

// Code returns the daemon's error code for err, or "" if err did not come from
// the daemon. Switching on this is the documented way to tell a retryable
// failure from one worth showing someone.
func Code(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return ""
}

// Health is the /healthz body.
type Health struct {
	Status     string `json:"status"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	APIVersion string `json:"api_version"`
	PID        int    `json:"pid"`
	UptimeMS   int64  `json:"uptime_ms"`
}

// Health checks that the daemon is up and speaking a contract version we know.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	if err := c.do(ctx, http.MethodGet, "/healthz", nil, &h); err != nil {
		return Health{}, err
	}
	if h.APIVersion != APIVersion {
		return h, fmt.Errorf("daemon speaks %s, this shell speaks %s", h.APIVersion, APIVersion)
	}
	return h, nil
}

// Models fetches the provider, endpoint and model catalog that drives the
// pickers. No session and no API key are required, which matters because the
// settings command is usually run before there is a key at all.
func (c *Client) Models(ctx context.Context) (catalog.Catalog, error) {
	var cat catalog.Catalog
	if err := c.do(ctx, http.MethodGet, "/"+APIVersion+"/models", nil, &cat); err != nil {
		return catalog.Catalog{}, err
	}
	return cat, nil
}

// SessionRequest hands the daemon the provider credentials. The key is read
// from the OS secret store immediately before this call and is never written
// anywhere by this process.
type SessionRequest struct {
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	BaseURL        string `json:"base_url,omitempty"`
	APIKey         string `json:"api_key"`
	ThinkingEffort string `json:"thinking_effort,omitempty"`
}

// SessionResponse echoes what the daemon resolved. The key is never echoed.
type SessionResponse struct {
	OK             bool   `json:"ok"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Endpoint       string `json:"endpoint"`
	ThinkingEffort string `json:"thinking_effort,omitempty"`
}

// Session establishes provider credentials for the daemon's lifetime.
func (c *Client) Session(ctx context.Context, req SessionRequest) (SessionResponse, error) {
	var resp SessionResponse
	if err := c.do(ctx, http.MethodPost, "/"+APIVersion+"/session", req, &resp); err != nil {
		return SessionResponse{}, err
	}
	return resp, nil
}

// Preset is one entry in the editable preset set.
type Preset struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Instruction string `json:"instruction"`
}

// PresetSet is the /v1/presets body.
type PresetSet struct {
	Presets []Preset `json:"presets"`
	Path    string   `json:"path"`
	Source  string   `json:"source"`
	Problem string   `json:"problem"`
}

// Presets returns the active preset set, and where it came from.
func (c *Client) Presets(ctx context.Context) (PresetSet, error) {
	var set PresetSet
	if err := c.do(ctx, http.MethodGet, "/"+APIVersion+"/presets", nil, &set); err != nil {
		return PresetSet{}, err
	}
	return set, nil
}

// do performs a request and decodes a JSON response into out.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.send(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if err := errorFor(resp); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding %s %s: %w", method, path, err)
	}
	return nil
}

func (c *Client) send(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}

	// The Host header is ignored by the daemon; "starchd" is the conventional
	// value and makes a packet capture legible.
	req, err := http.NewRequestWithContext(ctx, method, "http://starchd"+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	return resp, nil
}

// errorFor turns a non-2xx response into an *APIError, reading the envelope if
// there is one. A daemon that failed without one still produces a usable
// error rather than a nil one, which would read as success.
func errorFor(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	apiErr := &APIError{Status: resp.StatusCode, Code: "http_error"}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err == nil && len(raw) > 0 {
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &envelope) == nil && envelope.Error.Code != "" {
			apiErr.Code = envelope.Error.Code
			apiErr.Message = envelope.Error.Message
		}
	}
	if apiErr.Message == "" {
		apiErr.Message = fmt.Sprintf("The helper returned %s.", resp.Status)
	}
	return apiErr
}
