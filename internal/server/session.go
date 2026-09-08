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
}

type sessionResponse struct {
	OK       bool       `json:"ok"`
	Provider ProviderID `json:"provider"`
	Model    string     `json:"model"`
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
	s.session = &session{provider: prov, model: req.Model, id: req.Provider, endpoint: endpoint}
	s.mu.Unlock()

	// Deliberately logs the endpoint and model but never the key, and never
	// its length or prefix either — those narrow a search.
	s.log.Info("session configured",
		"provider", string(req.Provider),
		"model", req.Model,
		"endpoint", endpoint,
		"has_key", req.APIKey != "",
	)

	writeJSON(w, http.StatusOK, sessionResponse{
		OK:       true,
		Provider: req.Provider,
		Model:    req.Model,
		Endpoint: endpoint,
	})
}

// currentSession returns the configured session, or nil.
func (s *Server) currentSession() *session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.session
}
