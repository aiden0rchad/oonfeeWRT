package discovery

import (
	"fmt"
	"net"
	"strings"
)

// MinPrefix is the largest network a sweep will touch: a /22 is 1022 hosts,
// which finishes in a few seconds. Anything wider is refused rather than
// clamped, because clamping a /8 to its first 1022 addresses would sweep a
// range nobody asked about and report "nothing found" about the rest.
const MinPrefix = 22

// LocalNetworks reports the IPv4 networks this host is directly attached to,
// and — just as importantly — the ones it deliberately did not include.
//
// The exclusions are all cases where sweeping is either impossible or actively
// wrong, and each is reported rather than dropped, because the failure mode
// this function has is invisible: a controller that quietly declines to look at
// the interface the operator's router is on reports "no devices found", which
// reads as a statement about the network rather than about itself.
func LocalNetworks() (nets []*net.IPNet, skipped []string, err error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, nil, fmt.Errorf("discovery: list interfaces: %w", err)
	}
	// Group by reason rather than listing one line per interface. A laptop with
	// a VPN client has ten utun devices and repeating the same sentence ten
	// times buries the one skip that might actually matter.
	skips := newSkipList()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue // down: not worth a line of explanation
		}
		if ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		if ifc.Flags&net.FlagPointToPoint != 0 {
			// VPN and tunnel interfaces. The far end is a routed network, not a
			// broadcast domain, and its size is unrelated to the netmask the
			// tunnel presents — a WireGuard or Tailscale interface routinely
			// carries a /32. Sweeping one probes a remote network over a link
			// that was never meant for it.
			skips.add(ifc.Name, "point-to-point or tunnel interfaces; the far "+
				"side is routed rather than local. Add devices behind one by address")
			continue
		}
		addrs, aerr := ifc.Addrs()
		if aerr != nil {
			skips.add(ifc.Name, fmt.Sprintf("could not read the interface's addresses: %v", aerr))
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue // IPv6 and non-IP addresses: see below
			}
			ip4 := ipnet.IP.To4()
			if ip4.IsLinkLocalUnicast() {
				skips.add(fmt.Sprintf("%s (%s)", ifc.Name, ipnet.String()),
					"a link-local address, which means the interface never got a "+
						"DHCP lease")
				continue
			}
			ones, _ := ipnet.Mask.Size()
			if ones < MinPrefix {
				skips.add(fmt.Sprintf("%s (%s)", ifc.Name, ipnet.String()),
					fmt.Sprintf("wider than a /%d, so sweeping it would probe %d "+
						"addresses. Give a narrower range explicitly if the device "+
						"really is in there", MinPrefix, 1<<(32-ones)))
				continue
			}
			if ones >= 31 {
				continue // a /31 or /32 holds nothing to find
			}
			n := &net.IPNet{IP: ip4.Mask(ipnet.Mask), Mask: ipnet.Mask}
			if !containsNet(nets, n) {
				nets = append(nets, n)
			}
		}
	}
	// IPv6 is not swept at all, and that is a property of IPv6 rather than a
	// gap: the smallest subnet a device normally gets is a /64, which is four
	// billion times the entire IPv4 address space. Say so once, here, instead of
	// per interface.
	if hasIPv6(ifaces) {
		skips.add("", "IPv6 networks are never swept — a /64 holds 1.8e19 "+
			"addresses. IPv6-only devices have to be added by address")
	}
	skipped = skips.lines()
	if len(nets) == 0 && len(skipped) == 0 {
		skipped = append(skipped, "no directly attached IPv4 network was found. "+
			"On a bridged container this is expected — discovery cannot cross the "+
			"bridge, and adding devices by address is the supported path there")
	}
	return nets, skipped, nil
}

// skipList groups skipped interfaces by the reason they were skipped, keeping
// the order reasons were first seen.
type skipList struct {
	order []string
	byWhy map[string][]string
}

func newSkipList() *skipList {
	return &skipList{byWhy: map[string][]string{}}
}

func (s *skipList) add(what, why string) {
	if _, seen := s.byWhy[why]; !seen {
		s.order = append(s.order, why)
	}
	if what != "" {
		s.byWhy[why] = append(s.byWhy[why], what)
	} else if s.byWhy[why] == nil {
		s.byWhy[why] = []string{}
	}
}

func (s *skipList) lines() []string {
	out := make([]string, 0, len(s.order))
	for _, why := range s.order {
		if names := s.byWhy[why]; len(names) > 0 {
			out = append(out, strings.Join(names, ", ")+" — "+why)
		} else {
			out = append(out, why)
		}
	}
	return out
}

func hasIPv6(ifaces []net.Interface) bool {
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if ok && ipnet.IP.To4() == nil && ipnet.IP.IsGlobalUnicast() {
				return true
			}
		}
	}
	return false
}

func containsNet(nets []*net.IPNet, n *net.IPNet) bool {
	want := n.String()
	for _, e := range nets {
		if e.String() == want {
			return true
		}
	}
	return false
}

// NetworkStrings renders networks for the API.
func NetworkStrings(nets []*net.IPNet) []string {
	out := make([]string, 0, len(nets))
	for _, n := range nets {
		out = append(out, n.String())
	}
	return out
}

// HostCount reports how many addresses a sweep of these networks would probe,
// so the UI can say "this will probe 254 addresses" before doing it rather than
// after.
func HostCount(nets []*net.IPNet) int {
	total := 0
	for _, n := range nets {
		total += len(hostsIn(n))
	}
	return total
}

// ParseNetworks validates operator-supplied CIDRs without sweeping them.
func ParseNetworks(in []string) ([]*net.IPNet, []string, error) {
	cleaned := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			cleaned = append(cleaned, s)
		}
	}
	if len(cleaned) == 0 {
		return nil, nil, nil
	}
	return resolveNetworks(cleaned)
}
