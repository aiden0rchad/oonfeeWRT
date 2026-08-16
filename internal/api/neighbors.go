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
	res, err := s.Neighbours(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}
