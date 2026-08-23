package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
)

// Reprober re-runs the capability probe against an adopted device.
//
// Separate from Enroller because it needs neither of the things adoption needs
// from an operator — no device administrator credential, no ACL write. It uses
// the controller's own login and only reads.
type Reprober interface {
	Reprobe(ctx context.Context, deviceID int64) (*ReprobeResult, error)
}

// ErrReprobeBusy reports that a probe is already running for this device, or
// that an automatic one ran very recently.
//
// Its own error because it is not a failure: nothing went wrong, the answer is
// "not now". Reporting it as a probe failure would send an operator looking for
// a device problem that does not exist.
var ErrReprobeBusy = errors.New("a capability probe for this device is already " +
	"running or ran very recently")

// ReprobeResult is what one re-probe learned.
type ReprobeResult struct {
	DeviceID int64  `json:"device_id"`
	Name     string `json:"name"`
	Summary  string `json:"summary"`

	// Changes is the difference from the previous record, classified by what a
	// reader may conclude from it — see capability.Effect. The classification
	// is the point: "we can no longer check this" and "the device no longer has
	// this" look identical in the raw states and mean entirely different things.
	Changes  []capability.Change  `json:"changes"`
	Registry *capability.Registry `json:"capabilities"`

	// Unchanged says plainly that a probe ran and found nothing. Without it the
	// UI shows an empty list, which reads as a failure rather than as a result.
	Unchanged bool `json:"unchanged"`

	// RoleFit is where this device's role and its hardware disagree, as the
	// probe just found it. Reported here as well as at adoption because that is
	// when it can CHANGE: a device that loses a radio has not only lost a
	// radio, it has stopped matching the role it was adopted under, and the
	// diff alone does not say so.
	RoleFit []string `json:"role_fit,omitempty"`
}

// Actionable counts the changes that alter what may be rendered or sent, so a
// client can distinguish "this device is different" from "we can see less of
// it" without re-implementing the classification.
func (r *ReprobeResult) Actionable() int {
	return len(capability.Actionable(r.Changes))
}

func (s *Server) handleReprobe(w http.ResponseWriter, r *http.Request) {
	if s.Reprobe == nil {
		writeErr(w, http.StatusServiceUnavailable,
			"capability probing is not available")
		return
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	release, ok := s.beginOperation(w, operationCapability)
	if !ok {
		return
	}
	defer release()
	// A successful probe replaces the capability record used to render plans.
	if !s.lockSiteMutation(w, r) {
		return
	}
	defer s.siteMu.Unlock()
	res, err := s.Reprobe.Reprobe(r.Context(), id)
	switch {
	case errors.Is(err, ErrReprobeBusy):
		// 429, not 500: the request was fine and retrying later will work.
		writeErr(w, http.StatusTooManyRequests, err.Error())
		return
	case err != nil:
		// 502: the failure is the device's or the network's, not the caller's.
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device_id":  res.DeviceID,
		"name":       res.Name,
		"summary":    res.Summary,
		"unchanged":  res.Unchanged,
		"changes":    res.Changes,
		"actionable": res.Actionable(),
		// The registry itself, so the device screen can update without a
		// second round trip.
		"capabilities": res.Registry,
		"role_fit":     res.RoleFit,
		"note": "a capability that became unobservable has NOT been lost: the " +
			"check was refused, usually by a narrowed ACL, and the device may " +
			"be unchanged. Only \"gained\" and \"lost\" describe the hardware",
	})
}
