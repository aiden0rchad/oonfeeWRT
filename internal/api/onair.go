package api

import "net/http"

// Verifying that the fleet is actually transmitting what it says it is.
//
// A POST, not a GET, and the method is the honest one: this makes every radio
// in the fleet scan, which takes each one off-channel briefly. On a radio
// serving clients that is a real if small interruption, so it happens when
// somebody asks for it and never on a refresh.

// OnAirBSS is one broadcasting interface's verdict.
type OnAirBSS struct {
	DeviceID int64  `json:"device_id"`
	Name     string `json:"name"`
	Iface    string `json:"iface"`
	BSSID    string `json:"bssid"`
	// SSID is what the device CLAIMS to be broadcasting, from hostapd.
	SSID string `json:"ssid"`
	Band string `json:"band"`

	// Verdict is confirmed / mismatched / unheard / not-checked.
	Verdict string `json:"verdict"`
	// HeardSSID is what was actually on the air, when it differs from the
	// claim. This is the field the whole feature exists to be able to fill in.
	HeardSSID string `json:"heard_ssid,omitempty"`
	// Witnesses are the devices whose radios heard this BSSID. "Nobody heard
	// it" and "only the AP in the same room heard it" are different levels of
	// confidence and a reader should be able to tell them apart.
	Witnesses []int64 `json:"witnesses,omitempty"`
	Reason    string  `json:"reason"`
	// Fault is true ONLY for a mismatch. Unheard and not-checked are absences
	// of evidence, and rendering those as faults is how a screen teaches
	// somebody to ignore it.
	Fault bool `json:"fault"`
}

// OnAirDevice is one device's part in the check.
type OnAirDevice struct {
	DeviceID int64    `json:"device_id"`
	Name     string   `json:"name"`
	Scanned  []string `json:"scanned,omitempty"`
	Heard    int      `json:"heard"`
	// ScanErrors are radios that could not scan. Reported rather than
	// swallowed: a radio serving a busy AP frequently cannot, and that is the
	// reason a BSS on its band comes back "not checked" instead of "unheard".
	ScanErrors []string `json:"scan_errors,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// OnAirResult is the whole check.
type OnAirResult struct {
	Results []OnAirBSS    `json:"results"`
	Devices []OnAirDevice `json:"devices"`
	Faults  int           `json:"faults"`
	Note    string        `json:"note,omitempty"`
}

func (s *Server) handleOnAir(w http.ResponseWriter, r *http.Request) {
	if s.OnAir == nil {
		writeErr(w, http.StatusServiceUnavailable,
			"this controller has no fleet attached, so there is nothing to verify")
		return
	}
	release, ok := s.beginOperation(w, operationRFScan)
	if !ok {
		return
	}
	defer release()
	res, err := s.OnAir(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}
