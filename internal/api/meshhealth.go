package api

import "net/http"

// Mesh backhaul health.
//
// A GET, unlike the neighbour endpoint next door, and the difference is worth
// stating: this reads nothing from any device. Every input is the site model, a
// stored record, or the last poll, so a screen may refresh it as often as it
// likes without touching the request budget.

// MeshLink is one backhaul on one device, as the controller understands it.
//
// State is a closed vocabulary computed once in internal/meshlink. The UI
// switches on it and never re-derives health from the other fields — a screen
// that decides for itself what a null peer count means is a second
// implementation of that logic, and the two drift.
type MeshLink struct {
	DeviceID   int64  `json:"device_id"`
	DeviceName string `json:"device_name"`
	MeshID     int    `json:"mesh_id"`
	Name       string `json:"name"`
	Iface      string `json:"iface,omitempty"`

	State string `json:"state"`
	// Tone is how to weight it, decided with the state rather than per screen.
	Tone string `json:"tone"`
	// Reason is always present. A state with no sentence is a code an operator
	// has to look up, and nobody looks it up.
	Reason string `json:"reason"`

	// Peers is null when peers were not counted — which is currently always,
	// and is a real state rather than an omission. Never an empty array: "none"
	// and "not counted" are different answers and this is the field that would
	// blur them.
	Peers []MeshPeer `json:"peers"`
	// Established is null unless peer-link state was actually read.
	Established *int `json:"established"`

	// Actionable separates something to go and fix from something the
	// controller merely could not see. Sent rather than inferred, because
	// rendering the second as the first recreates on screen exactly the
	// collapse the capability model exists to prevent.
	Actionable bool `json:"actionable"`
}

// MeshPeer is one 802.11s neighbour.
type MeshPeer struct {
	MAC string `json:"mac"`
	// Plink is the peer-link state — "ESTAB", "OPN_SNT". Empty when the driver
	// reported none, which is why a bare count is not a health reading.
	Plink      string `json:"plink,omitempty"`
	SignalDBm  *int   `json:"signal_dbm"`
	InactiveMS *int   `json:"inactive_ms"`
}

// MeshHealthResult is every configured backhaul in the site.
type MeshHealthResult struct {
	Links []MeshLink `json:"links"`
	// Note explains an empty list, because zero rows and a broken feature look
	// identical otherwise.
	Note string `json:"note,omitempty"`
}

func (s *Server) handleMeshHealth(w http.ResponseWriter, r *http.Request) {
	if s.MeshHealth == nil {
		writeErr(w, http.StatusServiceUnavailable,
			"this controller has no fleet attached, so there is nothing to "+
				"report backhaul health for")
		return
	}
	res, err := s.MeshHealth(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if len(res.Links) == 0 && res.Note == "" {
		res.Note = "no mesh backhaul is configured on any adopted device. Add a " +
			"mesh under Mesh backhauls and apply it to see its health here"
	}
	writeJSON(w, http.StatusOK, res)
}
