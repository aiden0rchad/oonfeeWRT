package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/firmware"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

type FirmwareChecker interface {
	Check(context.Context, firmware.Identity) firmware.Result
}

type firmwareDevice struct {
	DeviceID       int64             `json:"device_id"`
	Name           string            `json:"name"`
	Status         string            `json:"status"`
	ManagementMode string            `json:"management_mode"`
	Identity       firmware.Identity `json:"identity"`
	IdentitySource string            `json:"identity_source"`
}

func firmwareIdentity(device *store.Device) firmware.Identity {
	var caps capability.Registry
	if json.Unmarshal([]byte(device.CapsJSON), &caps) != nil {
		return firmware.Identity{Release: device.FWRelease}
	}
	release := caps.Board.Release
	if device.FWRelease != "" && release != device.FWRelease {
		// The poll has seen different firmware since the stored capability
		// probe. Do not combine a new version with stale board/target facts.
		return firmware.Identity{Release: device.FWRelease}
	}
	return firmware.Identity{BoardName: caps.Board.BoardName, Target: caps.Board.Target, RootFSType: caps.Board.RootFSType, Release: release}
}

func (s *Server) handleFirmware(w http.ResponseWriter, r *http.Request) {
	devices, err := s.Store.Devices(r.Context())
	if handleStoreErr(w, err, "firmware inventory") {
		return
	}
	out := make([]firmwareDevice, 0, len(devices))
	for _, device := range devices {
		if !device.Adopted() {
			continue
		}
		view := s.viewDevice(device, s.now())
		out = append(out, firmwareDevice{DeviceID: device.ID, Name: device.Name, Status: view.Status,
			ManagementMode: view.ManagementMode, Identity: firmwareIdentity(device), IdentitySource: "stored_capability_probe"})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"devices": out,
		"checking": map[string]string{
			"source_url": firmware.SourceURL, "scope": "same_release_branch",
			"note": "Checks run only when requested. The controller sends the reported target and release to the official OpenWrt download service; it does not contact or change the router.",
		},
		"agent": map[string]any{
			"available": true, "installed_state": "not_observed", "package_path": "deploy/openwrt-agent",
			"note": "Optional read-only rpcd helper source is included for manual SDK packaging. No helper is installed automatically and installation is not detected by this screen.",
		},
		"installation": map[string]any{
			"enabled": false,
			"required_checks": []string{
				"Fresh board, target, filesystem, boot-mode and storage-layout proof on the device.",
				"Downloaded-image checksum and authenticity checks, followed by the device's sysupgrade validation without force.",
				"A saved and verified router configuration backup, package-preservation plan and sufficient temporary memory.",
				"A reviewed per-device recovery procedure, stable power and acknowledged interruption of service.",
				"An opt-in, reauthenticated staging and confirmation workflow with durable audit records and post-reboot verification.",
			},
		},
	})
}

func (s *Server) handleFirmwareCheck(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	device, err := s.Store.DeviceByID(r.Context(), id)
	if handleStoreErr(w, err, "device") {
		return
	}
	if !device.Adopted() {
		writeErr(w, http.StatusConflict, "complete device adoption before checking its stored firmware identity")
		return
	}
	if s.FirmwareChecker == nil {
		writeErr(w, http.StatusServiceUnavailable, "firmware catalogue checking is unavailable")
		return
	}
	result := s.FirmwareChecker.Check(r.Context(), firmwareIdentity(device))
	writeJSON(w, http.StatusOK, map[string]any{"device_id": id, "result": result})
}
