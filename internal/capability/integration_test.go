//go:build integration

package capability

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/ubus"
)

// Probes the real device and asserts the conclusions match what was measured by
// hand in docs/IMPLEMENTATION.md §14. If the probe and the documented findings
// ever disagree, one of them is wrong and it matters which.
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

	// Measured facts about this reference device.
	if r.Class != ClassA {
		t.Errorf("WRT3200ACM is class A (mvebu), got %s", r.Class)
	}
	if got := r.State(FeatDSA); got != Present {
		t.Errorf("this device HAS a DSA switch; got %s", got)
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

	// Radios are up, so survey should be usable while the airtime split is not:
	// mwlwifi leaves rx_time/tx_time uninitialised.
	if len(r.Radios) == 0 {
		t.Skip("no radios enabled; skipping the wifi-dependent assertions")
	}
	if got := r.State(FeatSurvey); got != Present {
		t.Errorf("iwinfo.survey works natively on mwlwifi; got %s", got)
	}
	if got := r.State(FeatAirtimeSplit); got == Present {
		t.Error("the airtime split must NOT be advertised on mwlwifi: " +
			"rx_time/tx_time are uninitialised there")
	}
	if !r.HasQuirk("iwinfo.survey", "rx_time/tx_time") {
		t.Error("expected the rx_time/tx_time quirk to be recorded")
	}
	if !r.HasQuirk("iwinfo.survey", "noise") {
		t.Error("expected the unsigned-noise quirk to be recorded")
	}
}
