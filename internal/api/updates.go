package api

import (
	"context"
	"net/http"

	"github.com/aiden0rchad/oonfeewrt/internal/updates"
)

type UpdateChecker interface {
	Check(context.Context, string) updates.Status
}

func (s *Server) handleUpdates(w http.ResponseWriter, r *http.Request) {
	if s.UpdateChecker == nil {
		writeJSON(w, http.StatusOK, updates.Status{
			State: updates.StateUnavailable, CurrentVersion: s.ControllerVersion,
			Message: "Controller release checking is unavailable. This installation is not being reported as current.",
			Error:   &updates.StatusError{Code: "checker_unavailable", Message: "Controller release checking is unavailable."},
		})
		return
	}
	writeJSON(w, http.StatusOK, s.UpdateChecker.Check(r.Context(), s.ControllerVersion))
}
