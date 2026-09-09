package server

import (
	"encoding/json"
	"net/http"

	"github.com/srikary12/starch/internal/prompt"
)

// maxPresetBody bounds a PUT. Generous for sixty-odd instructions, small
// enough that a runaway client cannot make the daemon allocate without limit.
const maxPresetBody = 256 << 10

type presetsBody struct {
	Presets []prompt.Preset `json:"presets"`
}

func (s *Server) handleGetPresets(w http.ResponseWriter, r *http.Request) {
	// Load stats the file and re-parses only when it changed, so this reflects
	// a hand edit made a second ago without anything having to be restarted.
	writeJSON(w, http.StatusOK, s.presets.Load())
}

func (s *Server) handlePutPresets(w http.ResponseWriter, r *http.Request) {
	var body presetsBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPresetBody)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, ErrBadRequest, "Could not read the preset list.")
		return
	}

	if err := s.presets.Save(body.Presets); err != nil {
		// The validation messages are written for a person — "preset ids must
		// be unique: \"concise\" appears twice" — so they pass straight
		// through rather than being replaced with something vaguer.
		writeError(w, http.StatusBadRequest, ErrBadRequest, err.Error())
		return
	}

	s.log.Info("presets replaced", "count", len(body.Presets))
	writeJSON(w, http.StatusOK, s.presets.Load())
}
