package collector

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

func TestGatewayPollCapturesMockWANProbe(t *testing.T) {
	p := &poller{
		c:      New(newRecorder(), Options{Log: quiet()}),
		target: Target{DeviceID: 1, Gateway: true},
	}
	client, err := mockConnect(t)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snap := p.poll(context.Background(), client, p.target, Baseline, nil, nil)
	if snap.Err != nil {
		t.Fatal(snap.Err)
	}
	if snap.WAN == nil || !snap.WAN.Up || snap.WAN.LossPct != 0 ||
		snap.WAN.LatencyMS == nil || *snap.WAN.LatencyMS != 12 {
		t.Fatalf("WAN probe = %+v", snap.WAN)
	}
}

func TestWANProbeUsesExactGatewayOnlyLowCadenceCall(t *testing.T) {
	now := time.Unix(1_000, 0)
	p := &poller{
		c:      New(newRecorder(), Options{Now: func() time.Time { return now }}),
		target: Target{DeviceID: 1, Gateway: true},
	}
	wantParams := []string{"-q", "-c", "3", "-W", "1", "1.1.1.1"}
	assert := func(calls []call, want int) {
		t.Helper()
		got := 0
		for _, spec := range calls {
			if spec.inv.Object != "file" || spec.inv.Method != "exec" {
				continue
			}
			args, ok := spec.inv.Args.(map[string]any)
			if !ok || args["command"] != wanProbeCommand {
				continue
			}
			got++
			if !spec.optional || spec.adaptiveWait != 2*time.Second ||
				!reflect.DeepEqual(args["params"], wantParams) {
				t.Fatalf("WAN call optional=%v adaptive wait=%v args=%#v",
					spec.optional, spec.adaptiveWait, spec.inv.Args)
			}
		}
		if got != want {
			t.Fatalf("WAN calls=%d, want %d", got, want)
		}
	}

	assert(p.buildCalls(Baseline, nil, nil), 1)
	assert(p.buildCalls(Focused, nil, nil), 0)
	now = now.Add(wanProbeInterval - time.Millisecond)
	assert(p.buildCalls(Focused, nil, nil), 0)
	now = now.Add(time.Millisecond)
	assert(p.buildCalls(Focused, nil, nil), 1)
	// A controller clock correction must rebase the attempt timer rather than
	// suppress probes until wall time catches up.
	now = now.Add(-2 * wanProbeInterval)
	assert(p.buildCalls(Focused, nil, nil), 1)

	// Losing the Gateway function stops the site probe even after its cadence
	// expires. Interface naming and AP capability are deliberately irrelevant.
	p.mu.Lock()
	p.target.Gateway = false
	p.mu.Unlock()
	now = now.Add(wanProbeInterval)
	assert(p.buildCalls(Baseline, []string{"phy0-ap0"}, map[string]string{"phy0-ap0": "ap"}), 0)
}

func TestGatewayProbePacingIsExcludedFromAdaptiveWidening(t *testing.T) {
	c := New(newRecorder(), Options{
		Baseline: time.Second, MaxInterval: time.Hour,
		SlowPoll: DefaultSlowPoll, LoadLimit: DefaultLoadLimit, Log: quiet(),
	})
	p := newPoller(c, Target{DeviceID: 1, MAC: "aa", Gateway: true})
	probeWait := wanProbeCall().adaptiveWait

	snapshot := func(total, completedWait time.Duration) Snapshot {
		t.Helper()
		snap := Snapshot{Duration: total}
		snap.setBusyDuration(completedWait)
		return snap
	}

	clamped := snapshot(time.Second, probeWait)
	if !clamped.busyDurationKnown || clamped.busyDuration != 0 {
		t.Fatalf("busy duration below fixed pacing = %v known=%v, want 0 true",
			clamped.busyDuration, clamped.busyDurationKnown)
	}

	ordinary := snapshot(2*time.Second+200*time.Millisecond, probeWait)
	p.succeed(ordinary)
	if ordinary.Duration != 2200*time.Millisecond {
		t.Fatalf("diagnostic duration changed to %v", ordinary.Duration)
	}
	if ordinary.busyDuration != 200*time.Millisecond || p.widen != 0 {
		t.Fatalf("ping plus normal core work: busy=%v widen=%d, want 200ms and 0",
			ordinary.busyDuration, p.widen)
	}

	slow := snapshot(2*time.Second+1600*time.Millisecond, probeWait)
	p.succeed(slow)
	if slow.busyDuration != 1600*time.Millisecond || p.widen != 1 {
		t.Fatalf("ping plus unexplained overhead: busy=%v widen=%d, want 1.6s and 1",
			slow.busyDuration, p.widen)
	}

	// An RPC response is not enough: only a valid decoded ping result proves
	// that the request spent the expected two seconds pacing packets.
	p.widen = 0
	invalid := snapshot(2*time.Second+200*time.Millisecond, 0)
	if err := wanProbeCall().decode([]byte(`{"code":2,"stdout":"","stderr":"bad option"}`), &invalid); err == nil {
		t.Fatal("invalid ping unexpectedly decoded")
	}
	p.succeed(invalid)
	if invalid.busyDuration != 2200*time.Millisecond || p.widen != 1 {
		t.Fatalf("invalid ping: busy=%v widen=%d, want full 2.2s and 1",
			invalid.busyDuration, p.widen)
	}
}

