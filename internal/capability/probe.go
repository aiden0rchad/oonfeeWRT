package capability

import (
	"context"
	"errors"
	"fmt"
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
	case isNotFound(err):
		r.Set(FeatFirewall4, Absent) // the binary genuinely is not installed
	default:
		// A transport or protocol failure never reached a device answer.
		// Calling it Absent silently selects the legacy iptables model.
		r.Set(FeatFirewall4, NotObservable)
		r.Note("firewall4 undetermined: %v", err)
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
func readSurvey(ctx context.Context, c *ubus.Client, dev string) (surveyRow, error) {
	var out struct {
		Results []surveyRow `json:"results"`
	}
	if err := c.Call(ctx, "iwinfo", "survey", map[string]any{"device": dev}, &out); err != nil {
		return surveyRow{}, err
	}
	if len(out.Results) == 0 {
		return surveyRow{}, nil // answered, with nothing to report
	}
	best := out.Results[0]
	for _, row := range out.Results[1:] {
		if row.ActiveTime > best.ActiveTime {
			best = row
		}
	}
	return best, nil
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
	surveyUnreachable := false
	for _, dev := range devs.Devices {
		radio := Radio{Device: dev, SurveyUsest: Absent}

		info, infoErr := readInfo(ctx, c, dev)
		if infoErr == nil {
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
		first, surveyErr := readSurvey(ctx, c, dev)
		if surveyErr != nil {
			// Refused is not "this driver has no survey". Record why, and let
			// the aggregate below stay NotObservable rather than Absent.
			if isDenied(surveyErr) {
				surveyUnreachable = true
				r.Note("%s: iwinfo.survey denied; channel utilization "+
					"undetermined rather than absent (%v)", dev, surveyErr)
			}
		} else {
			if first.ActiveTime > 0 {
				surveyOK = Present
				radio.SurveyUsest = Present
			}
			if first.Noise > 0 {
				r.AddQuirk(Quirk{Source: "iwinfo.survey", Field: "noise",
					Reason: "reported unsigned (161 for -95); iwinfo.info reports " +
						"the same quantity signed — but see noise:stability, " +
						"switching source fixes only the encoding"})
			}
			absurdTimers := first.RxTime > 1<<40 || first.TxTime > 1<<40
			if absurdTimers {
				r.AddQuirk(Quirk{Source: "iwinfo.survey", Field: "rx_time/tx_time",
					Reason: "uninitialised on this driver (absurd u64); the airtime split is not computable"})
			}

			// The second sample is taken either way, because it answers two
			// questions and only one of them depends on the timers.
			time.Sleep(surveySampleGap)
			second, err2 := readSurvey(ctx, c, dev)
			radio.NoiseStable = checkNoiseStability(ctx, c, r, dev,
				info, infoErr, first, second, err2)
			if !absurdTimers {
				switch {
				case err2 != nil:
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

	if surveyUnreachable && surveyOK != Present {
		surveyOK, splitOK = NotObservable, NotObservable
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
	case err == nil, isNotFound(err):
		// NOT_FOUND means the grant answered and the savedir simply does not
		// exist yet — a clean device. Recording that as Absent would disable
		// the foreign-edit guard on exactly the devices where it works.
		r.Set(FeatPreflightDirty, Present)
	case isDenied(err):
		r.Set(FeatPreflightDirty, NotObservable)
		r.Note("PREFLIGHT cannot detect foreign uncommitted edits: grant " +
			"file.list on /tmp/.uci. uci.changes CANNOT substitute — it only " +
			"sees our own session's staged delta.")
	default:
		r.Set(FeatPreflightDirty, NotObservable)
		r.Note("PREFLIGHT dirty-check undetermined: %v", err)
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
	case isNotFound(err):
		r.Set(FeatAccounting, Absent) // nlbwmon not installed
	default:
		r.Set(FeatAccounting, NotObservable)
		r.Note("per-client accounting undetermined: %v", err)
	}
}

// isNotFound reports a genuine device answer of "that is not here".
func isNotFound(err error) bool {
	var se *ubus.StatusError
	return errors.As(err, &se) && se.Status == ubus.StatusNotFound
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

// noiseJumpDB is the swing between two consecutive survey reads that marks the
// noise floor as untrustworthy. Measured spread on a healthy 5 GHz radio was
// 2 dB, so this is comfortably above normal jitter and well below the 25 dB
// excursions the 2.4 GHz radio produced.
const noiseJumpDB = 6

// noiseDBm normalises iwinfo.survey's noise field, which is reported UNSIGNED
// here while iwinfo.info reports the same quantity signed: 161 means -95.
func noiseDBm(n int) int {
	if n > 0 {
		return n - 256
	}
	return n
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

type radioInfo struct {
	Phy       string   `json:"phy"`
	Channel   int      `json:"channel"`
	Frequency int      `json:"frequency"`
	HWModes   []string `json:"hwmodes"`
	Noise     int      `json:"noise"`
	Hardware  struct {
		Name string `json:"name"`
	} `json:"hardware"`
}

func readInfo(ctx context.Context, c *ubus.Client, dev string) (radioInfo, error) {
	var info radioInfo
	err := c.Call(ctx, "iwinfo", "info", map[string]any{"device": dev}, &info)
	return info, err
}

// checkNoiseStability decides whether this radio's noise floor means anything.
//
// It checks BOTH sources, which is the correction to an earlier belief. The
// documented advice was "iwinfo.survey reports noise unsigned, so read it from
// iwinfo.info instead" — true, and it fixes the encoding. It does not fix
// trustworthiness. Measured 2026-08-13 over 20 samples ~0.35 s apart:
//
//	5 GHz radio:   iwinfo.info  7 dB spread   iwinfo.survey  5 dB spread
//	2.4 GHz radio: iwinfo.info 42 dB spread   iwinfo.survey 46 dB spread
//
// The instability belongs to the radio, not to the method, so switching source
// buys nothing. Both radios run the same driver on the same device, which is
// also why this is recorded per radio: gating the whole device would suppress a
// perfectly good 5 GHz noise floor.
//
// Whether the excursions are a driver defect or genuine bursts on a congested
// band is not settled here — channel busy time did not explain them, but 2.4 GHz
// was uniformly busy. It does not change the conclusion: one reading is not a
// noise floor.
//
// The detector is asymmetric. Firing proves the value moves; two samples
// agreeing proves nothing, so Present here means "not caught misbehaving", never
// "verified stable".
func checkNoiseStability(ctx context.Context, c *ubus.Client, r *Registry, dev string,
	info radioInfo, infoErr error, first, second surveyRow, surveyErr error) State {

	stable := Unknown
	unstable := false

	if surveyErr == nil {
		if jump := abs(noiseDBm(first.Noise) - noiseDBm(second.Noise)); jump >= noiseJumpDB {
			unstable = true
			// Its own field name: the registry dedupes by source+field, and the
			// encoding quirk and this one are different facts about one value.
			// Sharing a key would let whichever fired first discard the other.
			r.AddQuirk(Quirk{Source: "iwinfo.survey", Field: "noise:stability",
				Reason: fmt.Sprintf("moved %d dB between consecutive reads on %s; "+
					"smooth over several samples or show utilization alone", jump, dev)})
		} else {
			stable = Present
		}
	}

	// The same question of the other source, because "read it from iwinfo.info"
	// is the advice this check exists to qualify.
	if infoErr == nil {
		if again, err := readInfo(ctx, c, dev); err == nil {
			if jump := abs(noiseDBm(info.Noise) - noiseDBm(again.Noise)); jump >= noiseJumpDB {
				unstable = true
				r.AddQuirk(Quirk{Source: "iwinfo.info", Field: "noise:stability",
					Reason: fmt.Sprintf("moved %d dB between consecutive reads on %s; "+
						"switching away from iwinfo.survey fixes the sign, not this", jump, dev)})
			} else if stable == Unknown {
				stable = Present
			}
		}
	}

	if unstable {
		r.Note("%s: noise floor is unstable; show RSSI or utilization rather than SNR", dev)
		return Absent
	}
	return stable
}
