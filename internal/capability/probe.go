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
	probeMesh(ctx, c, r)
	probeUplink(ctx, c, r)

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
	probePorts(ctx, c, r)
	return nil
}

// probePorts reads the wired layout from the device's own board description.
//
// Best effort, and deliberately not fatal. Ports are needed only to tag a VLAN
// onto physical ports; a device that will not report them can still carry
// wireless config perfectly well, and failing adoption over it would refuse a
// device for lacking something most deployments never use. What must not
// happen is inventing port names — the renderer reports the absence instead.
func probePorts(ctx context.Context, c *ubus.Client, r *Registry) {
	var out struct {
		Network map[string]struct {
			Ports  []string `json:"ports"`
			Device string   `json:"device"`
		} `json:"network"`
	}
	if err := c.Call(ctx, "luci-rpc", "getBoardJSON", nil, &out); err != nil {
		r.Notes = append(r.Notes, "wired port layout could not be read "+
			"(luci-rpc.getBoardJSON: "+err.Error()+"); VLANs cannot be tagged "+
			"onto physical ports on this device until it can")
		return
	}
	if lan, ok := out.Network["lan"]; ok {
		r.Ports.LAN = lan.Ports
		// A board with switch ports bridges them; one with a single lan device
		// names it directly. Both are real layouts.
		if len(lan.Ports) > 0 {
			r.Ports.Bridge = "br-lan"
		} else if lan.Device != "" {
			r.Ports.Bridge = lan.Device
		}
	}
	if wan, ok := out.Network["wan"]; ok {
		r.Ports.WAN = wan.Device
	}
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

// splitJudgement is what one radio's two survey samples demonstrated about
// whether rx_time/tx_time can carry an airtime split.
type splitJudgement int

const (
	// splitUndemonstrated: the samples showed nothing either way. The usual
	// cause is an idle channel — busy_time did not move, so counters that do
	// not move prove nothing about counters that would.
	splitUndemonstrated splitJudgement = iota
	// splitUsable: the counters advanced in proportion to busy time.
	splitUsable
	// splitBroken: the counters exist and do not track reality.
	splitBroken
)

// surveyJudgement is what one radio's survey call demonstrated about channel
// utilization.
type surveyJudgement int

const (
	// surveyIdle: the call answered and reported no active time. Almost always
	// a radio that is not running — disabled, or up but never brought online.
	// It says nothing about whether the driver can survey.
	surveyIdle surveyJudgement = iota
	// surveyUsable: counters are there and moving.
	surveyUsable
	// surveyRefused: the ACL blocked the call.
	surveyRefused
	// surveyUnsupported: the call answered with a real failure. That IS a
	// determination — the driver was asked and could not.
	surveyUnsupported
)

// judgeSurvey classifies one radio's survey read.
//
// The same three-way shape as judgeSplit, and for the same reason found the
// same way. `active_time == 0` used to fall through to the caller's Absent
// default, so a device whose radios were all disabled reported that its driver
// cannot do channel utilization. A radio that is switched off has not
// demonstrated anything about its driver, and the claim would flip back the
// moment someone enabled it — which a re-probe would then report as the device
// gaining a feature.
//
// It survived until now because the reference device has both radios up: one
// radio with active time is enough to set the device-wide state, so the
// hardware never exercised the path.
func judgeSurvey(row surveyRow, err error) surveyJudgement {
	switch {
	case err != nil && isDenied(err):
		return surveyRefused
	case err != nil:
		return surveyUnsupported
	case row.ActiveTime > 0:
		return surveyUsable
	default:
		return surveyIdle
	}
}

// judgeSplit classifies a pair of survey samples.
//
// A function rather than an inline switch because the three outcomes are the
// whole point and two of them used to collapse: "could not tell" fell through
// to the caller's Absent default, so an idle channel reported that the driver
// cannot supply the split. Extracted so the distinction can be tested without a
// device that happens to be busy.
func judgeSplit(first, second surveyRow, err2 error) splitJudgement {
	switch {
	case err2 != nil:
		return splitUndemonstrated
	case second.BusyTime <= first.BusyTime:
		return splitUndemonstrated
	case !advancesProportionately(first, second):
		return splitBroken
	default:
		return splitUsable
	}
}

// verdict accumulates one feature's determination across several radios.
//
// It exists because three features on this path had the same bug, written three
// times: a State variable initialised to Absent, and at least one branch that
// could reach the end without demonstrating anything. The default then asserts
// the device lacks a capability that was never actually tested — the collapse
// of NotObservable into Absent that this package exists to prevent, and which
// it is apparently easy to reintroduce one feature at a time.
//
// Encoding it once makes the rule structural rather than remembered: you cannot
// accidentally get an Absent out of this without calling demonstrated(Absent).
type verdict struct {
	present      bool
	absent       bool // some radio demonstrated it is not there
	refused      bool // some radio's check was blocked
	undetermined bool // some radio could not tell either way
}

func (v *verdict) demonstrated(s State) {
	switch s {
	case Present:
		v.present = true
	case Absent:
		v.absent = true
	}
}

func (v *verdict) refuse()    { v.refused = true }
func (v *verdict) undecided() { v.undetermined = true }

// state resolves the accumulated evidence.
//
// Present wins: one radio that demonstrably has the capability settles it for
// the device. A demonstrated absence beats an inconclusive check, because it is
// evidence and the other is not. Anything else that was tried but unresolved is
// NotObservable.
//
// With nothing recorded at all the answer is Absent, which is the device that
// reported no radios: there is genuinely nothing here to survey or control, and
// telling an operator to re-probe a switch would be nonsense.
func (v verdict) state() State {
	switch {
	case v.present:
		return Present
	case v.absent:
		return Absent
	case v.refused, v.undetermined:
		return NotObservable
	default:
		return Absent
	}
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

	// One accumulator per feature. The first three previously defaulted to
	// Absent and all three had a path that reached the end without
	// demonstrating anything — see verdict.
	var survey, split, hostapd, neighbors verdict
	// Tracked apart from the verdict because the note below must only fire when
	// rrm_nr_get_own itself was refused. A radio whose hostapd object was
	// unreachable also leaves neighbours unobservable, and blaming that on the
	// rrm grant would name a call that was never made.
	rrmRefused := false

	for _, dev := range devs.Devices {
		// SurveyUsest starts Unknown, not Absent: the switch below sets it from
		// what the call actually showed, and a default of Absent is a claim
		// about a radio nobody has asked yet.
		radio := Radio{Device: dev}

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
		verdictSurvey := judgeSurvey(first, surveyErr)
		// Whether this radio was actually running, which several checks below
		// need in order to tell "not there" from "not switched on".
		radioLive := verdictSurvey == surveyUsable

		switch verdictSurvey {
		case surveyUsable:
			survey.demonstrated(Present)
			radio.SurveyUsest = Present
		case surveyRefused:
			// Refused is not "this driver has no survey". Record why, and let
			// the aggregate stay NotObservable rather than Absent. The split is
			// read from the same call, so it is equally unreachable.
			survey.refuse()
			split.refuse()
			radio.SurveyUsest = NotObservable
			r.Note("%s: iwinfo.survey denied; channel utilization "+
				"undetermined rather than absent (%v)", dev, surveyErr)
		case surveyUnsupported:
			// Asked and could not answer: a real determination.
			survey.demonstrated(Absent)
			radio.SurveyUsest = Absent
			r.Note("%s: iwinfo.survey failed (%v); channel utilization is not "+
				"available from this driver", dev, surveyErr)
		case surveyIdle:
			survey.undecided()
			radio.SurveyUsest = NotObservable
			r.Note("%s: reported no active time, so nothing was demonstrated "+
				"about channel utilization here. The usual cause is a radio "+
				"that is switched off — which is not the same as a driver "+
				"that cannot report it", dev)
		}
		if surveyErr == nil {
			if first.Noise > 0 {
				r.AddQuirk(Quirk{Source: "iwinfo.survey", Field: "noise",
					Reason: "reported unsigned (161 for -95); iwinfo.info reports " +
						"the same quantity signed — but see noise:stability, " +
						"switching source fixes only the encoding"})
			}
			absurdTimers := first.RxTime > 1<<40 || first.TxTime > 1<<40
			if absurdTimers {
				// A real determination: the counters are visibly uninitialised.
				split.demonstrated(Absent)
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
				switch judgeSplit(first, second, err2) {
				case splitUndemonstrated:
					split.undecided()
				case splitBroken:
					// Present, typed, plausible — and not tracking reality. On
					// mwlwifi rx_time never moves and tx_time crept 2ms while
					// busy_time advanced ~3000ms, which would make the split a
					// rounding error masquerading as a measurement.
					split.demonstrated(Absent)
					r.AddQuirk(Quirk{Source: "iwinfo.survey", Field: "rx_time/tx_time",
						Reason: "do not track busy time (rx+tx advanced <10% of busy); the airtime split is not computable"})
				case splitUsable:
					split.demonstrated(Present)
				}
			}
		}

		// hostapd is the cheap per-AP source; its presence also gates the
		// per-client reconnect/block actions.
		//
		// The `hostapd.<dev>` object only exists while a BSS is running on that
		// radio, so a missing object has two completely different causes:
		// hostapd is not managing this device, or the radio is switched off.
		// The error looks identical. radioLive is what separates them — a radio
		// that reported active time is up, so a missing object then really does
		// mean no hostapd. Without that distinction, a device with its radios
		// disabled reports the per-client controls as unavailable, and enabling
		// a radio makes it look like the device gained a feature.
		// 802.11k neighbour reports ride on the same object, so this radio's
		// hostapd verdict is also the ceiling for them: you cannot learn
		// anything about a method on an object you could not reach.
		hostapdUp := false
		switch err := c.Call(ctx, "hostapd."+dev, "get_status", nil, nil); {
		case err == nil:
			hostapd.demonstrated(Present)
			hostapdUp = true
		case isDenied(err):
			hostapd.refuse()
			neighbors.refuse()
		case radioLive:
			// The radio is up and hostapd is not managing it, so there is no
			// BSS here to hold a neighbour list. A real absence for both.
			hostapd.demonstrated(Absent)
			neighbors.demonstrated(Absent)
		default:
			hostapd.undecided()
			neighbors.undecided()
		}

		// Asked with rrm_nr_get_own because it is the read half of the pair and
		// changes nothing. A device that will hand over its own element will
		// take a list back: both are methods on the same object behind the same
		// ACL entry, and hostapd has carried them together since RRM landed.
		if hostapdUp {
			switch err := c.Call(ctx, "hostapd."+dev, "rrm_nr_get_own", nil, nil); {
			case err == nil:
				neighbors.demonstrated(Present)
			case isDenied(err):
				// The ACL. Measured: stock rpcd grants no rrm_* method, and a
				// grant only reaches devices adopted after it exists — so this
				// is the expected state of every device adopted before this
				// feature, and recording it as Absent would tell an operator
				// their hardware cannot do something it does perfectly well.
				neighbors.refuse()
				rrmRefused = true
			case isMethodMissing(err):
				// hostapd is running and does not carry the method: a build
				// without RRM. A real absence, and not fixable by re-adopting.
				neighbors.demonstrated(Absent)
			default:
				neighbors.undecided()
			}
		}

		r.Radios = append(r.Radios, radio)
	}

	// A radio whose survey could not be read tells us nothing about the split
	// either: they come from the same call.
	if survey.undetermined {
		split.undecided()
	}

	surveyOK := survey.state()
	splitOK := split.state()

	r.Set(FeatSurvey, surveyOK)
	r.Set(FeatHostapdControl, hostapd.state())

	neighborsOK := neighbors.state()
	r.Set(FeatNeighborReport, neighborsOK)
	if neighborsOK == NotObservable && rrmRefused {
		r.Note("802.11k neighbour lists cannot be distributed to this device: " +
			"rpcd refused hostapd's rrm_nr_get_own. The controller's ACL is " +
			"written to a device once, at adoption, so a device adopted before " +
			"this feature existed will keep refusing until it is re-adopted. " +
			"Nothing is wrong with the hardware")
	}

	// These notes are the operator-facing half of the distinction above. Both
	// states gate the same — Buildable accepts only Present — so what changes is
	// what the record CLAIMS, and what someone is told to do about it.
	if surveyOK == NotObservable && !survey.refused {
		r.Note("channel utilization could not be determined: no radio reported " +
			"any active time. Enable a radio and re-probe — a switched-off " +
			"radio demonstrates nothing about what its driver can do")
	}
	// A recorded quirk on these fields settles it for the whole device: one
	// radio reporting a plausible-looking counter cannot license a metric the
	// driver does not really supply.
	if r.HasQuirk("iwinfo.survey", "rx_time/tx_time") {
		splitOK = Absent
	} else if splitOK == NotObservable && !split.refused {
		r.Note("the airtime split could not be determined: the counters " +
			"demonstrated nothing either way, usually because the channel was " +
			"idle for the whole sample. This is not a driver limitation — " +
			"re-probe while there is traffic to settle it")
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

// isMethodMissing reports that the object answered and does not carry the
// method. That is a real determination about the running daemon — a hostapd
// built without RRM, say — and is the one error here that licenses an Absent.
// It is deliberately narrow: METHOD_NOT_FOUND comes from the target, whereas
// an ACL gap never reaches the target at all and surfaces as a denial.
func isMethodMissing(err error) bool {
	var se *ubus.StatusError
	return errors.As(err, &se) && se.Status == ubus.StatusMethodNotFound
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

// FeatMesh gating: can this device run an 802.11s mesh point?
//
// # Why the package list and not the driver
//
// Measured on the reference device 2026-08-14, because the obvious sources do
// not answer it:
//
//	iwinfo.info / luci-rpc.getWirelessDevices  hwmodes are PHY modes (n, ac);
//	                                           no supported-interface-mode list
//	hostapd.<dev> get_features                 {ht_supported, vht_supported} only
//	file.exec /usr/sbin/iw phy <phy> info      ubus status 6 — not in the ACL,
//	                                           and status 6 is permanent
//
// What does answer it is which wpad build is installed, and that grant already
// exists. On OpenWrt the 802.11s mesh daemon is a build of wpad: `wpad-mesh-*`
// carries mesh with SAE, `wpad-basic-*` and `wpad-mini` deliberately do not.
// The reference device reports `wpad-mesh-openssl`.
//
// # Why some builds come back NotObservable
//
// The full wpad variants — `wpad-openssl` and friends — are not named for their
// feature set, and this has not been verified against one. Claiming Present
// from a package name that does not settle it would be exactly the guess this
// package exists to refuse, so those record NotObservable with the package
// named, and an operator can widen or correct it from evidence.
func probeMesh(ctx context.Context, c *ubus.Client, r *Registry) {
	pkgs, err := installedPackages(ctx, c)
	if err != nil {
		r.Set(FeatMesh, NotObservable)
		r.Note("802.11s mesh support undetermined: the installed-package list "+
			"could not be read (%v). It is the only source that answers this — "+
			"iwinfo reports PHY modes rather than interface modes, and "+
			"`iw phy` is not in the ACL", err)
		return
	}

	state := meshFromPackages(pkgs)
	r.Note("802.11s mesh %s from the installed 802.11 daemon (%s)",
		state, describeWpad(pkgs))

	// The daemon having mesh does not mean the RADIO can run one.
	//
	// Measured on the reference device 2026-08-15, and it is the reason this
	// check exists at all. mwlwifi (Marvell 88W8964) advertises "mesh point" in
	// its supported interface modes AND permits it in its interface
	// combinations — `#{ AP } <= 16, #{ mesh point } <= 1` — so every source a
	// controller can consult says yes. Configure one and netifd creates the
	// interface, `iw dev` reports `type mesh point`, and then:
	//
	//	wpa_supplicant: Could not set interface phy0-mesh0 flags (UP):
	//	                Operation not permitted
	//	wpa_supplicant: phy0-mesh0: Failed to initialize driver interface
	//
	// The interface sits at state DOWN. With the AP on the same phy disabled it
	// still sits at DOWN, so it is not a combination limit — the driver simply
	// will not bring a mesh point up.
	//
	// Nothing refuses the config: uci accepts it, the apply's health check
	// passes (it asks whether the SSIDs are on air, and they are), and the
	// confirm lands. A controller trusting the package list alone would report
	// a healthy applied mesh that does not exist.
	//
	// This is the exact category Quirk was made for — present, correctly typed,
	// plausible, and wrong — and the same driver already supplies three others.
	if hw := marvellRadio(r); hw != "" {
		r.AddQuirk(Quirk{
			Source: "mac80211", Field: "mesh-point",
			Reason: "advertised as a supported interface mode and permitted by " +
				"the interface combinations, but the driver refuses to bring a " +
				"mesh interface UP (\"Operation not permitted\"); measured on " +
				hw + ". uci accepts the config and the apply reports success",
		})
		state = Absent
		r.Note("802.11s mesh is gated OFF despite the daemon supporting it: %s "+
			"creates the mesh interface and cannot bring it up. Configuring one "+
			"would apply cleanly and never work", hw)
	}
	r.Set(FeatMesh, state)
}

// marvellRadio reports a radio whose driver is known not to run a mesh point,
// or "" when none is.
//
// Keyed on the hardware name because the driver name lives in /sys, which rpcd
// canonicalises out of reach (see the ACL's note). iwinfo reports
// hardware.name, and on this board that is "Marvell 88W8964".
//
// Only what has been measured. A different Marvell part may well behave
// differently, and the honest failure here is a device wrongly told it cannot
// mesh — recoverable, and visible, because the note says exactly which chip the
// judgement came from.
func marvellRadio(r *Registry) string {
	for _, radio := range r.Radios {
		if strings.Contains(strings.ToLower(radio.Hardware), "marvell") {
			return radio.Hardware
		}
	}
	return ""
}

// meshFromPackages decides FeatMesh from an installed-package list.
//
// Split out from the call so the rule is testable without a device, and because
// the rule is the whole content of the check.
func meshFromPackages(pkgs []string) State {
	var wpad []string
	for _, p := range pkgs {
		if strings.HasPrefix(p, "wpad") || strings.HasPrefix(p, "hostapd") {
			wpad = append(wpad, p)
		}
	}
	for _, p := range wpad {
		if strings.HasPrefix(p, "wpad-mesh") {
			return Present
		}
	}
	for _, p := range wpad {
		// Builds named for lacking it.
		if strings.HasPrefix(p, "wpad-basic") || strings.HasPrefix(p, "wpad-mini") {
			return Absent
		}
	}
	if len(wpad) == 0 {
		// No 802.11 daemon at all: this device cannot run an AP either.
		return Absent
	}
	// A full build — wpad-openssl and friends — is not named for its feature
	// set, and this has not been verified against one. Claiming Present from a
	// package name that does not settle it is the guess this package refuses.
	return NotObservable
}

func describeWpad(pkgs []string) string {
	var wpad []string
	for _, p := range pkgs {
		if strings.HasPrefix(p, "wpad") || strings.HasPrefix(p, "hostapd") {
			wpad = append(wpad, p)
		}
	}
	if len(wpad) == 0 {
		return "none installed"
	}
	return strings.Join(wpad, ", ")
}

// installedPackages reads the package list from whichever manager this build
// uses.
//
// Both are tried because OpenWrt is mid-migration: the reference device answers
// apk and returns "not found" for opkg. Trying one and reporting its absence as
// a failure would make the whole check unavailable on half the fleet.
func installedPackages(ctx context.Context, c *ubus.Client) ([]string, error) {
	type run struct {
		cmd    string
		params []string
	}
	var lastErr error
	for _, r := range []run{
		{"/usr/bin/apk", []string{"list", "--installed"}},
		{"/bin/opkg", []string{"list-installed"}},
	} {
		var out struct {
			Code   int    `json:"code"`
			Stdout string `json:"stdout"`
		}
		err := c.Call(ctx, "file", "exec",
			map[string]any{"command": r.cmd, "params": r.params}, &out)
		if err != nil {
			lastErr = err
			continue
		}
		if out.Code != 0 || strings.TrimSpace(out.Stdout) == "" {
			lastErr = fmt.Errorf("%s exited %d", r.cmd, out.Code)
			continue
		}
		return packageNames(out.Stdout), nil
	}
	return nil, lastErr
}

// packageNames pulls package names out of either manager's output.
//
//	apk:  wpad-mesh-openssl-2025.08.26~ca266cc2-r2 arm_cortex-a9 {feeds/...} ...
//	opkg: wpad-mesh-openssl - 2025.08.26-r2
//
// Both put the name first, and apk glues the version on with a hyphen — so the
// name is taken up to the first hyphen-digit boundary rather than the first
// hyphen, which would truncate "wpad-mesh-openssl" to "wpad".
func packageNames(stdout string) []string {
	var out []string
	for _, line := range strings.Split(stdout, "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) == 0 {
			continue
		}
		out = append(out, trimVersion(f[0]))
	}
	return out
}

func trimVersion(s string) string {
	for i := 1; i < len(s); i++ {
		if s[i-1] == '-' && s[i] >= '0' && s[i] <= '9' {
			return s[:i-1]
		}
	}
	return s
}

// probeUplink decides whether this device could join a network over the air.
//
// # What a package list can and cannot settle
//
// A 4-address (WDS) uplink needs two halves, and only one of them is a package
// question. The station side runs `wpa_supplicant`, which ships inside every
// `wpad*` build and inside no bare `hostapd*` build — so a device carrying only
// hostapd can serve an AP and can never join one, and that IS answerable here.
//
// The other half is whether the radio will actually carry a 4addr station, and
// nothing installable answers that. `iw phy info` would, and it is not in the
// ACL and should not be: §5m measured the alternatives and none of them
// reports interface modes.
//
// So Present here means "the software is there", not "this will work" — stated
// in the note, because §5q is precisely what happens when a capability inferred
// from a daemon is read as a promise about a driver. The renderer treats it the
// same way it treats mesh: it gates on Present and says which of the two
// possible Absents it is looking at.
func probeUplink(ctx context.Context, c *ubus.Client, r *Registry) {
	pkgs, err := installedPackages(ctx, c)
	if err != nil {
		r.Set(FeatWirelessUplink, NotObservable)
		r.Note("wireless uplink undetermined: the installed-package list could "+
			"not be read (%v), and it is the only source that says whether a "+
			"supplicant is present", err)
		return
	}
	state := uplinkFromPackages(pkgs)

	// Measured 2026-08-16, and it is why the note below is worded as it is.
	//
	// On an Archer C6 v2 (ath10k, OpenWrt 25.12.5) a station interface can be
	// created and cannot associate. Everything a controller can consult says it
	// should work: wpad-mesh-openssl is installed, wpa_supplicant is running and
	// opens a control socket for the interface, and `iw phy` declares
	// `#{ managed } <= 16` alongside the APs on one channel. The interface comes
	// up in Client mode at channel 0 with 0 dBm, `iw link` says "Not connected",
	// and a scan returns zero BSSes.
	//
	// Isolated properly rather than guessed at: it fails identically with a
	// hand-written UCI section and no controller involved, WITHOUT `wds` as well
	// as with it, and with every AP on that radio disabled so the station is
	// alone. So it is not the controller, not 4-address framing, and not a
	// concurrency limit — station mode does not work on that radio.
	//
	// NOT recorded as a Quirk, deliberately. A quirk here would gate the feature
	// off, and one board is not a driver: this could be that board, that
	// firmware build, or ath10k generally, and the three send an operator to
	// three different places. What it does instead is refuse to let Present be
	// read as a promise — see the note.
	r.Set(FeatWirelessUplink, state)
	if state == Present {
		r.Note("a supplicant is installed (%s), so the SOFTWARE for joining a "+
			"network over the air is present. Whether this radio will actually "+
			"carry a station is NOT settled by that, and no source the ACL can "+
			"reach answers it. Measured on an ath10k board where every "+
			"available signal said yes — supplicant running, `iw phy` declaring "+
			"the combination valid — and the station never associated on any "+
			"channel. Treat Present here as 'worth trying', and expect a station "+
			"that comes up and never associates to be the way it fails",
			describeWpad(pkgs))
	}
}

// uplinkFromPackages is the rule, split out so it is testable without a device.
//
// Deliberately more permissive than meshFromPackages. Mesh needs a build named
// for carrying 802.11s, and a build that is not named for its feature set
// therefore settles nothing. A supplicant is different: it is in every wpad
// build including wpad-basic and wpad-mini, which are named for lacking MESH
// and not for lacking a supplicant. So `wpad` in any form is Present, and the
// only Absent is a device carrying hostapd alone or no daemon at all.
func uplinkFromPackages(pkgs []string) State {
	var wpad, hostapdOnly bool
	for _, p := range pkgs {
		switch {
		case strings.HasPrefix(p, "wpad"):
			wpad = true
		case strings.HasPrefix(p, "hostapd"):
			hostapdOnly = true
		}
	}
	if wpad {
		return Present
	}
	if hostapdOnly {
		// An AP daemon and no supplicant: this device can serve a network and
		// cannot join one. A real absence, and fixable by installing wpad.
		return Absent
	}
	// No 802.11 daemon at all. It cannot do wireless anything.
	return Absent
}
