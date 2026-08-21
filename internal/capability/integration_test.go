//go:build integration

package capability

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

// Probes either measured lab device and asserts the conclusions match the
// hardware record. If the probe and the device disagree, one of them is wrong
// and it matters which.
//
//	OONFEE_TEST_HOST=192.168.1.1 OONFEE_TEST_USER=oonfeewrt \
//	OONFEE_TEST_PASS=... go test -tags=integration ./internal/capability/ -v

func TestIntegrationProbeMatchesMeasuredFindings(t *testing.T) {
	host, user, pass := os.Getenv("OONFEE_TEST_HOST"),
		os.Getenv("OONFEE_TEST_USER"), os.Getenv("OONFEE_TEST_PASS")
	if host == "" || user == "" {
		t.Skip("set OONFEE_TEST_HOST and OONFEE_TEST_USER")
	}
	ctx := context.Background()
	c := ubus.New(ubus.Options{Host: host, Timeout: 30 * time.Second})
	if err := c.Login(ctx, user, pass); err != nil {
		t.Fatalf("login: %v", err)
	}
	defer c.Close()

	r, err := Probe(ctx, c)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	t.Logf("%s", r.Summary())
	for _, n := range r.Notes {
		t.Logf("note: %s", n)
	}
	for _, q := range r.Quirks {
		t.Logf("quirk: %s.%s — %s", q.Source, q.Field, q.Reason)
	}

	// Shared measured facts.
	if got := r.State(FeatSwitchPorts); got != Present {
		t.Errorf("this device exposes switch-port state; got %s", got)
	}
	if got := r.State(FeatBridgeFDB); got != Present {
		t.Errorf("this device exposes its bridge forwarding database; got %s", got)
	}
	if got := r.State(FeatFirewall4); got != Present {
		t.Errorf("this device HAS firewall4/nftables; got %s", got)
	}
	if got := r.State(FeatBatching); got != Present {
		t.Errorf("this uhttpd build accepts batches; got %s", got)
	}
	if got := r.State(FeatPreflightDirty); got != Present {
		t.Errorf("the ACL grants file.list on /tmp/.uci; got %s", got)
	}

	// Radios are up on both lab devices, so survey should be usable.
	if len(r.Radios) == 0 {
		t.Skip("no radios enabled; skipping the wifi-dependent assertions")
	}
	if got := r.State(FeatSurvey); got != Present {
		t.Errorf("iwinfo.survey works natively on this device; got %s", got)
	}
	if !r.HasQuirk("iwinfo.survey", "noise") {
		t.Error("expected the unsigned-noise quirk to be recorded")
	}

	switch r.Board.BoardName {
	case "linksys,wrt3200acm":
		if r.Class != ClassA {
			t.Errorf("WRT3200ACM is class A (mvebu), got %s", r.Class)
		}
		if got := r.State(FeatDSA); got != Present {
			t.Errorf("WRT3200ACM has a DSA switch; got %s", got)
		}
		if got := r.State(FeatAirtimeSplit); got == Present {
			t.Error("the airtime split must not be advertised on mwlwifi")
		}
		if !r.HasQuirk("iwinfo.survey", "rx_time/tx_time") {
			t.Error("expected the mwlwifi rx_time/tx_time quirk")
		}
	case "tplink,archer-c6-v2-us":
		if r.Class != ClassC {
			t.Errorf("QCA956X Archer C6 is measured class C, got %s", r.Class)
		}
		if got := r.State(FeatDSA); got != Absent {
			t.Errorf("Archer C6 is a legacy swconfig device, DSA=%s", got)
		}
	default:
		t.Fatalf("no measured expectations for %q", r.Board.BoardName)
	}
}
