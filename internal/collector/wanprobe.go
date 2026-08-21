package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

const (
	wanProbeCommand     = "/bin/ping"
	wanProbeTarget      = "1.1.1.1"
	wanProbePackets     = 3
	wanProbeWaitSecs    = 1
	wanProbeInterval    = time.Minute
	maxWANProbeOutput   = 16 << 10
	maxWANProbeEnvelope = 128 << 10
)

var (
	pingSummaryRE = regexp.MustCompile(`^(\d+) packets transmitted, (\d+)(?: packets)? received(?:, (\d+) duplicates)?, (\d+)% packet loss(?:, time \d+ms)?$`)
	pingRTTRE     = regexp.MustCompile(`^(?:round-trip min/avg/max|rtt min/avg/max/mdev) = ([0-9]+(?:\.[0-9]+)?)/([0-9]+(?:\.[0-9]+)?)/([0-9]+(?:\.[0-9]+)?)(?:/[0-9]+(?:\.[0-9]+)?)? ms$`)
)

// WANProbe is one completed gateway-vantage reachability check. The pointer on
// Snapshot distinguishes a measured 100% loss from an unavailable probe.
// Latency is absent when no packet returned; zero is never invented.
type WANProbe struct {
	Up        bool
	LossPct   float64
	LatencyMS *float64
}

// takeWANProbe is both the gateway/cadence gate and the attempt stamp. Stamping
// when the call is built prevents a denied optional call from being retried on
// every focused poll; the next minute still notices a refreshed ACL.
func (p *poller) takeWANProbe() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.target.Gateway {
		return false
	}
	now := p.c.now()
	elapsed := now.Sub(p.wanProbeAt)
	if !p.wanProbeAt.IsZero() && elapsed >= 0 && elapsed < p.wanIntervalLocked() {
		return false
	}
	p.wanProbeAt = now
	return true
}

func (p *poller) wanIntervalLocked() time.Duration {
	if p.wanInterval > 0 {
		return p.wanInterval
	}
	return wanProbeInterval
}

func wanProbeCall() call {
	return call{
		inv: ubus.Invocation{Object: "file", Method: "exec", Args: map[string]any{
			"command": wanProbeCommand,
			"params": []string{
				"-q", "-c", strconv.Itoa(wanProbePackets),
				"-W", strconv.Itoa(wanProbeWaitSecs), wanProbeTarget,
			},
		}},
		decode: decodeWANProbe, optional: true,
	}
}

func (p *poller) pollWAN(ctx context.Context, client *ubus.Client, target Target) Snapshot {
	snap := Snapshot{DeviceID: target.DeviceID, MAC: target.MAC, Name: target.Name,
		Tier: Baseline, At: p.c.now(), WANOnly: true}
	spec := wanProbeCall()
	start := p.c.now()
	results, err := client.Batch(ctx, []ubus.Invocation{spec.inv})
	snap.Duration = p.c.now().Sub(start)
	if err != nil {
		snap.Err = err
		return snap
	}
	if len(results) != 1 {
		snap.Err = fmt.Errorf("collector: WAN probe returned %d results for 1 call", len(results))
		return snap
	}
	result := results[0]
	if result.Err != nil {
		snap.Degraded = append(snap.Degraded, Degradation{
			Object: spec.inv.Object, Method: spec.inv.Method, Status: result.Status,
			Cause: degradationCause(result.Err, result.Status), Err: result.Err.Error(),
			Permanent: ubus.IsPermanent(result.Err),
		})
		return snap
	}
	if err := spec.decode(result.Data, &snap); err != nil {
		snap.Degraded = append(snap.Degraded, Degradation{
			Object: spec.inv.Object, Method: spec.inv.Method,
			Cause: CauseDecode, Err: fmt.Sprintf("decode: %v", err),
		})
	}
	return snap
}

// decodeWANProbe accepts the two stock BusyBox ping outcomes: code 0 with at
// least one reply, or code 1 with a complete zero-reply summary. ACL refusal,
// missing binaries, timeouts and malformed/incomplete output are decoder
// failures, which leaves the probe unknown rather than recording zeroes.
func decodeWANProbe(raw json.RawMessage, snap *Snapshot) error {
	snap.WAN = nil
	probe, err := parseWANProbe(raw)
	if err != nil {
		return err
	}
	snap.WAN = &probe
	return nil
}

