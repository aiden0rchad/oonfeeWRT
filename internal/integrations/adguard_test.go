package integrations

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

type responseTransport func(*http.Request) (*http.Response, error)

func (f responseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAdGuardConfigAllowsExplicitLANButRejectsUnsafeTargets(t *testing.T) {
	for _, host := range []string{"https://10.0.0.2:3000/", "https://dns.example.test", "https://[fd12::1]", "https://192.168.1.1"} {
		config := AdGuardConfig{URL: host, Username: "reader", Password: "sentinel"}
		if err := config.Normalize(); err != nil {
			t.Fatalf("%s: %v", host, err)
		}
	}
	for _, host := range []string{
		"https://0.0.0.1", "https://240.0.0.1", "https://255.255.255.255", "https://192.0.2.1", "https://198.18.0.1", "https://198.51.100.1", "https://203.0.113.1", "https://[2001:db8::1]",
		"http://10.0.0.2", "https://user:secret@10.0.0.2", "https://dns.test/?token=secret", "https://dns.test/#secret", "https://dns.test/control/status", "https://dns.test/%2e%2e", "https://127.0.0.1", "https://[::1]", "https://[::ffff:127.0.0.1]", "https://169.254.169.254", "https://168.63.129.16", "https://100.100.100.200", "https://[fd00:ec2::254]", "https://[64:ff9b::a9fe:a9fe]", "https://0.0.0.0", "https://224.0.0.1", "https://[fe80::1%25en0]", "https://dns.test:0", "https://dns.test:65536", "https://dns.test:",
	} {
		config := AdGuardConfig{URL: host}
		if err := config.Normalize(); err == nil {
			t.Fatalf("accepted %s", host)
		}
	}
}

func TestAdGuardDialValidatesEveryDNSAnswerBeforeConnecting(t *testing.T) {
	for _, addresses := range [][]net.IPAddr{
		{{IP: net.ParseIP("0.0.0.1")}}, {{IP: net.ParseIP("255.255.255.255")}},
		{{IP: net.ParseIP("10.0.0.2")}, {IP: net.ParseIP("169.254.169.254")}},
		{{IP: net.ParseIP("127.0.0.1")}}, {{IP: net.ParseIP("fd00:ec2::254")}},
	} {
		called := false
		_, err := dialAdGuard(context.Background(), "dns.example.test:443", func(context.Context, string) ([]net.IPAddr, error) { return addresses, nil }, func(context.Context, string, string) (net.Conn, error) { called = true; return nil, nil })
		if err == nil || called {
			t.Fatalf("unsafe DNS set connected: %+v", addresses)
		}
	}
	var lookedUp, dialed string
	_, err := dialAdGuard(context.Background(), "dns.example.test:8443", func(_ context.Context, host string) ([]net.IPAddr, error) {
		lookedUp = host
		return []net.IPAddr{{IP: net.ParseIP("10.0.0.2")}}, nil
	}, func(_ context.Context, _, address string) (net.Conn, error) {
		dialed = address
		return nil, errors.New("fixture stop")
	})
	if err == nil || lookedUp != "dns.example.test" || dialed != "10.0.0.2:8443" {
		t.Fatalf("did not pin resolved address: %s %s %v", lookedUp, dialed, err)
	}
}

func TestAdGuardTLSUsesNormalTrustOrExactExplicitPin(t *testing.T) {
	if config := adGuardTLS(""); config.InsecureSkipVerify || config.MinVersion != tls.VersionTLS12 {
		t.Fatal("default certificate verification weakened")
	}
	certificate := &x509.Certificate{Raw: []byte("certificate-fixture"), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	digest := sha256.Sum256(certificate.Raw)
	config := adGuardTLS(hex.EncodeToString(digest[:]))
	if !config.InsecureSkipVerify || config.VerifyConnection == nil {
		t.Fatal("pin verifier missing")
	}
	if err := config.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{certificate}}); err != nil {
		t.Fatal(err)
	}
	certificate.Raw = []byte("different-certificate")
	if err := config.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{certificate}}); err == nil {
		t.Fatal("mismatching certificate accepted")
	}
	certificate.Raw = []byte("certificate-fixture")
	certificate.NotAfter = time.Now().Add(-time.Minute)
	if err := config.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{certificate}}); err == nil {
		t.Fatal("expired pinned certificate accepted")
	}
	client := newAdGuardClient(AdGuardConfig{})
	transport := client.Transport.(*http.Transport)
	if transport.Proxy != nil || !transport.DisableKeepAlives || transport.DialContext == nil {
		t.Fatal("proxy or unvalidated dial route exists")
	}
	if transport.MaxResponseHeaderBytes != 16<<10 {
		t.Fatal("response headers are not bounded")
	}
	if client.CheckRedirect(&http.Request{}, nil) == nil {
		t.Fatal("redirect allowed")
	}
}

