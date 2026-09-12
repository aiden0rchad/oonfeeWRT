// Package integrations contains opt-in, read-only external service adapters.
package integrations

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// AdGuardConfig is encrypted as a whole; never return this type from an API.
type AdGuardConfig struct {
	URL            string `json:"url"`
	Username       string `json:"username"`
	Password       string `json:"password"`
	TLSFingerprint string `json:"tls_fingerprint"`
}

func (c *AdGuardConfig) Normalize() error {
	u, err := url.Parse(strings.TrimSpace(c.URL))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" || strings.Contains(u.Host, "%") || len(u.Host) > 253 {
		return errors.New("use an HTTPS server origin without credentials, path, query, or fragment")
	}
	if port := u.Port(); port != "" {
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return errors.New("the HTTPS port must be between 1 and 65535")
		}
	}
	if strings.HasSuffix(u.Host, ":") {
		return errors.New("the HTTPS port is empty")
	}
	if address, err := netip.ParseAddr(u.Hostname()); err == nil && !allowedAdGuardIP(address) {
		return errors.New("loopback, link-local, cloud-metadata, shared-address, and multicast targets are not allowed")
	}
	if len(c.Username) > 256 || strings.ContainsAny(c.Username, ":\r\n\x00") || len(c.Password) > 4096 || strings.ContainsAny(c.Password, "\r\n\x00") {
		return errors.New("the username or password is invalid or too long")
	}
	if c.Username == "" && c.Password != "" {
		return errors.New("a username is required when a password is set")
	}
	c.TLSFingerprint = strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(strings.TrimSpace(c.TLSFingerprint), "sha256:"), ":", ""))
	if c.TLSFingerprint != "" {
		pin, err := hex.DecodeString(c.TLSFingerprint)
		if err != nil || len(pin) != sha256.Size {
			return errors.New("the certificate fingerprint must be a SHA-256 value (64 hexadecimal characters)")
		}
	}
	u.Path, u.RawPath = "", ""
	u.Host = strings.ToLower(u.Host)
	c.URL = u.String()
	return nil
}

var deniedAdGuardNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("168.63.129.16/32"),
	netip.MustParsePrefix("fd00:ec2::254/128"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
}

func allowedAdGuardIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range deniedAdGuardNetworks {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true // Explicitly configured RFC1918 and IPv6 ULA services are supported.
}

type lookupIPs func(context.Context, string) ([]net.IPAddr, error)
type dialIP func(context.Context, string, string) (net.Conn, error)

func dialAdGuard(ctx context.Context, address string, lookup lookupIPs, dial dialIP) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("invalid integration destination")
	}
	addresses, err := lookup(ctx, host)
	if err != nil || len(addresses) == 0 || len(addresses) > 64 {
		return nil, errors.New("integration destination could not be resolved")
	}
	validated := make([]netip.Addr, 0, len(addresses))
	for _, resolved := range addresses {
		ip, ok := netip.AddrFromSlice(resolved.IP)
		if !ok || resolved.Zone != "" || !allowedAdGuardIP(ip) {
			return nil, errors.New("integration destination resolves to a restricted address")
		}
		validated = append(validated, ip.Unmap())
	}
	// Dial an already-validated literal, never resolve the hostname a second
	// time. The transport retains the original hostname for SNI/verification.
	for _, ip := range validated {
		conn, err := dial(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, errors.New("integration destination could not be reached")
}

func adGuardTLS(pin string) *tls.Config {
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	if pin == "" {
		return config
	}
	want, _ := hex.DecodeString(pin) // Caller already validated the exact SHA-256 length.
	config.InsecureSkipVerify = true // Replaced only by the operator's exact leaf-certificate pin below.
	config.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 || len(want) != sha256.Size {
			return errors.New("missing pinned certificate")
		}
		cert := state.PeerCertificates[0]
		got := sha256.Sum256(cert.Raw)
		if subtle.ConstantTimeCompare(got[:], want) != 1 || time.Now().Before(cert.NotBefore) || time.Now().After(cert.NotAfter) {
			return errors.New("the pinned certificate did not match or is outside its validity period")
		}
		return nil
	}
	return config
}

func newAdGuardClient(config AdGuardConfig) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("integration redirects are not followed")
		},
		Transport: &http.Transport{
			Proxy: nil, DisableKeepAlives: true, MaxResponseHeaderBytes: 16 << 10,
			DialContext: func(ctx context.Context, _ string, address string) (net.Conn, error) {
				return dialAdGuard(ctx, address, net.DefaultResolver.LookupIPAddr, dialer.DialContext)
			},
			TLSClientConfig: adGuardTLS(config.TLSFingerprint), TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second,
		},
	}
}

