package server

import (
	"net/http"

	"github.com/srikary12/starch/internal/catalog"
)

// handleModels serves the curated provider and model table.
//
// No session and no API key: this is what a settings window reads to populate
// its pickers, and that window is most often open precisely because there is
// no key yet. Requiring one would mean the pickers stay empty until after the
// thing they exist to configure has been configured.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, catalog.Builtin())
}
