package model

import "encoding/json"

// TopologySourceState distinguishes a demonstrated empty source from one that
// could not be observed. A missing row is Unknown as well, but a persisted
// reason lets the UI explain a standing ACL/driver failure.
type TopologySourceState string

const (
	TopologySourceUnknown  TopologySourceState = "unknown"
	TopologySourceEmpty    TopologySourceState = "empty"
	TopologySourceObserved TopologySourceState = "observed"
	TopologySourceError    TopologySourceState = "error"
)

// TopologyEdge is one validity interval in the inferred infrastructure graph.
// Managed-device refs use the canonical inventory MAC (device:<mac>), which
// survives unadopt/re-adopt. Observed interface aliases belong in Evidence and
// must not create a duplicate managed-device node.
type TopologyEdge struct {
	ID             int64              `json:"id"`
	ChildNode      string             `json:"child_node"`
	ChildMAC       string             `json:"child_mac,omitempty"`
	ParentNode     string             `json:"parent_node"`
	ParentDeviceID *int64             `json:"parent_device_id,omitempty"`
	ParentPort     string             `json:"parent_port,omitempty"`
	Medium         string             `json:"medium"`
	Confidence     string             `json:"confidence"`
	ValidFrom      int64              `json:"valid_from"`         // Unix milliseconds
	ValidTo        *int64             `json:"valid_to,omitempty"` // exclusive Unix milliseconds
	LastSeen       int64              `json:"last_seen"`          // Unix milliseconds
	Evidence       []TopologyEvidence `json:"evidence"`
	Ambiguities    []string           `json:"ambiguities"`
}

// TopologyEvidence names the observation supporting an inferred edge. Detail
// is source-specific but remains an object; scalar/raw log content is not part
// of this API contract.
type TopologyEvidence struct {
	Kind     string         `json:"kind"`
	Source   string         `json:"source"`
	DeviceID *int64         `json:"device_id,omitempty"`
	Detail   map[string]any `json:"detail"`
}

// TopologySourceObservation is the latest collection outcome for one source.
type TopologySourceObservation struct {
	DeviceID   int64               `json:"device_id"`
	Source     string              `json:"source"`
	State      TopologySourceState `json:"state"`
	Reason     string              `json:"reason,omitempty"`
	ObservedAt int64               `json:"observed_at"` // Unix milliseconds
}

// RadioKey is stable across netifd reloads. Section is the LuCI/UCI
// wifi-device map key (for example radio0), never a phy or interface name.
type RadioKey struct {
	DeviceID int64  `json:"device_id"`
	Section  string `json:"radio_key"`
}

type RadioScanStatus string

const (
	RadioScanPending   RadioScanStatus = "pending"
	RadioScanRunning   RadioScanStatus = "running"
	RadioScanCompleted RadioScanStatus = "completed"
	RadioScanFailed    RadioScanStatus = "failed"
)

// RadioScan is one operator-triggered scan of one configured radio.
type RadioScan struct {
	ID         int64           `json:"id"`
	Radio      RadioKey        `json:"radio"`
	StartedAt  int64           `json:"started_at"`            // Unix milliseconds
	FinishedAt *int64          `json:"finished_at,omitempty"` // Unix milliseconds
	Status     RadioScanStatus `json:"status"`
	Detail     json.RawMessage `json:"detail"`
}

// RadioScanBSS is a visible BSS observation. It intentionally has no inferred
// DFS field: iwinfo freqlist's restricted bit is not proof of radar/DFS state.
type RadioScanBSS struct {
	ScanID  int64  `json:"scan_id"`
	BSSID   string `json:"bssid"`
	SSID    string `json:"ssid"`
	MHz     int    `json:"mhz"`
	Channel int    `json:"channel"`
	Signal  *int   `json:"signal,omitempty"`
	Width   *int   `json:"width,omitempty"`
}