func TestAPOnlyTargetNeverSchedulesWANProbe(t *testing.T) {
	p := &poller{
		c:      New(newRecorder(), Options{Now: func() time.Time { return time.Unix(2_000, 0) }}),
		target: Target{DeviceID: 2, Name: "AP", Gateway: false},
	}
	for _, spec := range p.buildCalls(Baseline, nil, nil) {
		if spec.inv.Object == "file" && spec.inv.Method == "exec" {
			args, _ := spec.inv.Args.(map[string]any)
			if args["command"] == wanProbeCommand {
				t.Fatalf("AP-only target scheduled WAN probe: %s", fmt.Sprint(spec.inv.Args))
			}
		}
	}
}

func TestGatewayWANDeadlineDoesNotShortenFifteenMinuteBaseline(t *testing.T) {
	now := time.Unix(2_500, 0)
	p := newPoller(New(newRecorder(), Options{
		Baseline: time.Minute, Now: func() time.Time { return now }, Log: quiet(),
	}), Target{DeviceID: 1, Gateway: true, Baseline: 15 * time.Minute})
	p.wanProbeAt = now
	if got := p.next(); got != 15*time.Minute {
		t.Fatalf("full baseline delay = %v, want 15m", got)
	}
	if got, enabled := p.nextWANDelay(); !enabled || got != time.Minute {
		t.Fatalf("WAN delay = %v enabled=%v, want 1m independently", got, enabled)
	}
}

func TestLongBaselineRunsMinuteWANOnlyPollsWithoutRepeatingFullWork(t *testing.T) {
	// 40 ms models one minute; 600 ms preserves the production 15:1 ratio.
	rec := newRecorder()
	c := New(rec, Options{Baseline: 40 * time.Millisecond, Focused: 10 * time.Millisecond,
		MaxInterval: time.Second, Log: quiet()})
	c.Add(Target{DeviceID: 1, MAC: "aa:bb:cc:dd:ee:ff", Name: "gateway",
		Gateway: true, Baseline: 600 * time.Millisecond, Connect: mockConnect(t)})
	p := c.pollers[1]
	p.wanInterval = 40 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx)
	t.Cleanup(func() { cancel(); c.Stop() })

	full, wanOnly := 0, 0
	deadline := time.After(500 * time.Millisecond)
	for wanOnly < 3 {
		select {
		case snap := <-rec.ch:
			if snap.WANOnly {
				wanOnly++
				if snap.Err != nil || snap.WAN == nil {
					t.Fatalf("WAN-only snapshot = %+v", snap)
				}
			} else {
				full++
			}
		case <-deadline:
			t.Fatalf("full=%d WAN-only=%d; want one full and three minute probes", full, wanOnly)
		}
	}
	if full != 1 {
		t.Fatalf("full baseline polls=%d during three modeled minutes, want 1", full)
	}
}

func TestLongBaselineRunsMinuteLogPollsWithoutRepeatingFullWork(t *testing.T) {
	// 40 ms models one minute; 600 ms preserves the production 15:1 ratio.
	rec := newRecorder()
	c := New(rec, Options{Baseline: 40 * time.Millisecond, Focused: 10 * time.Millisecond,
		MaxInterval: time.Second, Log: quiet()})
	c.Add(Target{DeviceID: 1, MAC: "aa:bb:cc:dd:ee:ff", Name: "access-point",
		Baseline: 600 * time.Millisecond, Connect: mockConnect(t)})
	p := c.pollers[1]
	p.logInterval = 40 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx)
	t.Cleanup(func() { cancel(); c.Stop() })

	full, logs := 0, 0
	deadline := time.After(500 * time.Millisecond)
	for logs < 3 {
		select {
		case snap := <-rec.ch:
			if snap.LogOnly {
				logs++
				if snap.WANOnly {
					t.Fatal("AP-only auxiliary log poll also claimed WAN data")
				}
			} else {
				full++
			}
		case <-deadline:
			t.Fatalf("full=%d log-only=%d; want one full and three minute log attempts", full, logs)
		}
	}
	if full != 1 {
		t.Fatalf("full baseline polls=%d during three modeled minutes, want 1", full)
	}
}

