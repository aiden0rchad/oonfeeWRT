package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/onair"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

// Verifying that the fleet is actually on the air.
//
// internal/onair holds the rules and the reasoning; this file does the two
// device-facing halves: ask each AP what it believes it is broadcasting, and
// ask each radio what it can hear.
//
// # Why this is operator-initiated and never polled
//
// A scan takes the radio off-channel. On a radio that is serving clients that
// is a real, if brief, interruption — and unlike the capability probe, which is
// merely expensive, this one is felt by whoever is using the network. It is
// also not information that changes minute to minute: a BSS that is on the air
// stays on the air until something breaks.
//
// So it runs when somebody asks, like the re-probe, and the request says what
// it costs. Putting this on the poll loop would trade a permanent small
// degradation of everyone's wifi for an answer nobody is reading.

// onAirTimeout bounds the whole check. A scan is seconds per radio and the
// fleet is scanned in series, so this is generous — but bounded, because a
// device that has wedged mid-scan must not hold the request open forever.
const onAirTimeout = 120 * time.Second

// VerifyOnAir asks every AP what it claims to broadcast, asks every radio what
// it can hear, and compares the two.
func (d *Daemon) VerifyOnAir(ctx context.Context) (*api.OnAirResult, error) {
	ctx, cancel := context.WithTimeout(ctx, onAirTimeout)
	defer cancel()

	devices, err := d.Store.Devices(ctx)
	if err != nil {
		return nil, err
	}

	var claimed []onair.BSS
	var scans []onair.Scan
	out := &api.OnAirResult{Devices: []api.OnAirDevice{}}

	for _, dev := range devices {
		if !dev.Adopted() || !model.RoleOf(dev.Role).Wireless() {
			continue
		}
		row := api.OnAirDevice{DeviceID: dev.ID, Name: dev.Name}

		c, err := d.Connect(ctx, dev)
		if err != nil {
			row.Error = fmt.Sprintf("could not reach this device: %v", err)
			out.Devices = append(out.Devices, row)
			continue
		}

		ifaces := wirelessIfaces(ctx, c)
		claimed = append(claimed, claimedBSSes(ctx, c, dev, ifaces)...)

		scan := onair.Scan{DeviceID: dev.ID, Name: dev.Name}
		for _, iface := range ifaces {
			heard, band, err := scanFrom(ctx, c, iface)
			if err != nil {
				// Recorded, not fatal. A radio that cannot scan is the normal
				// case on a busy 5 GHz AP — measured on the reference C6,
				// whose 2.4 GHz radio returned 20 BSSes while its 5 GHz radio
				// returned zero. What must NOT happen is that silence counting
				// as evidence, which is what BandsCovered prevents.
				row.ScanErrors = append(row.ScanErrors,
					fmt.Sprintf("%s: %v", iface, err))
				continue
			}
			if len(heard) == 0 && band == "" {
				continue
			}
			if band != "" {
				scan.BandsCovered = append(scan.BandsCovered, band)
			}
			scan.Heard = append(scan.Heard, heard...)
			row.Scanned = append(row.Scanned, iface)
		}
		c.Close()

		row.Heard = len(scan.Heard)
		scans = append(scans, scan)
		out.Devices = append(out.Devices, row)
	}

	for _, r := range onair.Check(claimed, scans) {
		out.Results = append(out.Results, api.OnAirBSS{
			DeviceID: r.BSS.DeviceID, Name: r.BSS.Name, Iface: r.BSS.Iface,
			BSSID: r.BSS.BSSID, SSID: r.BSS.SSID, Band: r.BSS.Band,
			Verdict: string(r.Verdict), HeardSSID: r.HeardSSID,
			Witnesses: r.Witnesses, Reason: r.Reason, Fault: r.Fault(),
		})
		if r.Fault() {
			out.Faults++
		}
	}
	if len(out.Results) == 0 {
		out.Note = "no adopted device reported a broadcasting interface, so " +
			"there is nothing to verify"
	}
	return out, nil
}

// wirelessIfaces lists the device's wireless interfaces.
func wirelessIfaces(ctx context.Context, c *ubus.Client) []string {
	var devs struct {
		Devices []string `json:"devices"`
	}
	if err := c.Call(ctx, "iwinfo", "devices", nil, &devs); err != nil {
		return nil
	}
	return devs.Devices
}

// claimedBSSes asks each interface what IT believes it is broadcasting.
//
// Deliberately from hostapd, which is the source that was wrong for fourteen
// hours. That is the point: this is the claim under test, not the evidence.
func claimedBSSes(ctx context.Context, c *ubus.Client, dev *store.Device,
	ifaces []string) []onair.BSS {

	var out []onair.BSS
	for _, iface := range ifaces {
		var st struct {
			SSID  string `json:"ssid"`
			BSSID string `json:"bssid"`
			Freq  int    `json:"freq"`
		}
		if err := c.Call(ctx, "hostapd."+iface, "get_status", nil, &st); err != nil {
			continue
		}
		if st.SSID == "" || st.BSSID == "" {
			continue
		}
		out = append(out, onair.BSS{
			DeviceID: dev.ID, Name: dev.Name, Iface: iface,
			BSSID: st.BSSID, SSID: st.SSID, Band: bandOf(st.Freq),
		})
	}
	return out
}

// scanFrom runs one scan and returns what it heard plus the band it covered.
//
// An empty band means the scan produced nothing usable, and the caller must not
// record it as coverage — a radio that could not scan has not established that
// anything is absent.
func scanFrom(ctx context.Context, c *ubus.Client, iface string) ([]onair.Heard, string, error) {
	var res struct {
		Results []struct {
			BSSID string `json:"bssid"`
			// Absent entirely for a hidden network, which is why this is a
			// pointer-free string and emptiness is handled below rather than
			// treated as a name.
			SSID string `json:"ssid"`
			MHz  int    `json:"mhz"`
		} `json:"results"`
	}
	if err := c.Call(ctx, "iwinfo", "scan", map[string]any{"device": iface}, &res); err != nil {
		return nil, "", err
	}
	if len(res.Results) == 0 {
		// Measured on the reference C6: a 5 GHz radio serving an AP returns an
		// empty result rather than an error. That is a scan which did not
		// happen, not a quiet band, and reporting coverage for it would turn
		// silence into evidence.
		return nil, "", nil
	}

	var heard []onair.Heard
	band := ""
	for _, r := range res.Results {
		if band == "" {
			band = bandOf(r.MHz)
		}
		if r.SSID == "" {
			// A beacon carrying no SSID is what a hidden network looks like.
			// It can neither confirm nor refute a name, so it is not evidence
			// either way and is dropped rather than compared against "".
			continue
		}
		heard = append(heard, onair.Heard{
			BSSID: r.BSSID, SSID: r.SSID, Band: bandOf(r.MHz),
		})
	}
	return heard, band, nil
}

// bandOf maps a frequency in MHz to the band names the site model uses.
func bandOf(mhz int) string {
	switch {
	case mhz >= 2400 && mhz < 2500:
		return string(model.Band2G)
	case mhz >= 5000 && mhz < 5900:
		return string(model.Band5G)
	case mhz >= 5925:
		return string(model.Band6G)
	}
	return ""
}

// OnAirReport is the API adapter.
func (d *Daemon) OnAirReport(ctx context.Context) (*api.OnAirResult, error) {
	return d.VerifyOnAir(ctx)
}
