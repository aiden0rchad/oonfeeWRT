package daemon

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// hostResolver is the small part of net.Resolver used by device workflows. It
// is an interface so the one-resolution guarantee can be tested without
// changing the process resolver or relying on external DNS.
type hostResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

// workflowEndpoint pins one operator-entered host to one address for a complete
// inspect or adoption. Adoption crosses HTTP, SSH, and a second HTTP session;
// resolving independently at each boundary can identify one router, install the
// controller login on another, and persist a record that belongs to neither.
type workflowEndpoint struct {
	original string
	ip       netip.Addr
	port     string // optional port embedded in original, retained for compatibility
	named    bool
}

func (d *Daemon) resolveWorkflowEndpoint(ctx context.Context, raw string) (workflowEndpoint, error) {
	resolver := d.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return resolveWorkflowEndpoint(ctx, resolver, raw)
}

func resolveWorkflowEndpoint(ctx context.Context, resolver hostResolver, raw string) (workflowEndpoint, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return workflowEndpoint{}, fmt.Errorf("device host is empty")
	}

	host, port := raw, ""
	if addr, err := netip.ParseAddr(raw); err == nil {
		return workflowEndpoint{original: raw, ip: addr.Unmap()}, nil
	}
	// A bracketed IPv6 address without a port is valid input but SplitHostPort
	// quite correctly rejects it. Strip only a complete bracket pair; malformed
	// bracket/port combinations continue to the strict error below.
	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		if addr, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(raw, "["), "]")); err == nil {
			return workflowEndpoint{original: raw, ip: addr.Unmap()}, nil
		}
	}
	if strings.Contains(raw, ":") {
		var err error
		host, port, err = net.SplitHostPort(raw)
		if err != nil {
			return workflowEndpoint{}, fmt.Errorf("device host %q has an invalid port; use brackets around an IPv6 address: %w", raw, err)
		}
		if err := validateEmbeddedPort(port); err != nil {
			return workflowEndpoint{}, fmt.Errorf("device host %q: %w", raw, err)
		}
		if addr, err := netip.ParseAddr(host); err == nil {
			return workflowEndpoint{original: raw, ip: addr.Unmap(), port: port}, nil
		}
	}

	if strings.ContainsAny(host, "/\\[] 	\r\n") {
		return workflowEndpoint{}, fmt.Errorf("device hostname %q is invalid", host)
	}
	ips, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return workflowEndpoint{}, fmt.Errorf("resolve device hostname %q: %w", host, err)
	}
	for _, ip := range ips {
		if ip.IsValid() {
			return workflowEndpoint{
				original: raw,
				ip:       ip.Unmap(),
				port:     port,
				named:    true,
			}, nil
		}
	}
	return workflowEndpoint{}, fmt.Errorf("resolve device hostname %q: no IP address returned", host)
}

func validateEmbeddedPort(port string) error {
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("port %q is out of range", port)
	}
	return nil
}

// httpAuthority always includes an explicit port. Besides making the endpoint
// chosen above unambiguous, this brackets IPv6 correctly before ubus builds its
// URL. A port embedded in Host retains the pre-existing test and advanced-use
// convention; supplying both forms is rejected instead of producing a nested,
// unusable authority.
func (e workflowEndpoint) httpAuthority(port int, https bool) (string, error) {
	if e.port != "" {
		if port != 0 {
			return "", fmt.Errorf("device host already includes port %s; do not also set the port field", e.port)
		}
		return net.JoinHostPort(e.ip.String(), e.port), nil
	}
	if port < 0 || port > 65535 {
		return "", fmt.Errorf("device port %d is out of range", port)
	}
	if port == 0 {
		port = 80
		if https {
			port = 443
		}
	}
	return net.JoinHostPort(e.ip.String(), strconv.Itoa(port)), nil
}

func (e workflowEndpoint) sshAddress() string {
	if e.port != "" {
		return net.JoinHostPort(e.ip.String(), e.port)
	}
	return e.ip.String()
}

// inventoryHost keeps a hostname only when later connections have an identity
// pin. HTTPS adoption records the exact certificate observed during this pinned
// workflow, so future DNS movement cannot silently select another endpoint.
// Plain HTTP has no such peer identity; persist the resolved address instead.
func (e workflowEndpoint) inventoryHost(preservePinnedName bool) string {
	if e.named && preservePinnedName {
		return e.original
	}
	if e.port != "" {
		return net.JoinHostPort(e.ip.String(), e.port)
	}
	return e.ip.String()
}

func effectiveDevicePort(port int, https bool) int {
	if port != 0 {
		return port
	}
	if https {
		return 443
	}
	return 80
}
