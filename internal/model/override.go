package model

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Per-device overrides: the places a single device is allowed to differ from
// the site model.
//
// # What is overridable, and why the rest is not
//
// The list is deliberately short, and the boundary is the point of the whole
// product. A controller exists to guarantee that certain things are identical
// across every AP — the SSID, the passphrase, the security mode, the roaming
// configuration and the mobility domain derived from it. Those are exactly the
// settings that are miserable to keep consistent by hand and that break in
// confusing ways when they drift: a client that roams between two APs with
// different keys does not fail cleanly, it fails intermittently.
//
// So they are not overridable. Not "overridable with a warning" — absent. An
// escape hatch that can break the one guarantee the system offers is not an
// escape hatch, it is a slow leak, and the first support question it produces
// is "why does WiFi drop when I walk down the hall".
//
// What IS overridable is presentation and capacity per AP: whether a WLAN is
// published here at all, whether it is hidden, whether clients are isolated,
// and how many may associate. Those genuinely vary — a guest network in the
// lobby and not the server room is a real requirement — and none of them can
// desynchronise a client's view of the network it is already on.
//
// # Every deviation is visible
//
// The danger of overrides is not any single one. It is a fleet that drifts
// apart device by device until nobody can say what is actually deployed. So
// overrides are surfaced everywhere the site model is: the preview names them
// per device, and a device carrying any override is marked as deviating.

// OverrideKey identifies one overridable setting.
type OverrideKey string

const (
	// OverrideDisabled stops a WLAN being published on this device.
	OverrideDisabled OverrideKey = "disabled"
	// OverrideHidden suppresses the SSID beacon here.
	OverrideHidden OverrideKey = "hidden"
	// OverrideIsolate stops clients on this AP talking to each other.
	OverrideIsolate OverrideKey = "isolate"
	// OverrideMaxAssoc caps associations on this AP.
	OverrideMaxAssoc OverrideKey = "max_assoc"
)

// Override is one device's deviation from the site model for one WLAN.
type Override struct {
	DeviceID int64
	WLANID   int
	Key      OverrideKey
	Value    string
}

// Path is the storage key, "wlan.<id>.<key>".
func (o Override) Path() string {
	return fmt.Sprintf("wlan.%d.%s", o.WLANID, o.Key)
}

// ParseOverridePath reads a stored path back.
func ParseOverridePath(path string) (wlanID int, key OverrideKey, err error) {
	parts := strings.Split(path, ".")
	if len(parts) != 3 || parts[0] != "wlan" {
		return 0, "", fmt.Errorf("model: %q is not a WLAN override path", path)
	}
	id, err := strconv.Atoi(parts[1])
	if err != nil || id <= 0 {
		return 0, "", fmt.Errorf("model: %q has no valid WLAN id", path)
	}
	k := OverrideKey(parts[2])
	if !k.Valid() {
		return 0, "", fmt.Errorf("model: %q is not an overridable setting. "+
			"Security, SSID and roaming are deliberately not overridable — they "+
			"are what a controller exists to keep identical across every AP", parts[2])
	}
	return id, k, nil
}

// Valid reports whether a key may be overridden at all.
func (k OverrideKey) Valid() bool {
	switch k {
	case OverrideDisabled, OverrideHidden, OverrideIsolate, OverrideMaxAssoc:
		return true
	}
	return false
}

// Bool reads a boolean override value. Anything but "1"/"true" is false, so a
// malformed value fails closed rather than silently enabling something.
func (o Override) Bool() bool {
	return o.Value == "1" || strings.EqualFold(o.Value, "true")
}

// Int reads a numeric override value, reporting whether it parsed.
func (o Override) Int() (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(o.Value))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// Describe renders an override for a human, for the deviation list.
func (o Override) Describe(ssid string) string {
	if ssid == "" {
		ssid = fmt.Sprintf("WLAN %d", o.WLANID)
	}
	switch o.Key {
	case OverrideDisabled:
		if o.Bool() {
			return fmt.Sprintf("%s is not published on this device", ssid)
		}
		return fmt.Sprintf("%s is published on this device even though the site "+
			"model disables it", ssid)
	case OverrideHidden:
		if o.Bool() {
			return fmt.Sprintf("%s does not beacon its name here", ssid)
		}
		return fmt.Sprintf("%s beacons its name here", ssid)
	case OverrideIsolate:
		if o.Bool() {
			return fmt.Sprintf("%s requests client isolation on this access point", ssid)
		}
		return fmt.Sprintf("%s does not request client isolation on this access point", ssid)
	case OverrideMaxAssoc:
		if n, ok := o.Int(); ok && n > 0 {
			return fmt.Sprintf("%s allows at most %d clients here", ssid, n)
		}
		return fmt.Sprintf("%s has no client limit here", ssid)
	}
	return fmt.Sprintf("%s: %s = %s", ssid, o.Key, o.Value)
}

// Overrides is a device's full set, indexed for lookup during a render.
type Overrides map[int64][]Override

// For returns one device's overrides for one WLAN, in stable key order.
func (o Overrides) For(deviceID int64, wlanID int) []Override {
	var out []Override
	for _, ov := range o[deviceID] {
		if ov.WLANID == wlanID {
			out = append(out, ov)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Deviates reports whether a device carries any override at all.
func (o Overrides) Deviates(deviceID int64) bool { return len(o[deviceID]) > 0 }

// Apply folds a device's overrides into a WLAN, returning the effective
// settings for that device and whether the WLAN is published there.
//
// The WLAN is returned by value: overriding must never mutate the site model,
// or the second device rendered would inherit the first device's overrides.
func (o Overrides) Apply(deviceID int64, w WLAN) (effective WLAN, published bool) {
	effective = w
	published = w.Enabled
	for _, ov := range o.For(deviceID, w.ID) {
		switch ov.Key {
		case OverrideDisabled:
			published = !ov.Bool()
		case OverrideHidden:
			effective.Options.Hidden = ov.Bool()
		case OverrideIsolate:
			effective.Options.Isolate = ov.Bool()
		case OverrideMaxAssoc:
			if n, ok := ov.Int(); ok {
				effective.Options.MaxAssoc = n
			}
		}
	}
	return effective, published
}