func TestDefaultMinuteBaselineAbsorbsWANProbeWithoutExtraRequest(t *testing.T) {
	rec := newRecorder()
	c := New(rec, Options{Baseline: 40 * time.Millisecond, Focused: 10 * time.Millisecond,
		MaxInterval: time.Second, Log: quiet()})
	c.Add(Target{DeviceID: 1, MAC: "aa:bb:cc:dd:ee:ff", Name: "gateway",
		Gateway: true, Connect: mockConnect(t)})
	c.pollers[1].wanInterval = 40 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx)
	t.Cleanup(func() { cancel(); c.Stop() })

	full := 0
	deadline := time.After(500 * time.Millisecond)
	for full < 3 {
		select {
		case snap := <-rec.ch:
			if snap.WANOnly {
				t.Fatal("default baseline scheduled a separate WAN request")
			}
			full++
		case <-deadline:
			t.Fatalf("full polls=%d, want 3", full)
		}
	}
}

func TestGatewayFunctionChangeStartsWANDeadlineWithoutForcingFullPoll(t *testing.T) {
	rec := newRecorder()
	c := New(rec, Options{Baseline: 40 * time.Millisecond, Focused: 10 * time.Millisecond,
		MaxInterval: time.Second, Log: quiet()})
	target := Target{DeviceID: 1, MAC: "aa:bb:cc:dd:ee:ff", Name: "router",
		Baseline: 600 * time.Millisecond, Connect: mockConnect(t)}
	c.Add(target)
	c.pollers[1].wanInterval = 40 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx)
	t.Cleanup(func() { cancel(); c.Stop() })

	first := rec.next(t, time.Second)
	if first.WANOnly {
		t.Fatal("non-gateway emitted a WAN-only snapshot")
	}
	target.Gateway = true
	c.Add(target)

	deadline := time.After(250 * time.Millisecond)
	for {
		select {
		case snap := <-rec.ch:
			if !snap.WANOnly {
				t.Fatal("gateway function change forced expensive full work")
			}
			if snap.WAN == nil || snap.Err != nil {
				t.Fatalf("first scheduled gateway probe = %+v", snap)
			}
			return
		case <-deadline:
			t.Fatal("gateway function change did not schedule WAN probe by one modeled minute")
		}
	}
}

func TestWANProbeACLDenialStaysUnknownAndOnlyDegradesPoll(t *testing.T) {
	ctx := context.Background()
	admin := ubus.New(ubus.Options{Host: mockAddr})
	if err := admin.Login(ctx, "root", "good"); err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err := admin.Call(ctx, "__test", "set_acl_gap", map[string]any{
		"pairs": []map[string]string{{"object": "file", "method": "exec"}},
	}, nil); err != nil {
		t.Skipf("mock does not support ACL-gap simulation: %v", err)
	}
	defer admin.Call(ctx, "__test", "set_acl_gap", map[string]any{"pairs": []any{}}, nil)

	now := time.Unix(3_000, 0)
	p := &poller{
		c:          New(newRecorder(), Options{Now: func() time.Time { return now }, Log: quiet()}),
		target:     Target{DeviceID: 1, Gateway: true},
		topologyAt: now, // keep ping as this poll's only file.exec call
		netAt:      now,
		meshAt:     now,
	}
	client, err := mockConnect(t)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snap := p.poll(ctx, client, p.target, Baseline, nil, nil)
	if snap.Err != nil || snap.WAN != nil {
		t.Fatalf("ACL-denied probe: err=%v WAN=%+v", snap.Err, snap.WAN)
	}
	var found int
	for _, degradation := range snap.Degraded {
		if degradation.Object == "file" && degradation.Method == "exec" {
			found++
			if degradation.Cause != CausePermission || !degradation.Permanent {
				t.Errorf("WAN ACL degradation = %+v", degradation)
			}
		}
	}
	if found != 1 {
		t.Fatalf("file.exec degradations=%d, want one WAN gap: %+v", found, snap.Degraded)
	}
}
