package discovery

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// rpcdList is the shape stock OpenWrt returns for `list ["*"]` on the null
// session, trimmed to the objects the fingerprint and the signals read. Taken
// from a real response captured off the reference device 2026-08-14.
const rpcdList = `{"jsonrpc":"2.0","id":1,"result":{
  "session":{"login":{"username":"string","password":"string"},"destroy":{}},
  "uci":{"get":{},"set":{},"apply":{"rollback":"boolean"},"confirm":{}},
  "system":{"board":{},"info":{}},
  "iwinfo":{"devices":{},"info":{"device":"string"}},
  "dnsmasq":{"metrics":{}},
  "hostapd.phy0-ap0":{"get_status":{}},
  "hostapd.phy1-ap0":{"get_status":{}},
  "network.interface.lan":{"status":{}},
  "network.interface.wan":{"status":{}},
  "file":{"read":{},"write":{}}
}}`

// fakeDevice serves the probe response a device would.
func fakeDevice(t *testing.T, body string, status int) (host string, port int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ubus" {
			http.NotFound(w, r)
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	h, p, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(p)
	return h, n
}

func TestProbeIdentifiesRPCD(t *testing.T) {
	host, port := fakeDevice(t, rpcdList, http.StatusOK)
	c := Probe(context.Background(), host, port, "http", Options{})

	if c.Verdict != VerdictOpenWrt {
		t.Fatalf("verdict = %q, want %q (note: %s)", c.Verdict, VerdictOpenWrt, c.Note)
	}
	// Two hostapd BSSes on two distinct PHYs is two radios, not two of
	// anything else. A device with two SSIDs on one radio must not read as two.
	if c.Signals.Radios != 2 {
		t.Errorf("radios = %d, want 2", c.Signals.Radios)
	}
	if !c.Signals.Gateway {
		t.Error("a device with network.interface.wan should retain the WAN-object compatibility hint")
	}
	if !c.Signals.Wireless || !c.Signals.DHCP {
		t.Errorf("wireless=%v dhcp-object=%v, want both true", c.Signals.Wireless, c.Signals.DHCP)
	}
	if c.Signals.Objects != 10 {
		t.Errorf("objects = %d, want 10", c.Signals.Objects)
	}
}

func TestRadiosCountPHYsNotBSSes(t *testing.T) {
	// Three SSIDs, all on phy0. One radio.
	body := `{"result":{
	  "session":{"login":{}},"uci":{},"system":{"board":{}},
	  "hostapd.phy0-ap0":{},"hostapd.phy0-ap1":{},"hostapd.phy0-ap2":{}}}`
	host, port := fakeDevice(t, body, http.StatusOK)
	c := Probe(context.Background(), host, port, "http", Options{})
	if c.Verdict != VerdictOpenWrt {
		t.Fatalf("verdict = %q", c.Verdict)
	}
	if c.Signals.Radios != 1 {
		t.Errorf("radios = %d, want 1 — three BSSes share one PHY", c.Signals.Radios)
	}
}

func TestDHCPCompatibilitySignalIncludesOdhcpdObject(t *testing.T) {
	// odhcpd publishes `dhcp`, not `dnsmasq`. The compatibility boolean covers
	// either service, so callers must use a generic label rather than naming one
	// implementation.
	body := `{"result":{
	  "session":{"login":{}},"uci":{},"system":{"board":{}},
	  "dhcp":{"ipv4leases":{},"ipv6leases":{}}}}`
	host, port := fakeDevice(t, body, http.StatusOK)
	c := Probe(context.Background(), host, port, "http", Options{})
	if c.Verdict != VerdictOpenWrt {
		t.Fatalf("verdict = %q, want %q", c.Verdict, VerdictOpenWrt)
	}
	if !c.Signals.DHCP {
		t.Fatal("odhcpd's dhcp object did not set the DHCP-service compatibility signal")
	}
}

