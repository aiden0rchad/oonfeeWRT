package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/reconcile"
	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

const (
	previewBindingVersion = 1
	previewTokenDomain    = "preview-token/v1"
)

// previewState is the private half of a preview. The public result deliberately
// omits passphrases and option values; this state binds Apply to the full input
// and plans without ever returning those values to the browser.
type previewState struct {
	result           *api.PreviewResult
	site             model.Site
	devices          []*store.Device
	siteFingerprint  string
	fleetFingerprint string
	planFingerprints map[int64]string
}

type stableDevice struct {
	ID            int64
	MAC           string
	Host          string
	Port          int
	Scheme        string
	CertFP        string
	HostKeyFP     string
	Name          string
	Role          string
	Functions     []string
	FunctionError string
	AdoptedAt     *int64
	Credential    []byte
	Class         string
	Capabilities  string
	Firmware      string
}

type ownedConfig struct {
	Config       string
	Section      string
	RenderedHash string
}

type devicePlanBinding struct {
	Version         int
	SiteFingerprint string
	Device          stableDevice
	Owned           []ownedConfig
	Plan            *reconcile.DevicePlan
	Error           string
}

type fleetPlanBinding struct {
	Version          int
	SiteFingerprint  string
	FleetFingerprint string
	Plans            []boundPlan
}

type boundPlan struct {
	DeviceID    int64
	Fingerprint string
}

func stableDeviceState(dev *store.Device) stableDevice {
	return stableDevice{
		ID: dev.ID, MAC: dev.MAC, Host: dev.Host, Port: dev.Port,
		Scheme: dev.Scheme, CertFP: dev.CertFP, HostKeyFP: dev.HostKeyFP,
		Name: dev.Name, Role: dev.Role, Functions: append([]string(nil), dev.Functions...),
		FunctionError: dev.FunctionError, AdoptedAt: dev.AdoptedAt,
		Credential: append([]byte(nil), dev.CredEnc...), Class: dev.Class,
		Capabilities: dev.CapsJSON, Firmware: dev.FWRelease,
	}
}

func ownedConfigState(rows []store.OwnedSection) []ownedConfig {
	out := make([]ownedConfig, 0, len(rows))
	for _, row := range rows {
		out = append(out, ownedConfig{
			Config: row.Config, Section: row.Section, RenderedHash: row.RenderedHash,
		})
	}
	return out
}

// stateFingerprint streams JSON straight into SHA-256. Secret fields are part
// of the binding, but never exist in a token, response, error, or log line.
func stateFingerprint(value any) (string, error) {
	h := sha256.New()
	enc := json.NewEncoder(h)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return "", fmt.Errorf("could not bind preview state: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func siteStateFingerprint(site model.Site) (string, error) {
	return stateFingerprint(struct {
		Version int
		Site    model.Site
	}{previewBindingVersion, site})
}

func fleetStateFingerprint(devices []*store.Device) (string, error) {
	state := make([]stableDevice, 0, len(devices))
	for _, dev := range applyOrder(devices) {
		if dev.Adopted() {
			state = append(state, stableDeviceState(dev))
		}
	}
	return stateFingerprint(struct {
		Version int
		Devices []stableDevice
	}{previewBindingVersion, state})
}

func planStateFingerprint(siteFingerprint string, dev *store.Device,
	owned []store.OwnedSection, plan *reconcile.DevicePlan, planErr error) (string, error) {
	errText := ""
	if planErr != nil {
		errText = planErr.Error()
	}
	return stateFingerprint(devicePlanBinding{
		Version: previewBindingVersion, SiteFingerprint: siteFingerprint,
		Device: stableDeviceState(dev), Owned: ownedConfigState(owned),
		Plan: plan, Error: errText,
	})
}

func previewToken(keys *secrets.Keeper, siteFingerprint, fleetFingerprint string,
	plans map[int64]string, devices []*store.Device) (string, error) {
	bound := make([]boundPlan, 0, len(plans))
	for _, dev := range applyOrder(devices) {
		if fp, ok := plans[dev.ID]; ok {
			bound = append(bound, boundPlan{DeviceID: dev.ID, Fingerprint: fp})
		}
	}
	digest, err := stateFingerprint(fleetPlanBinding{
		Version: previewBindingVersion, SiteFingerprint: siteFingerprint,
		FleetFingerprint: fleetFingerprint, Plans: bound,
	})
	if err != nil {
		return "", err
	}
	if keys == nil {
		return "", errors.New("could not authenticate preview token: secrets keeper is unavailable")
	}
	// The unkeyed digest commits to secret-bearing site and plan state. Returning
	// it would let a browser-side token become an offline passphrase verifier.
	keyed, err := keys.HMACSHA256(previewTokenDomain, []byte(digest))
	if err != nil {
		return "", fmt.Errorf("could not authenticate preview token: %w", err)
	}
	return "pv1_" + hex.EncodeToString(keyed), nil
}
