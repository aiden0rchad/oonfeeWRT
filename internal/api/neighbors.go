package api

import "net/http"

// The 802.11k neighbour-list endpoint.
//
// One POST, no GET. The reconciliation reads the fleet before it decides
// anything, so a "what would this do" endpoint would cost the same requests as
// doing it — and unlike a UCI apply there is nothing to preview *against*,
// because the change cannot be rolled back or confirmed and cannot make a
// device unhealthy. The result of doing it is the report.

// NeighbourResult is what one distribution cycle did.
type NeighbourResult struct {
	// SSIDs are the managed WLANs that asked for 802.11k. Reported even when
	// the run changed nothing, because "which networks does this apply to" is
	// the first question anyone reading a zero has.
	SSIDs []string `json:"ssids"`

	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`

	Devices []NeighbourDevice `json:"devices"`

	// Note explains an empty run. A result of all zeroes with no sentence is
	// indistinguishable from a broken feature.
	Note string `json:"note,omitempty"`
}

// NeighbourDevice is one device's part in a cycle.
type NeighbourDevice struct {
	DeviceID int64  `json:"device_id"`
	Name     string `json:"name"`
	Error    string `json:"error,omitempty"`

	// Skipped is why this device took no part, when that is a standing
	// property rather than a failure — an ACL that predates the feature, a
	// hostapd without RRM, no managed WLAN on it. Separate from Error because
	// an operator responds to the two differently and a screen that renders
	// both as red teaches people to ignore red.
	Skipped string `json:"skipped,omitempty"`

	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`

	BSSes []NeighbourBSS `json:"bsses,omitempty"`
}

// NeighbourBSS is one BSS's outcome.
type NeighbourBSS struct {
	Iface      string `json:"iface"`
	SSID       string `json:"ssid"`
	BSSID      string `json:"bssid"`
	Neighbours int    `json:"neighbours"`
	Changed    bool   `json:"changed,omitempty"`
	Failed     string `json:"failed,omitempty"`
}

// Neighbours is the handler. Wired at POST /api/v1/roaming/neighbours.
func (s *Server) handleNeighbours(w http.ResponseWriter, r *http.Request) {
	if s.Neighbours == nil {
		writeErr(w, http.StatusServiceUnavailable,
			"this controller has no fleet attached, so there is nothing to "+
				"distribute neighbour lists across")
		return
	}
	release, ok := s.beginOperation(w, operationNeighbourReconcile)
	if !ok {
		return
	}
	defer release()
	res, err := s.Neighbours(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleLastNeighbours reports the most recent cycle WITHOUT running one.
//
// The screen had no way to ask this. It rendered nothing until an operator
// pressed "Distribute now" — on a feature whose own description says it runs
// automatically every fifteen minutes — so the only way to learn whether
// 802.11k was working was to trigger it, which is not an observation. Every
// automatic cycle that had been running all along left no trace anywhere a user
// looks.
func (s *Server) handleLastNeighbours(w http.ResponseWriter, r *http.Request) {
	if s.LastNeighbours == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ran": false})
		return
	}
	res, errText, at, ok := s.LastNeighbours()
	if !ok {
		// Not an error and not "nothing to do": no cycle has run since this
		// controller started, and the first lands within the interval.
		writeJSON(w, http.StatusOK, map[string]any{"ran": false})
		return
	}
	out := map[string]any{"ran": true, "at": at.Unix()}
	if errText != "" {
		out["error"] = errText
	}
	if res != nil {
		out["result"] = res
		// Per-device failures, counted here rather than left for the caller to
		// find. DistributeNeighbours returns a nil error when the CYCLE ran and
		// individual devices failed — their reasons land in res.Devices[].Failed
		// — so a screen reading only the top-level error would report "no
		// errors" for a run in which half the fleet was unreachable.
		// A device counts as failed on a device-level error OR on any BSS whose
		// push failed.
		//
		// Counting only the device level reported "no errors" for a cycle in
		// which an AP was left holding an empty neighbour list: a batch that is
		// DELIVERED and refused per call comes back with a nil batch error, so
		// pushNeighbours records the reason on the BSS and never touches
		// row.Error. The failure is real and complete — that radio tells its
		// clients about no neighbours at all — and it was invisible.
		//
		// Skipped is still not counted. A device that took no part for a
		// standing reason — an ACL predating the feature, no managed WLAN — is
		// not a failure, and counting it as one teaches people to ignore the
		// number.
		var failed int
		for _, d := range res.Devices {
			if d.Error != "" {
				failed++
				continue
			}
			for _, b := range d.BSSes {
				if b.Failed != "" {
					failed++
					break
				}
			}
		}
		out["devices_failed"] = failed
	}
	writeJSON(w, http.StatusOK, out)
}
