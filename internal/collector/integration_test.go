//go:build integration

package collector

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

// Integration tests against a real OpenWrt device, under the SCOPED controller
// credential rather than root — which is the only way to learn that the poll
// asks for something the ACL does not grant.
//
//	OONFEE_TEST_HOST=192.168.1.1 OONFEE_TEST_USER=oonfeewrt \
//	OONFEE_TEST_PASS=... go test -tags=integration ./internal/collector/ -v
//
// Read-only: nothing here writes UCI or touches a service.

func realConnect(t *testing.T) Connect {
	t.Helper()
	host := os.Getenv("OONFEE_TEST_HOST")
	user := os.Getenv("OONFEE_TEST_USER")
	pass := os.Getenv("OONFEE_TEST_PASS")
	if host == "" || user == "" {
		t.Skip("set OONFEE_TEST_HOST and OONFEE_TEST_USER to run integration tests")
	}
	return func(ctx context.Context) (*ubus.Client, error) {
		c := ubus.New(ubus.Options{Host: host, Timeout: 20 * time.Second})
		if err := c.Login(ctx, user, pass); err != nil {
			return nil, err
		}
		t.Cleanup(c.Close)
		return c, nil
	}
}

// The point of this test is the ACL, not the numbers. Every degradation here is
// a grant the deployed ACL is missing, and a degradation is easy to overlook
// precisely because the poll still "works".
func TestIntegrationPollUnderTheScopedCredential(t *testing.T) {
	ctx := context.Background()
	rec := newRecorder()
	c := New(rec, Options{Log: quiet()})
	c.Add(Target{DeviceID: 1, MAC: "integration", Name: "device", Connect: realConnect(t)})

	p := c.pollers[1]
	client, err := p.dial(ctx, p.target)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	ifaces, err := p.discoverIfaces(ctx, client)
	if err != nil {
		t.Fatalf("iwinfo.devices under the scoped credential: %v", err)
	}
	if len(ifaces) == 0 {
		t.Fatal("no wireless interfaces found")
	}
	t.Logf("interfaces: %v", ifaces)

	// What each interface is FOR. The poll uses this to avoid asking a mesh
	// point for its "clients" — a mesh point's peers are other access points,
	// and counting them would report the backhaul as connected users.
	modes, err := p.discoverIfaceModes(ctx, client)
	if err != nil {
		t.Fatalf("luci-rpc.getWirelessDevices under the scoped credential: %v", err)
	}
	t.Logf("interface modes: %v", modes)
	for _, iface := range ifaces {
		if _, ok := modes[iface]; !ok {
			t.Errorf("%s has no mode; it will be polled for clients on the "+
				"assumption that it is an AP", iface)
		}
	}

	for _, tier := range []Tier{Baseline, Focused} {
		p.boardAt = time.Time{} // force the board read on both, to exercise it
		snap := p.poll(ctx, client, tier, ifaces, modes)
		if !snap.OK() {
			t.Fatalf("%s poll failed: %v", tier, snap.Err)
		}
		for _, d := range snap.Degraded {
			t.Errorf("%s poll: %s (the deployed ACL is missing this grant)", tier, d)
		}
		t.Logf("%s: %v, %d calls' worth — uptime=%ds load1=%.2f mem_used=%dMiB "+
			"ifaces=%d aps=%d stations=%d surveys=%d",
			tier, snap.Duration.Round(time.Millisecond), len(p.buildCalls(tier, ifaces, modes)),
			snap.Uptime, snap.Load[0], snap.Memory.Used()/(1<<20),
			len(snap.Interfaces), len(snap.APs), len(snap.Stations), len(snap.Surveys))

		if snap.Board == nil {
			t.Error("board was not read")
		}
		if snap.Uptime == 0 {
			t.Error("uptime is zero")
		}
		if len(snap.APs) != len(ifaces) {
			t.Errorf("got %d APs for %d interfaces", len(snap.APs), len(ifaces))
		}
		for _, ap := range snap.APs {
			if ap.Clients == nil {
				t.Errorf("%s: client count unknown", ap.Iface)
			}
			if ap.Airtime != nil {
				t.Logf("  %s ssid=%q ch=%d clients=%d airtime=%.1f%%",
					ap.Iface, ap.SSID, ap.Channel, *ap.Clients,
					ap.Airtime.UtilizationPercent())
			}
		}
		if tier == Focused {
			if len(snap.Surveys) == 0 {
				t.Error("focused poll returned no surveys")
			}
			for _, s := range snap.Surveys {
				// Counters, not a ratio: utilization is computed from deltas in
				// internal/telemetry. Printing the raw values here keeps the
				// wrong formula out of even a log line.
				t.Logf("  survey %s %dMHz noise=%ddBm active=%dms busy=%dms",
					s.Iface, s.MHz, s.NoiseDBm(), s.ActiveTime, s.BusyTime)
				if s.ActiveTime == 0 {
					t.Errorf("%s: survey has no active time", s.Iface)
				}
				if dbm := s.NoiseDBm(); dbm > -30 || dbm < -120 {
					t.Errorf("%s: noise %d dBm is outside any plausible range — "+
						"the unsigned/signed conversion is wrong", s.Iface, dbm)
				}
			}
			for _, st := range snap.Stations {
				t.Logf("  station %s on %s signal=%d rx=%dkbit tx=%dkbit retries=%d",
					st.MAC, st.Iface, st.Signal, st.RX.Rate, st.TX.Rate, st.TX.Retries)
			}
		} else if len(snap.Stations) != 0 || len(snap.Surveys) != 0 {
			t.Errorf("baseline poll paid for iwinfo: %d stations, %d surveys",
				len(snap.Stations), len(snap.Surveys))
		}
	}
}

// The tier split exists because iwinfo is ~92% of a focused poll. If that stops
// being true the split is pointless, and this is where we would find out.
func TestIntegrationBaselineIsMuchCheaperThanFocused(t *testing.T) {
	ctx := context.Background()
	c := New(newRecorder(), Options{Log: quiet()})
	c.Add(Target{DeviceID: 1, MAC: "integration", Name: "device", Connect: realConnect(t)})
	p := c.pollers[1]

	client, err := p.dial(ctx, p.target)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	ifaces, err := p.discoverIfaces(ctx, client)
	if err != nil {
		t.Fatalf("iwinfo.devices: %v", err)
	}
	modes, err := p.discoverIfaceModes(ctx, client)
	if err != nil {
		t.Fatalf("luci-rpc.getWirelessDevices: %v", err)
	}

	median := func(tier Tier) time.Duration {
		var best time.Duration
		for i := range 5 {
			p.boardAt = time.Now() // steady state: no board read
			snap := p.poll(ctx, client, tier, ifaces, modes)
			if snap.Err != nil {
				t.Fatalf("%s poll %d: %v", tier, i, snap.Err)
			}
			if best == 0 || snap.Duration < best {
				best = snap.Duration
			}
		}
		return best
	}
	base, focus := median(Baseline), median(Focused)
	t.Logf("baseline %v, focused %v (best of 5 each)",
		base.Round(time.Millisecond), focus.Round(time.Millisecond))
	if base >= focus {
		t.Errorf("baseline (%v) is not cheaper than focused (%v); the tier split "+
			"is buying nothing", base, focus)
	}
}
