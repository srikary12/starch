package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/srikary12/starch/internal/provider"
)

// The session is the shell handing over credentials.
//
// The key lives in this process's memory and nowhere else: not on disk, not in
// a log, not echoed back in a response. `starchd` has no access to any
// platform secret store by design — each shell uses its own native one, so the
// contract stays identical across macOS, Windows and Linux.

// ProviderID names a provider implementation on the wire.
type ProviderID string

const (
	ProviderAnthropic        ProviderID = "anthropic"
	ProviderOpenAICompatible ProviderID = "openai_compatible"
	ProviderGemini           ProviderID = "gemini"
)

type sessionRequest struct {
	Provider ProviderID `json:"provider"`
	Model    string     `json:"model"`
	BaseURL  string     `json:"base_url"`
	APIKey   string     `json:"api_key"`
	// ThinkingEffort is the reasoning level to ask for, in the provider's own
	// vocabulary. Optional: empty leaves the endpoint's own default alone,
	// except on Gemini, where the daemon still asks for its floor because
	// those models think by default and a rewrite is not a reasoning task.
	ThinkingEffort string `json:"thinking_effort"`
}

type sessionResponse struct {
	OK       bool       `json:"ok"`
	Provider ProviderID `json:"provider"`
	Model    string     `json:"model"`
	// ThinkingEffort is echoed so a shell can confirm what was accepted.
	ThinkingEffort string `json:"thinking_effort,omitempty"`
	// Endpoint is echoed so a shell can confirm what was resolved, especially
	// where the daemon supplied a default. The key is never echoed.
	Endpoint string `json:"endpoint"`
}

// session is the active provider configuration.
type session struct {
	provider provider.Provider
	model    string
	id       ProviderID
	endpoint string
	effort   string
}

// validEffort accepts an empty value, or a short lowercase word.
//
// Deliberately not an allowlist of the levels this build happens to know. Which
// levels a model accepts is curated in internal/catalog, differs per model, and
// changes when a provider ships one — a daemon that refused an unrecognised
// level would have to be rebuilt to keep up, and would reject a level its own
// catalog was offering. This check exists only to keep junk out of an upstream
// request body. The endpoint is the authority on what it accepts, and it says
// so in a message the user can read.
func validEffort(s string) bool {
	if len(s) > 16 {
		return false
	}
	for _, r := range s {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	var req sessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, ErrBadRequest, "Could not read the session request.")
		return
	}

	if strings.TrimSpace(req.Model) == "" {
		writeError(w, http.StatusBadRequest, ErrBadRequest, "A model is required.")
		return
	}

	effort := strings.TrimSpace(req.ThinkingEffort)
	if !validEffort(effort) {
		writeError(w, http.StatusBadRequest, ErrBadRequest,
			`thinking_effort must be a short lowercase word such as "low".`)
		return
	}

	var (
		prov     provider.Provider
		endpoint string
	)
	switch req.Provider {
	case ProviderAnthropic:
		endpoint = req.BaseURL
		if endpoint == "" {
			endpoint = provider.AnthropicDefaultBaseURL
		}
		prov = provider.NewAnthropic(s.httpClient, endpoint, req.APIKey, req.Model)

	case ProviderGemini:
		// Normalised here, not just inside the provider, so the endpoint
		// echoed back is the one actually used. A shell confirming what the
		// daemon resolved is the entire reason that field exists, and showing
		// the raw paste would hide exactly the rewriting that fixes a pasted
		// full request URL.
		endpoint = provider.NormalizeGeminiBaseURL(req.BaseURL)
		if endpoint == "" {
			endpoint = provider.GeminiDefaultBaseURL
		}
		prov = provider.NewGemini(s.httpClient, endpoint, req.APIKey, req.Model)

	case ProviderOpenAICompatible:
		endpoint = strings.TrimSpace(req.BaseURL)
		if endpoint == "" {
			writeError(w, http.StatusBadRequest, ErrBadRequest,
				"An endpoint URL is required for an OpenAI-compatible provider.")
			return
		}
		prov = provider.NewOpenAICompatible(s.httpClient, endpoint, req.APIKey, req.Model)

	default:
		writeError(w, http.StatusBadRequest, ErrBadRequest,
			"Unknown provider. Use \"anthropic\", \"gemini\" or \"openai_compatible\".")
		return
	}

	s.mu.Lock()
	s.session = &session{
		provider: prov,
		model:    req.Model,
		id:       req.Provider,
		endpoint: endpoint,
		effort:   effort,
	}
	s.mu.Unlock()

	// Deliberately logs the endpoint and model but never the key, and never
	// its length or prefix either — those narrow a search.
	s.log.Info("session configured",
		"provider", string(req.Provider),
		"model", req.Model,
		"endpoint", endpoint,
		"thinking_effort", effort,
		"has_key", req.APIKey != "",
	)

	writeJSON(w, http.StatusOK, sessionResponse{
		OK:             true,
		Provider:       req.Provider,
		Model:          req.Model,
		ThinkingEffort: effort,
		Endpoint:       endpoint,
	})
}

// currentSession returns the configured session, or nil.
func (s *Server) currentSession() *session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.session
}