type AdGuardResult struct {
	State             string   `json:"state"`
	SourceURL         string   `json:"source_url"`
	CheckedAt         int64    `json:"checked_at"`
	Version           string   `json:"version,omitempty"`
	Running           *bool    `json:"running"`
	ProtectionEnabled *bool    `json:"protection_enabled"`
	DNSQueries        *int64   `json:"dns_queries"`
	BlockedFiltering  *int64   `json:"blocked_filtering"`
	AvgProcessingMS   *float64 `json:"avg_processing_ms"`
	Notes             []string `json:"notes"`
}

type AdGuardChecker struct {
	clientFactory func(AdGuardConfig) *http.Client
	slots         chan struct{}
}

func NewAdGuardChecker() *AdGuardChecker {
	return &AdGuardChecker{clientFactory: newAdGuardClient, slots: make(chan struct{}, 4)}
}

func (c *AdGuardChecker) Check(ctx context.Context, config AdGuardConfig) AdGuardResult {
	out := AdGuardResult{State: "unavailable", CheckedAt: time.Now().UnixMilli(), Notes: []string{
		"Read-only snapshot from the explicitly configured AdGuard Home server. Statistics cover its configured reporting window, not the controller's chart range.",
	}}
	if err := config.Normalize(); err != nil {
		out.Notes = append(out.Notes, "The saved connection settings are invalid; review the HTTPS destination and credentials.")
		return out
	}
	out.SourceURL = config.URL
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	default:
		out.Notes = append(out.Notes, "Other integration checks are still running; try again shortly.")
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 18*time.Second)
	defer cancel()
	client := c.clientFactory(config)
	defer client.CloseIdleConnections()
	var status struct {
		Version           string `json:"version"`
		Running           *bool  `json:"running"`
		ProtectionEnabled *bool  `json:"protection_enabled"`
	}
	if err := readAdGuardJSON(ctx, client, config, "/control/status", &status); err != nil {
		out.Notes = append(out.Notes, "Status could not be read. Check the HTTPS address, certificate, authentication, and network reachability.")
	} else {
		if len(status.Version) <= 128 {
			out.Version = status.Version
		}
		out.Running, out.ProtectionEnabled = status.Running, status.ProtectionEnabled
	}
	var stats struct {
		Queries    *int64   `json:"num_dns_queries"`
		Blocked    *int64   `json:"num_blocked_filtering"`
		Processing *float64 `json:"avg_processing_time"`
	}
	if err := readAdGuardJSON(ctx, client, config, "/control/stats", &stats); err != nil {
		out.Notes = append(out.Notes, "Statistics could not be read. Check AdGuard Home's statistics settings and API access; missing figures are not zero.")
	} else {
		if stats.Queries != nil && *stats.Queries >= 0 && *stats.Queries <= 1<<53-1 {
			out.DNSQueries = stats.Queries
		}
		if stats.Blocked != nil && *stats.Blocked >= 0 && *stats.Blocked <= 1<<53-1 && (out.DNSQueries == nil || *stats.Blocked <= *out.DNSQueries) {
			out.BlockedFiltering = stats.Blocked
		}
		if stats.Processing != nil && *stats.Processing >= 0 && !math.IsNaN(*stats.Processing) && !math.IsInf(*stats.Processing, 0) && *stats.Processing < 86400 {
			ms := *stats.Processing * 1000
			out.AvgProcessingMS = &ms
		}
	}
	if out.Running != nil || out.ProtectionEnabled != nil || out.DNSQueries != nil || out.BlockedFiltering != nil || out.AvgProcessingMS != nil || out.Version != "" {
		out.State = "partial"
	}
	if out.Running != nil && out.ProtectionEnabled != nil && out.DNSQueries != nil && out.BlockedFiltering != nil && out.AvgProcessingMS != nil && out.Version != "" {
		out.State = "observed"
	}
	if out.State != "observed" {
		out.Notes = append(out.Notes, "Some expected fields were not observed; unavailable values remain unknown.")
	}
	return out
}

func readAdGuardJSON(ctx context.Context, client *http.Client, config AdGuardConfig, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, config.URL+path, nil)
	if err != nil {
		return errors.New("invalid integration request")
	}
	req.Header.Set("Accept", "application/json")
	if config.Username != "" {
		req.SetBasicAuth(config.Username, config.Password)
	}
	response, err := client.Do(req)
	if err != nil {
		return errors.New("integration request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("integration response was not successful")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil || len(body) > 2<<20 {
		return errors.New("integration response exceeded its limit or could not be read")
	}
	if json.Unmarshal(body, out) != nil {
		return errors.New("integration response did not contain usable JSON")
	}
	return nil
}
