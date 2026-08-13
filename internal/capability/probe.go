package capability

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

// Probe interrogates a device once, at adoption, and whenever firmware changes.
//
// It mirrors tools/probe.py, minus the write tests. The cardinal rule, learned
// the hard way there: a refused check yields NotObservable, never Absent.
func Probe(ctx context.Context, c *ubus.Client) (*Registry, error) {
	r := NewRegistry()

	if err := probeBoard(ctx, c, r); err != nil {
		return nil, err
	}
	r.Class = classify(r.Board)

	probeBatching(ctx, c, r)
	probeSwitchAndFirewall(ctx, c, r)
	probeRadios(ctx, c, r)
	probePreflight(ctx, c, r)
	probeAccounting(ctx, c, r)

	return r, nil
}

func probeBoard(ctx context.Context, c *ubus.Client, r *Registry) error {
	var b struct {
		Model      string `json:"model"`
		BoardName  string `json:"board_name"`
		Kernel     string `json:"kernel"`
		RootFSType string `json:"rootfs_type"`
		Release    struct {
			Target      string `json:"target"`
			Description string `json:"description"`
		} `json:"release"`
	}
	if err := c.Call(ctx, "system", "board", nil, &b); err != nil {
		return err
	}
	r.Board = Board{
		Model: b.Model, BoardName: b.BoardName, Kernel: b.Kernel,
		Target: b.Release.Target, Release: b.Release.Description,
		RootFSType: b.RootFSType,
	}
	return nil
}

// classify maps a target to a DEVICE-BUDGET class. Marketing names are useless
// here — "AX3000" spans both MT7621 (class C) and MT7981 (class B) — so this
// keys on the SoC target only.
func classify(b Board) Class {
	t := strings.ToLower(b.Target)
	switch {
	case strings.HasPrefix(t, "mvebu"):
		return ClassA
	case strings.Contains(t, "filogic"), strings.Contains(t, "mt7981"),
		strings.Contains(t, "mediatek/filogic"):
		return ClassB
	case strings.Contains(t, "ramips/mt7621"), strings.Contains(t, "mt7621"):
		return ClassC
	}
	return ClassUnknown
}

// probeBatching confirms the uhttpd build accepts array bodies. Recorded at
// adoption so the collector can fall back to sequential calls rather than
// discovering it mid-poll.
func probeBatching(ctx context.Context, c *ubus.Client, r *Registry) {
	res, err := c.Batch(ctx, []ubus.Invocation{
		{Object: "system", Method: "info"},
		{Object: "system", Method: "board"},
	})
	switch {
	case err != nil:
		r.Set(FeatBatching, NotObservable)
		r.Note("batching check failed: %v", err)
	case len(res) == 2 && res[0].Err == nil && res[1].Err == nil:
		r.Set(FeatBatching, Present)
	default:
		r.Set(FeatBatching, Absent)
		r.Note("device did not answer a 2-call batch correctly; polls will be sequential")
	}
}

// probeSwitchAndFirewall uses sources that need no filesystem grant.
//
// DSA comes from luci-rpc.getNetworkDevices, which the poll already fetches and
// which tags user ports devtype "dsa". The /sys route looks narrower but is
// not: rpcd canonicalises paths, so a /sys/class/net/* grant never matches
// (those are symlinks into /sys/devices), and widening it to /sys/devices/*
// hands over a subtree because '*' crosses '/'.
func probeSwitchAndFirewall(ctx context.Context, c *ubus.Client, r *Registry) {
	var devs map[string]struct {
		DevType string `json:"devtype"`
		Parent  string `json:"parent"`
	}
	if err := c.Call(ctx, "luci-rpc", "getNetworkDevices", nil, &devs); err != nil {
		r.Set(FeatDSA, NotObservable)
		r.Note("DSA undetermined: luci-rpc.getNetworkDevices denied (%v)", err)
	} else {
		found := Absent
		for _, d := range devs {
			if d.DevType == "dsa" {
				found = Present
				break
			}
		}
		r.Set(FeatDSA, found)
	}

	// firewall4 via the one nft command the ACL already grants. An exec that is
	// refused is NotObservable; one that runs and fails is Absent.
	var out struct {
		Code int `json:"code"`
	}
	err := c.Call(ctx, "file", "exec", map[string]any{
		"command": "/usr/sbin/nft",
		"params":  []string{"--terse", "--json", "list", "ruleset"},
	}, &out)
	switch {
	case err == nil && out.Code == 0:
		r.Set(FeatFirewall4, Present)
	case err == nil:
		r.Set(FeatFirewall4, Absent)
		r.Note("nft present but returned %d; assuming legacy iptables", out.Code)
	case isDenied(err):
		r.Set(FeatFirewall4, NotObservable)
		r.Note("firewall4 undetermined: exec of nft not granted (%v)", err)
	default:
		r.Set(FeatFirewall4, Absent)
	}
}

