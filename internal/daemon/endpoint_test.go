package daemon

import (
	"context"
	"errors"
	"net/netip"
	"testing"
)

type fixedHostResolver struct {
	ips   []netip.Addr
	err   error
	calls int
	host  string
}

func (r *fixedHostResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	r.calls++
	r.host = host
	if network != "ip" {
		return nil, errors.New("unexpected resolver network")
	}
	return r.ips, r.err
}

func TestWorkflowEndpointResolvesAHostnameOnceAndPinsEveryTransport(t *testing.T) {
	r := &fixedHostResolver{ips: []netip.Addr{
		netip.MustParseAddr("192.0.2.10"),
		netip.MustParseAddr("192.0.2.11"),
	}}
	e, err := resolveWorkflowEndpoint(context.Background(), r, "router.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	if r.calls != 1 || r.host != "router.example" {
		t.Fatalf("resolver calls=%d host=%q, want one lookup of router.example", r.calls, r.host)
	}
	httpHost, err := e.httpAuthority(0, false)
	if err != nil {
		t.Fatal(err)
	}
	if httpHost != "192.0.2.10:8080" || e.sshAddress() != "192.0.2.10:8080" {
		t.Fatalf("workflow was not pinned to one address: http=%q ssh=%q", httpHost, e.sshAddress())
	}
	if got := e.inventoryHost(false); got != "192.0.2.10:8080" {
		t.Fatalf("plain HTTP inventory kept a mutable hostname: %q", got)
	}
	if got := e.inventoryHost(true); got != "router.example:8080" {
		t.Fatalf("certificate-pinned inventory lost the operator hostname: %q", got)
	}
}

func TestWorkflowEndpointDoesNotResolveLiteralAddresses(t *testing.T) {
	r := &fixedHostResolver{err: errors.New("literal address reached DNS")}
	for _, tc := range []struct {
		input string
		http  string
		ssh   string
	}{
		{"192.0.2.8", "192.0.2.8:80", "192.0.2.8"},
		{"192.0.2.8:8080", "192.0.2.8:8080", "192.0.2.8:8080"},
		{"2001:db8::8", "[2001:db8::8]:80", "2001:db8::8"},
		{"[2001:db8::8]:8080", "[2001:db8::8]:8080", "[2001:db8::8]:8080"},
	} {
		e, err := resolveWorkflowEndpoint(context.Background(), r, tc.input)
		if err != nil {
			t.Fatalf("%s: %v", tc.input, err)
		}
		httpHost, err := e.httpAuthority(0, false)
		if err != nil {
			t.Fatalf("%s authority: %v", tc.input, err)
		}
		if httpHost != tc.http || e.sshAddress() != tc.ssh {
			t.Errorf("%s: http=%q ssh=%q, want %q / %q", tc.input, httpHost, e.sshAddress(), tc.http, tc.ssh)
		}
	}
	if r.calls != 0 {
		t.Fatalf("literal addresses caused %d DNS lookup(s)", r.calls)
	}
}

func TestWorkflowEndpointRejectsTwoPortSources(t *testing.T) {
	e, err := resolveWorkflowEndpoint(context.Background(), &fixedHostResolver{
		ips: []netip.Addr{netip.MustParseAddr("192.0.2.20")},
	}, "router.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.httpAuthority(8443, true); err == nil {
		t.Fatal("host port plus explicit port was accepted")
	}
}

func TestEffectiveHTTPSPortIsNotStoredAsHTTP(t *testing.T) {
	if got := effectiveDevicePort(0, true); got != 443 {
		t.Fatalf("default HTTPS port=%d, want 443", got)
	}
	if got := effectiveDevicePort(8443, true); got != 8443 {
		t.Fatalf("explicit HTTPS port=%d, want 8443", got)
	}
}