// The fingerprint has to be strong enough to put an address in front of an
// operator as "type your router password here". One matching object is not.
func TestFingerprintRejectsPartialMatches(t *testing.T) {
	cases := map[string]string{
		"session only":     `{"result":{"session":{"login":{}}}}`,
		"no login method":  `{"result":{"session":{"destroy":{}},"uci":{},"system":{"board":{}}}}`,
		"no uci":           `{"result":{"session":{"login":{}},"system":{"board":{}}}}`,
		"no system.board":  `{"result":{"session":{"login":{}},"uci":{},"system":{"info":{}}}}`,
		"empty result":     `{"result":{}}`,
		"not json at all":  `<html><body>router login</body></html>`,
		"json but not rpc": `{"status":"ok","devices":[]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			host, port := fakeDevice(t, body, http.StatusOK)
			c := Probe(context.Background(), host, port, "http", Options{})
			if c.Verdict != VerdictReachable {
				t.Errorf("verdict = %q, want %q — the host answered but is not "+
					"manageable, and claiming otherwise sends an operator's "+
					"router password to it", c.Verdict, VerdictReachable)
			}
		})
	}
}

// A 404 means an OpenWrt device without uhttpd-mod-ubus just as easily as it
// means something else entirely, so the note must not claim "not OpenWrt".
func TestNoUbusEndpointIsReportedAsSuch(t *testing.T) {
	host, port := fakeDevice(t, "", http.StatusNotFound)
	c := Probe(context.Background(), host, port, "http", Options{})
	if c.Verdict != VerdictReachable {
		t.Fatalf("verdict = %q, want %q", c.Verdict, VerdictReachable)
	}
	if !strings.Contains(c.Note, "uhttpd-mod-ubus") {
		t.Errorf("note = %q; it should name the missing package rather than "+
			"assert the device is not OpenWrt", c.Note)
	}
}

func TestSilentAddressIsNotReachable(t *testing.T) {
	// A port nothing is listening on: bind and immediately release it so the
	// number is real but dead.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().(*net.TCPAddr)
	l.Close()

	c := Probe(context.Background(), "127.0.0.1", addr.Port, "http",
		Options{DialTimeout: 300 * time.Millisecond})
	if c.Verdict != VerdictSilent {
		t.Errorf("verdict = %q, want %q", c.Verdict, VerdictSilent)
	}
}

// The probe must not be steerable by the host it is probing.
func TestProbeDoesNotFollowRedirects(t *testing.T) {
	var elsewhereHit bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereHit = true
		w.Write([]byte(rpcdList))
	}))
	defer elsewhere.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/ubus", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	h, p, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(p)
	c := Probe(context.Background(), h, port, "http", Options{})

	if elsewhereHit {
		t.Fatal("the probe followed a redirect: a host being probed could point " +
			"the controller at any address it likes")
	}
	if c.Verdict == VerdictOpenWrt {
		t.Error("a redirecting host was reported as a manageable device")
	}
}

// A hostile host must not be able to make the controller read an unbounded body.
func TestProbeBodyIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Far more than maxProbeBody, written forever until the client stops.
		chunk := strings.Repeat("A", 32<<10)
		for i := 0; i < 200; i++ {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	h, p, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(p)
	done := make(chan Candidate, 1)
	go func() {
		done <- Probe(context.Background(), h, port, "http", Options{})
	}()
	select {
	case c := <-done:
		if c.Verdict == VerdictOpenWrt {
			t.Error("a host returning 6 MB of 'A' was identified as a device")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the probe did not bound the response body")
	}
}

// ---- target expansion ----

func TestHostsInSkipsNetworkAndBroadcast(t *testing.T) {
	_, n, _ := net.ParseCIDR("192.168.1.0/24")
	hosts := hostsIn(n)
	if len(hosts) != 254 {
		t.Fatalf("a /24 has 254 usable hosts, got %d", len(hosts))
	}
	if hosts[0] != "192.168.1.1" {
		t.Errorf("first host = %s, want 192.168.1.1 (the network address is not a host)", hosts[0])
	}
	if hosts[len(hosts)-1] != "192.168.1.254" {
		t.Errorf("last host = %s, want 192.168.1.254 (the broadcast address is not a host)",
			hosts[len(hosts)-1])
	}
}

func TestHostsInHandlesPointToPointPrefixes(t *testing.T) {
	// A /31 is two usable addresses and a /32 is one; neither has a network or
	// broadcast address to skip, and the generic rule would return -1 hosts.
	for cidr, want := range map[string]int{
		"10.0.0.0/31": 2,
		"10.0.0.7/32": 1,
		"10.0.0.0/30": 2,
		"10.0.0.0/22": 1022,
	} {
		_, n, _ := net.ParseCIDR(cidr)
		if got := len(hostsIn(n)); got != want {
			t.Errorf("%s: %d hosts, want %d", cidr, got, want)
		}
	}
}

// A typo'd mask must not become a scan of somebody else's network.
func TestOversizedNetworksAreRefusedNotClamped(t *testing.T) {
	nets, skipped, err := ParseNetworks([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	if len(nets) != 0 {
		t.Errorf("a /8 was accepted for sweeping")
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "10.0.0.0/8") {
		t.Fatalf("skipped = %v; the refusal has to name the network, or an "+
			"operator reads the empty result as 'no devices'", skipped)
	}
}

func TestIPv6IsSkippedWithAReason(t *testing.T) {
	_, skipped, err := ParseNetworks([]string{"2001:db8::/64"})
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 {
		t.Fatalf("skipped = %v, want one entry", skipped)
	}
}

func TestMalformedNetworkIsAnErrorNotASilentSkip(t *testing.T) {
	if _, _, err := ParseNetworks([]string{"192.168.1.0/33"}); err == nil {
		t.Error("an invalid CIDR should be rejected loudly; skipping it silently " +
			"would sweep nothing and report success")
	}
}

// expand drops a whole network rather than truncating one, so the result never
// claims to have covered a range it stopped part-way through.
func TestExpandDropsWholeNetworksAtTheCap(t *testing.T) {
	var nets []*net.IPNet
	for i := 0; i < 20; i++ {
		_, n, _ := net.ParseCIDR("10.0." + strconv.Itoa(i) + ".0/24")
		nets = append(nets, n)
	}
	hosts, skipped := expand(nets)
	if len(hosts) > MaxHosts {
		t.Errorf("expanded to %d hosts, over the %d cap", len(hosts), MaxHosts)
	}
	if len(skipped) == 0 {
		t.Error("networks were dropped without saying so")
	}
	// Every kept network must be complete: 254 each, no partial one.
	if len(hosts)%254 != 0 {
		t.Errorf("%d hosts is not a whole number of /24s — a network was truncated",
			len(hosts))
	}
}

func TestSweepReportsWhatItSwept(t *testing.T) {
	host, port := fakeDevice(t, rpcdList, http.StatusOK)
	if host != "127.0.0.1" {
		t.Skipf("httptest bound %s, not loopback", host)
	}
	// A /30 around the loopback device: four addresses, two usable.
	res, err := Sweep(context.Background(), Options{
		Networks:    []string{"127.0.0.0/30"},
		Ports:       []int{port},
		DialTimeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Swept != 2 {
		t.Errorf("swept = %d, want 2", res.Swept)
	}
	if len(res.Found) != 1 || res.Found[0].Host != "127.0.0.1" {
		t.Fatalf("found = %+v, want just 127.0.0.1", res.Found)
	}
	if res.Networks == nil {
		t.Error("the result does not say which networks were swept")
	}
}

// EHOSTUNREACH/ENETUNREACH are facts about the controller's route, not about
// whether an address has a device on it. If every attempt in one CIDR returns
// either error, an empty Found must not be presented as a successful empty
// sweep. A second CIDR returning ordinary connection refusals proves failures
// are attributed per network rather than poisoning the whole request.
func TestSweepSeparatesUnroutableNetworksFromQuietOnes(t *testing.T) {
	res, err := Sweep(context.Background(), Options{
		Networks: []string{"127.0.0.0/30", "127.0.0.4/30"},
		Ports:    []int{80},
		dialContext: func(_ context.Context, _, address string) (net.Conn, error) {
			host, _, splitErr := net.SplitHostPort(address)
			if splitErr != nil {
				return nil, splitErr
			}
			if host == "127.0.0.1" {
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.EHOSTUNREACH}
			}
			if host == "127.0.0.2" {
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ENETUNREACH}
			}
			return nil, syscall.ECONNREFUSED
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Swept != 4 || res.Answered != 0 || len(res.Found) != 0 {
		t.Fatalf("unexpected sweep summary: %+v", res)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("failures = %+v, want only the unroutable CIDR", res.Failures)
	}
	got := res.Failures[0]
	if got.Network != "127.0.0.0/30" || got.Reason != FailureUnreachable || got.Attempts != 2 {
		t.Errorf("failure = %+v, want 127.0.0.0/30 unreachable after 2 attempts", got)
	}
}

// One route error is not enough to condemn a subnet. The other address may be
// quiet, firewalled, or absent, but its ordinary refusal proves the route can
// be used and the sweep completed.
func TestSweepDoesNotCallAMixedNetworkUnreachable(t *testing.T) {
	res, err := Sweep(context.Background(), Options{
		Networks: []string{"127.0.0.0/30"},
		Ports:    []int{80},
		dialContext: func(_ context.Context, _, address string) (net.Conn, error) {
			host, _, splitErr := net.SplitHostPort(address)
			if splitErr != nil {
				return nil, splitErr
			}
			if host == "127.0.0.1" {
				return nil, syscall.EHOSTUNREACH
			}
			return nil, syscall.ECONNREFUSED
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Failures) != 0 {
		t.Errorf("a partially routable network was reported failed: %+v", res.Failures)
	}
}

// The probe must never authenticate. A device with no root password hands out a
// root session to any password at all (measured), so a login-based probe would
// mint one on every passwordless host in the subnet, on every scan.
func TestSweepNeverAttemptsALogin(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		calls = append(calls, req.Method)
		if req.Method == "call" {
			// What a passwordless device does: hands over a root session.
			w.Write([]byte(`{"result":[0,{"ubus_rpc_session":"deadbeef"}]}`))
			return
		}
		w.Write([]byte(rpcdList))
	}))
	defer srv.Close()

	h, p, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(p)
	Probe(context.Background(), h, port, "http", Options{})

	for _, m := range calls {
		if m != "list" {
			t.Errorf("the probe issued a %q, not a bare list — discovery must not "+
				"attempt credentials against hosts nobody has chosen to manage", m)
		}
	}
	if len(calls) != 1 {
		t.Errorf("the probe made %d requests, want exactly 1", len(calls))
	}
}

// ---- local interfaces ----

func TestLocalNetworksExcludesLoopbackAndTunnels(t *testing.T) {
	nets, skipped, err := LocalNetworks()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nets {
		if n.IP.IsLoopback() {
			t.Errorf("loopback %s would be swept", n)
		}
		if ones, _ := n.Mask.Size(); ones < MinPrefix {
			t.Errorf("%s is wider than /%d and would be swept", n, MinPrefix)
		}
	}
	// This machine is on a network or it is not, and either is fine — what is
	// not fine is reporting neither a network nor a reason.
	if len(nets) == 0 && len(skipped) == 0 {
		t.Error("no networks and no explanation: an operator would read this as " +
			"'no devices found'")
	}
	t.Logf("sweepable: %v", NetworkStrings(nets))
	for _, s := range skipped {
		t.Logf("skipped: %s", s)
	}
}