// surveySampleGap is how long to wait between the two survey reads used to
// decide whether rx_time/tx_time are live counters or dead fields.
const surveySampleGap = 1200 * time.Millisecond

type surveyRow struct {
	MHz        int    `json:"mhz"`
	Noise      int    `json:"noise"`
	ActiveTime int64  `json:"active_time"`
	BusyTime   int64  `json:"busy_time"`
	RxTime     uint64 `json:"rx_time"`
	TxTime     uint64 `json:"tx_time"`
}

// readSurvey returns the in-use row — a 5 GHz radio reports one row per
// frequency and only the active one carries counters, so rows[0] is often the
// empty one.
func readSurvey(ctx context.Context, c *ubus.Client, dev string) (surveyRow, bool) {
	var out struct {
		Results []surveyRow `json:"results"`
	}
	if err := c.Call(ctx, "iwinfo", "survey", map[string]any{"device": dev}, &out); err != nil {
		return surveyRow{}, false
	}
	if len(out.Results) == 0 {
		return surveyRow{}, false
	}
	best := out.Results[0]
	for _, row := range out.Results[1:] {
		if row.ActiveTime > best.ActiveTime {
			best = row
		}
	}
	return best, true
}

// advancesProportionately reports whether rx_time+tx_time grew by enough of the
// busy-time growth to be a real accounting of it. The threshold is deliberately
// generous: we are separating "tracks reality" from "does not move".
func advancesProportionately(a, b surveyRow) bool {
	dBusy := b.BusyTime - a.BusyTime
	if dBusy <= 0 {
		return false
	}
	dRx := int64(b.RxTime - a.RxTime)
	dTx := int64(b.TxTime - a.TxTime)
	return (dRx+dTx)*10 >= dBusy
}

