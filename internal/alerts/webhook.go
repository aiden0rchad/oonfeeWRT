package alerts

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ValidateWebhookURL does not resolve DNS or contact a destination. Secrets in
// the path/query are supported only because the complete URL is encrypted.
func ValidateWebhookURL(raw string) (*url.URL, error) {
	if len(raw) > 4096 {
		return nil, errors.New("webhook URL is too long")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("webhook URL must be an absolute HTTPS URL without user information or a fragment")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("webhook port is invalid")
		}
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if strings.ContainsAny(host, "%\\ \t\r\n") || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return nil, errors.New("webhooks require a public HTTPS destination")
	}
	if ip, err := netip.ParseAddr(host); err == nil && !publicAddress(ip) {
		return nil, errors.New("webhooks cannot target private, local, or reserved addresses")
	}
	return u, nil
}

var blockedNetworks = []netip.Prefix{
	netip.MustParsePrefix("168.63.129.16/32"),
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"), netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
}

func publicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range blockedNetworks {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

type resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

func publicAddresses(ctx context.Context, lookup resolver, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		if !publicAddress(ip) {
			return nil, errors.New("destination address is not public")
		}
		return []netip.Addr{ip.Unmap()}, nil
	}
	ips, err := lookup.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 || len(ips) > 64 {
		return nil, errors.New("webhook DNS resolution failed")
	}
	for _, ip := range ips {
		if !publicAddress(ip) {
			return nil, errors.New("webhook DNS includes a non-public address")
		}
	}
	return ips, nil
}

// Share the connection budget across every validated answer so an unreachable
// IPv6 address cannot prevent a reachable IPv4 address from being attempted.
func dialValidated(ctx context.Context, ips []netip.Addr, port string, dial func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	for i, ip := range ips {
		if ctx.Err() != nil {
			break
		}
		attempt, done := context.WithTimeout(ctx, time.Until(deadline)/time.Duration(len(ips)-i))
		connection, err := dial(attempt, "tcp", net.JoinHostPort(ip.Unmap().String(), port))
		done()
		if err == nil {
			return connection, nil
		}
	}
	return nil, errors.New("webhook connection failed")
}

// SendWebhook pins the actual connection to validated DNS answers. No proxy,
// redirect, secondary resolution, router-local address, or insecure TLS path
// can bypass the public-destination boundary.
func SendWebhook(ctx context.Context, rawURL, token string, notification Notification) error {
	u, err := ValidateWebhookURL(rawURL)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := publicAddresses(ctx, net.DefaultResolver, u.Hostname())
	if err != nil {
		return err
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	dialer := net.Dialer{Timeout: 4 * time.Second}
	transport := &http.Transport{
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, ServerName: u.Hostname()},
		TLSHandshakeTimeout:    4 * time.Second,
		ResponseHeaderTimeout:  4 * time.Second,
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: 16 * 1024,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialValidated(ctx, ips, port, dialer.DialContext)
		},
	}
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	body, err := json.Marshal(notification)
	if err != nil {
		return errors.New("notification could not be encoded")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return errors.New("notification request could not be created")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "oonfeeWRT-alerts/1")
	request.Header.Set("X-OonfeeWRT-Event-ID", notification.EventID)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("webhook connection failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("webhook returned a non-success status")
	}
	return nil
}
