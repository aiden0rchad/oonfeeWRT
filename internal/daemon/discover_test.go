package daemon

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/discovery"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

// The object list stock OpenWrt returns to an unauthenticated `list ["*"]`,
// trimmed to what the fingerprint needs.
const fakeRPCDList = `{"jsonrpc":"2.0","id":1,"result":{
  "session":{"login":{"username":"string","password":"string"}},
  "uci":{"get":{},"apply":{}},
  "system":{"board":{},"info":{}},
  "hostapd.phy0-ap0":{"get_status":{}}
}}`

// fakeUbusHost serves the probe response on loopback and reports its port.
func fakeUbusHost(t *testing.T) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ubus" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(fakeRPCDList))
	}))
	t.Cleanup(srv.Close)
	_, p, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(p)
	return port
}

// A device already in the inventory must be listed and labelled, not filtered
// out: hiding it turns "my router is missing from the scan" into a question
// with two indistinguishable answers.
func TestAnnotateLabelsAdoptedDevicesRatherThanHidingThem(t *testing.T) {
	at := int64(1)
	known := map[string]*store.Device{
		"192.168.1.1": {ID: 7, Host: "192.168.1.1", Name: "already here", AdoptedAt: &at},
		// In the inventory but never adopted: it has no controller credential,
		// so it is a candidate exactly like an unknown address.
		"192.168.1.9": {ID: 8, Host: "192.168.1.9", Name: "half-added"},
	}
	res := &discovery.Result{
		Swept: 254, Answered: 3,
		Found: []discovery.Candidate{
			{Host: "192.168.1.1", Port: 80, Verdict: discovery.VerdictOpenWrt},
			{Host: "192.168.1.9", Port: 80, Verdict: discovery.VerdictOpenWrt},
			{Host: "192.168.1.50", Port: 80, Verdict: discovery.VerdictOpenWrt},
		},
	}
	out := annotate(res, known)

	if len(out.Found) != 3 {
		t.Fatalf("found %d candidates, want all 3 kept", len(out.Found))
	}
	if out.Found[0].KnownDeviceID != 7 || out.Found[0].KnownName != "already here" {
		t.Errorf("the adopted device was not labelled: %+v", out.Found[0])
	}
	if out.Found[1].KnownDeviceID != 0 {
		t.Errorf("192.168.1.9 is in the inventory but NOT adopted, so it is still "+
			"a candidate; labelling it as managed would hide the adopt button on "+
			"the one device that needs it: %+v", out.Found[1])
	}
	if out.Found[2].KnownDeviceID != 0 {
		t.Errorf("an unknown address was labelled as managed: %+v", out.Found[2])
	}
	if out.Swept != 254 || out.Answered != 3 {
		t.Errorf("the sweep summary was lost in translation: swept=%d answered=%d",
			out.Swept, out.Answered)
	}
}

// Annotation must not erase the distinction the sweep established. This seam
// is the last step before JSON, and a missing copy here turns a proven route
// failure back into the same empty result that caused the original incident.
func TestAnnotatePreservesNetworkFailures(t *testing.T) {
	res := &discovery.Result{
		Swept: 254,
		Failures: []discovery.NetworkFailure{{
			Network: "192.168.1.0/24", Reason: discovery.FailureUnreachable,
			Attempts: 254,
		}},
	}

	out := annotate(res, nil)
	if len(out.Failures) != 1 || out.Failures[0].Network != "192.168.1.0/24" ||
		out.Failures[0].Reason != discovery.FailureUnreachable ||
		out.Failures[0].Attempts != 254 {
		t.Fatalf("network failure was lost or changed: %+v", out.Failures)
	}
}

// The sweep probes the standard port and reports honestly when that finds
// nothing, which is the case an operator hits when their device is elsewhere.
func TestScanReportsAnEmptySweepLegibly(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, testConfig(t, "operator passphrase"), quietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	// A server on a random loopback port: reachable, but not where a scan looks.
	fakeUbusHost(t)

	res, err := d.Scan(ctx, api.ScanRequest{Networks: []string{"127.0.0.0/30"}})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(res.Found) != 0 {
		t.Fatalf("found %d devices on port 80, expected none", len(res.Found))
	}
	if res.Swept == 0 {
		t.Error("swept nothing; the result would be indistinguishable from a " +
			"scanner that did not run")
	}
	if res.Networks == nil {
		t.Error("the result does not name the networks it covered")
	}
}

// The plan has to answer "what will this do to my network" before it does it.
func TestPlanStatesSizeAndOmissions(t *testing.T) {
	d, err := Open(context.Background(), testConfig(t, "operator passphrase"), quietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	plan, err := d.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Networks) == 0 && len(plan.Skipped) == 0 {
		t.Error("neither a network to scan nor a reason there is none: an " +
			"operator reads that as 'no devices found'")
	}
	if len(plan.Networks) > 0 && plan.Hosts == 0 {
		t.Errorf("plan covers %v but reports 0 addresses", plan.Networks)
	}
	t.Logf("plan: %d addresses on %v", plan.Hosts, plan.Networks)
	for _, s := range plan.Skipped {
		t.Logf("skipped: %s", s)
	}
}

// An oversized or malformed network must be refused before anything is probed.
func TestScanRejectsAnUnsweepableNetwork(t *testing.T) {
	d, err := Open(context.Background(), testConfig(t, "operator passphrase"), quietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	res, err := d.Scan(context.Background(), api.ScanRequest{
		Networks: []string{"10.0.0.0/8"},
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if res.Swept != 0 {
		t.Errorf("swept %d addresses of a /8", res.Swept)
	}
	if len(res.Skipped) == 0 {
		t.Error("a refused network must say so; a silent empty result reads as " +
			"'nothing is there'")
	}
}