// probeRadios records per-radio capability and the mwlwifi quirks.
func probeRadios(ctx context.Context, c *ubus.Client, r *Registry) {
	var devs struct {
		Devices []string `json:"devices"`
	}
	if err := c.Call(ctx, "iwinfo", "devices", nil, &devs); err != nil {
		r.Set(FeatSurvey, NotObservable)
		r.Set(FeatAirtimeSplit, NotObservable)
		r.Set(FeatHostapdControl, NotObservable)
		r.Note("radios undetermined: iwinfo.devices denied (%v)", err)
		return
	}

	surveyOK, splitOK := Absent, Absent
	for _, dev := range devs.Devices {
		radio := Radio{Device: dev, SurveyUsest: Absent}

		var info struct {
			Phy       string   `json:"phy"`
			Channel   int      `json:"channel"`
			Frequency int      `json:"frequency"`
			HWModes   []string `json:"hwmodes"`
			Noise     int      `json:"noise"`
			Hardware  struct {
				Name string `json:"name"`
			} `json:"hardware"`
		}
		if err := c.Call(ctx, "iwinfo", "info", map[string]any{"device": dev}, &info); err == nil {
			radio.Phy, radio.Channel = info.Phy, info.Channel
			radio.Frequency, radio.HWModes = info.Frequency, info.HWModes
			radio.Hardware = info.Hardware.Name
		}

		// Sampled TWICE. A single reading cannot tell a usable counter from a
		// dead one: on mwlwifi rx_time sits at 0 forever and tx_time creeps by
		// a couple of ms, while active_time climbs ~4s per sample. Both fields
		// are present, correctly typed and plausible — and useless. Feeding
		// them into (busy - rx - tx)/active yields busy/active wearing an
		// "interference" label, which is exactly the confidently-wrong number
		// UI-SPEC §7 forbids.
		first, ok1 := readSurvey(ctx, c, dev)
		if ok1 {
			if first.ActiveTime > 0 {
				surveyOK = Present
				radio.SurveyUsest = Present
			}
			if first.Noise > 0 {
				r.AddQuirk(Quirk{Source: "iwinfo.survey", Field: "noise",
					Reason: "reported unsigned (161 for -95); take noise from iwinfo.info"})
			}
			if first.RxTime > 1<<40 || first.TxTime > 1<<40 {
				r.AddQuirk(Quirk{Source: "iwinfo.survey", Field: "rx_time/tx_time",
					Reason: "uninitialised on this driver (absurd u64); the airtime split is not computable"})
			} else {
				time.Sleep(surveySampleGap)
				second, ok2 := readSurvey(ctx, c, dev)
				switch {
				case !ok2:
					// leave splitOK as-is; we could not tell
				case second.BusyTime <= first.BusyTime:
					// Idle channel: this sample proves nothing either way.
				case !advancesProportionately(first, second):
					// Present, typed, plausible — and not tracking reality. On
					// mwlwifi rx_time never moves and tx_time crept 2ms while
					// busy_time advanced ~3000ms, which would make the split a
					// rounding error masquerading as a measurement.
					r.AddQuirk(Quirk{Source: "iwinfo.survey", Field: "rx_time/tx_time",
						Reason: "do not track busy time (rx+tx advanced <10% of busy); the airtime split is not computable"})
				default:
					splitOK = Present
				}
			}
		}

		// hostapd is the cheap per-AP source; its presence also gates the
		// per-client reconnect/block actions.
		if err := c.Call(ctx, "hostapd."+dev, "get_status", nil, nil); err == nil {
			r.Set(FeatHostapdControl, Present)
		} else if r.State(FeatHostapdControl) == Unknown {
			if isDenied(err) {
				r.Set(FeatHostapdControl, NotObservable)
			} else {
				r.Set(FeatHostapdControl, Absent)
			}
		}

		r.Radios = append(r.Radios, radio)
	}

	r.Set(FeatSurvey, surveyOK)
	// A recorded quirk on these fields settles it for the whole device: one
	// radio reporting a plausible-looking counter cannot license a metric the
	// driver does not really supply.
	if r.HasQuirk("iwinfo.survey", "rx_time/tx_time") {
		splitOK = Absent
	}
	r.Set(FeatAirtimeSplit, splitOK)
	if splitOK != Present {
		r.Note("interference and the airtime split are gated off: this driver " +
			"does not supply usable rx_time/tx_time. Channel utilization " +
			"(busy/active) is still available.")
	}
}

// probePreflight checks that the apply path can see foreign LuCI/SSH edits.
// Without this grant the "unsaved changes on device" guard silently never
// fires, because uci.changes is scoped to our own session.
func probePreflight(ctx context.Context, c *ubus.Client, r *Registry) {
	err := c.Call(ctx, "file", "list", map[string]any{"path": "/tmp/.uci"}, nil)
	switch {
	case err == nil:
		r.Set(FeatPreflightDirty, Present)
	case isDenied(err):
		r.Set(FeatPreflightDirty, NotObservable)
		r.Note("PREFLIGHT cannot detect foreign uncommitted edits: grant " +
			"file.list on /tmp/.uci. uci.changes CANNOT substitute — it only " +
			"sees our own session's staged delta.")
	default:
		r.Set(FeatPreflightDirty, Absent)
	}
}

func probeAccounting(ctx context.Context, c *ubus.Client, r *Registry) {
	var out struct {
		Code   int    `json:"code"`
		Stdout string `json:"stdout"`
	}
	err := c.Call(ctx, "file", "exec", map[string]any{
		"command": "/usr/sbin/nlbw", "params": []string{"-c", "json", "-g", "mac"},
	}, &out)
	switch {
	case err == nil && out.Code == 0:
		r.Set(FeatAccounting, Present)
		r.Note("per-client accounting available; note nlbwmon's commit_interval " +
			"defaults to 24h, so read after `nlbw -c commit` or you get zeroes.")
	case isDenied(err):
		r.Set(FeatAccounting, NotObservable)
	default:
		r.Set(FeatAccounting, Absent)
	}
}

// isDenied reports a reach problem rather than a device answer: either rpcd
// refused to proxy (-32002) or the object refused the target (status 6).
func isDenied(err error) bool {
	var de *ubus.DeniedError
	if errors.As(err, &de) {
		return true
	}
	var se *ubus.StatusError
	if errors.As(err, &se) {
		return se.Status == ubus.StatusPermissionDenied
	}
	return false
}