func testAdGuardChecker(t *testing.T, status, stats string) *AdGuardChecker {
	t.Helper()
	c := NewAdGuardChecker()
	c.clientFactory = func(config AdGuardConfig) *http.Client {
		return &http.Client{Transport: responseTransport(func(request *http.Request) (*http.Response, error) {
			if request.Method != http.MethodGet || request.URL.Scheme != "https" || request.URL.Host != "dns.example.test" {
				t.Fatalf("unsafe request: %s %s", request.Method, request.URL)
			}
			user, password, ok := request.BasicAuth()
			if !ok || user != "reader" || password != "PASSWORD-SENTINEL" {
				t.Fatal("wrong authentication")
			}
			body := status
			if request.URL.Path == "/control/stats" {
				body = stats
			} else if request.URL.Path != "/control/status" {
				t.Fatalf("unexpected endpoint %s", request.URL.Path)
			}
			if body == "failure" {
				return nil, errors.New("PASSWORD-SENTINEL in upstream failure")
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})}
	}
	return c
}

var aghConfig = AdGuardConfig{URL: "https://dns.example.test", Username: "reader", Password: "PASSWORD-SENTINEL"}

func TestAdGuardReportsOnlyProvenAggregates(t *testing.T) {
	c := testAdGuardChecker(t, `{"version":"v0.107.70","running":true,"protection_enabled":false}`, `{"num_dns_queries":0,"num_blocked_filtering":0,"avg_processing_time":0.002,"top_queried_domains":[{"PRIVATE-DOMAIN":5}]}`)
	result := c.Check(context.Background(), aghConfig)
	if result.State != "observed" || result.Running == nil || !*result.Running || result.ProtectionEnabled == nil || *result.ProtectionEnabled || result.DNSQueries == nil || *result.DNSQueries != 0 || result.AvgProcessingMS == nil || *result.AvgProcessingMS != 2 || result.SourceURL != aghConfig.URL {
		t.Fatalf("%+v", result)
	}
	data, _ := json.Marshal(result)
	if strings.Contains(string(data), "PRIVATE-DOMAIN") || strings.Contains(string(data), "PASSWORD-SENTINEL") {
		t.Fatalf("private upstream fields leaked: %s", data)
	}
}

func TestAdGuardPartialFieldsRemainUnknownAndFailuresDoNotReplay(t *testing.T) {
	c := testAdGuardChecker(t, `{"version":"v0.107.70","running":true,"protection_enabled":true}`, `{"num_dns_queries":0}`)
	result := c.Check(context.Background(), aghConfig)
	if result.State != "partial" || result.DNSQueries == nil || *result.DNSQueries != 0 || result.BlockedFiltering != nil || result.AvgProcessingMS != nil {
		t.Fatalf("unknowns collapsed into zero: %+v", result)
	}
	c = testAdGuardChecker(t, "failure", "failure")
	result = c.Check(context.Background(), aghConfig)
	data, _ := json.Marshal(result)
	if result.State != "unavailable" || result.DNSQueries != nil || result.Running != nil || strings.Contains(string(data), "PASSWORD-SENTINEL") {
		t.Fatalf("unsafe failure: %s", data)
	}
}

func TestAdGuardRejectsMalformedAndOversizedNumbersAndBodies(t *testing.T) {
	for _, stats := range []string{`{"num_dns_queries":-1,"num_blocked_filtering":-1,"avg_processing_time":-2}`, `{"num_dns_queries":9007199254740992}`, `{"num_dns_queries":"0"}`, strings.Repeat(" ", (2<<20)+1)} {
		result := testAdGuardChecker(t, `{}`, stats).Check(context.Background(), aghConfig)
		if result.State != "unavailable" || result.DNSQueries != nil || result.AvgProcessingMS != nil {
			t.Fatalf("invalid data trusted: %+v", result)
		}
	}
}
