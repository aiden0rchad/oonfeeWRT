package api

import (
	"context"
	"net/http"

	"github.com/aiden0rchad/oonfeewrt/internal/updates"
)

type UpdateChecker interface {
	Check(context.Context, string) updates.Status
	Refresh(context.Context, string) updates.Status
}

func (s *Server) handleUpdates(w http.ResponseWriter, r *http.Request) {
	s.writeUpdates(w, r, false)
}

func (s *Server) handleCheckUpdates(w http.ResponseWriter, r *http.Request) {
	s.writeUpdates(w, r, true)
}

func (s *Server) writeUpdates(w http.ResponseWriter, r *http.Request, refresh bool) {
	var status updates.Status
	if s.UpdateChecker == nil {
		status = updates.Status{
			State: updates.StateUnavailable, CurrentVersion: s.ControllerVersion,
			Message: "Controller release checking is unavailable. This installation is not being reported as current.",
			Error:   &updates.StatusError{Code: "checker_unavailable", Message: "Controller release checking is unavailable."},
		}
	} else if refresh {
		status = s.UpdateChecker.Refresh(r.Context(), s.ControllerVersion)
	} else {
		status = s.UpdateChecker.Check(r.Context(), s.ControllerVersion)
	}
	status.InstallKind = s.ControllerInstallKind
	switch status.InstallKind {
	case "container", "binary":
	default:
		status.InstallKind = "unknown"
	}
	writeJSON(w, http.StatusOK, status)
}
