package model

import (
	"fmt"
	"strings"
)

// ManagementMode controls whether the site model may configure a device.
// Monitoring, inventory, and telemetry remain available in both modes.
type ManagementMode string

const (
	ManagementModeManaged     ManagementMode = "managed"
	ManagementModeMonitorOnly ManagementMode = "monitor_only"
)

// ParseManagementMode validates a device's configuration boundary. Empty is
// the legacy/default contract and therefore means managed.
func ParseManagementMode(raw string) (ManagementMode, error) {
	switch mode := ManagementMode(strings.ToLower(strings.TrimSpace(raw))); mode {
	case "", ManagementModeManaged:
		return ManagementModeManaged, nil
	case ManagementModeMonitorOnly:
		return ManagementModeMonitorOnly, nil
	default:
		return "", fmt.Errorf("%q is not a management mode; valid modes are managed and monitor_only", raw)
	}
}

func (m ManagementMode) Configures() bool { return m == ManagementModeManaged }