func parseWANProbe(raw json.RawMessage) (WANProbe, error) {
	envelope, err := decodeWANExec(raw)
	if err != nil {
		return WANProbe{}, err
	}
	if envelope.Code == nil {
		return WANProbe{}, fmt.Errorf("wan probe: file.exec response has no exit code")
	}
	if len(envelope.Stdout) > maxWANProbeOutput || len(envelope.Stderr) > maxWANProbeOutput {
		return WANProbe{}, fmt.Errorf("wan probe: output exceeds %d bytes", maxWANProbeOutput)
	}
	if *envelope.Code != 0 && *envelope.Code != 1 {
		return WANProbe{}, fmt.Errorf("wan probe: ping exited %d", *envelope.Code)
	}

	var summary, timing []string
	for _, rawLine := range strings.Split(envelope.Stdout, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if match := pingSummaryRE.FindStringSubmatch(line); match != nil {
			if summary != nil {
				return WANProbe{}, fmt.Errorf("wan probe: duplicate packet summary")
			}
			summary = match
		}
		if match := pingRTTRE.FindStringSubmatch(line); match != nil {
			if timing != nil {
				return WANProbe{}, fmt.Errorf("wan probe: duplicate timing summary")
			}
			timing = match
		}
	}
	if summary == nil {
		return WANProbe{}, fmt.Errorf("wan probe: ping returned no packet summary")
	}
	transmitted, err := strconv.Atoi(summary[1])
	if err != nil || transmitted != wanProbePackets {
		return WANProbe{}, fmt.Errorf("wan probe: transmitted %q packets, want %d", summary[1], wanProbePackets)
	}
	received, err := strconv.Atoi(summary[2])
	if err != nil || received < 0 || received > transmitted {
		return WANProbe{}, fmt.Errorf("wan probe: invalid received packet count %q", summary[2])
	}
	if summary[3] != "" {
		if _, err := strconv.ParseUint(summary[3], 10, 64); err != nil {
			return WANProbe{}, fmt.Errorf("wan probe: invalid duplicate packet count")
		}
	}
	reportedLoss, err := strconv.Atoi(summary[4])
	// BusyBox 1.37 computes this with unsigned integer division, so one and two
	// losses out of three are printed as 33% and 66%, not rounded to 67%.
	wantReportedLoss := (transmitted - received) * 100 / transmitted
	if err != nil || reportedLoss != wantReportedLoss {
		return WANProbe{}, fmt.Errorf("wan probe: packet summary loss does not match its counts")
	}
	if (*envelope.Code == 0) != (received > 0) {
		return WANProbe{}, fmt.Errorf("wan probe: exit code and received packet count disagree")
	}

	probe := WANProbe{
		Up:      received > 0,
		LossPct: float64(transmitted-received) * 100 / float64(transmitted),
	}
	if received == 0 {
		if timing != nil {
			return WANProbe{}, fmt.Errorf("wan probe: timing reported with no received packets")
		}
		return probe, nil
	}
	if timing == nil {
		return WANProbe{}, fmt.Errorf("wan probe: replies returned without a timing summary")
	}
	minimum, errMin := strconv.ParseFloat(timing[1], 64)
	average, errAvg := strconv.ParseFloat(timing[2], 64)
	maximum, errMax := strconv.ParseFloat(timing[3], 64)
	if errMin != nil || errAvg != nil || errMax != nil ||
		math.IsNaN(minimum) || math.IsNaN(average) || math.IsNaN(maximum) ||
		math.IsInf(minimum, 0) || math.IsInf(average, 0) || math.IsInf(maximum, 0) ||
		minimum < 0 || minimum > average || average > maximum || maximum > 60_000 {
		return WANProbe{}, fmt.Errorf("wan probe: invalid timing summary")
	}
	probe.LatencyMS = &average
	return probe, nil
}

type wanExecEnvelope struct {
	Code   *int
	Stdout string
	Stderr string
}

// decodeWANExec rejects duplicate and unknown envelope members. Go's ordinary
// struct decoder accepts duplicates last-one-wins, which would make the same
// device response mean two things depending on ordering.
func decodeWANExec(raw []byte) (wanExecEnvelope, error) {
	if len(raw) > maxWANProbeEnvelope || !utf8.Valid(raw) {
		return wanExecEnvelope{}, fmt.Errorf("wan probe: file.exec envelope is invalid or exceeds %d bytes", maxWANProbeEnvelope)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	opening, err := dec.Token()
	if err != nil {
		return wanExecEnvelope{}, fmt.Errorf("wan probe: file.exec: %w", err)
	}
	if opening != json.Delim('{') {
		return wanExecEnvelope{}, fmt.Errorf("wan probe: file.exec expected a JSON object")
	}
	var envelope wanExecEnvelope
	seen := map[string]bool{}
	for dec.More() {
		member, err := dec.Token()
		if err != nil {
			return wanExecEnvelope{}, fmt.Errorf("wan probe: file.exec: %w", err)
		}
		name, ok := member.(string)
		if !ok || seen[name] {
			return wanExecEnvelope{}, fmt.Errorf("wan probe: file.exec has a duplicate or invalid member")
		}
		seen[name] = true
		switch name {
		case "code":
			err = dec.Decode(&envelope.Code)
		case "stdout":
			err = dec.Decode(&envelope.Stdout)
		case "stderr":
			err = dec.Decode(&envelope.Stderr)
		default:
			return wanExecEnvelope{}, fmt.Errorf("wan probe: file.exec has unknown member %q", name)
		}
		if err != nil {
			return wanExecEnvelope{}, fmt.Errorf("wan probe: file.exec member %q: %w", name, err)
		}
	}
	if closing, err := dec.Token(); err != nil || closing != json.Delim('}') {
		return wanExecEnvelope{}, fmt.Errorf("wan probe: file.exec object is incomplete")
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return wanExecEnvelope{}, fmt.Errorf("wan probe: file.exec has trailing data")
	}
	return envelope, nil
}
