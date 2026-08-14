// Package discovery finds candidate OpenWrt devices on the local network.
//
// Discovery is a convenience layer and adoption must never depend on it
// (ARCHITECTURE §1). A controller on a bridged container network cannot see the
// LAN's layer 2 at all, and on Docker Desktop it cannot see the LAN. Add-by-
// address therefore stays a first-class path, and everything here is best
// effort: it reports what it declined to look at as carefully as what it found,
// because "no devices found" and "I never looked at the subnet your device is
// on" are wildly different answers that look identical in an empty list.
//
// # The fingerprint, and why it is not the one that was specified
//
// ARCHITECTURE §6 specifies probing for "a /ubus endpoint that answers
// session.login with an auth failure — that response alone proves it's OpenWrt
// rpcd, without logging in".
//
// That probe is wrong, and measurably so. On a stock OpenWrt device with no
// root password, session.login does not fail: rpcd looks the account up in
// /etc/shadow, an empty entry matches every password, and the call returns a
// full root session. Measured on the reference device 2026-08-14 — a login as
// root with the password "definitely-not-the-password-9f3a" returned status 0,
// a session token, and an ACL set including uci write and file exec.
//
// So the documented probe would silently mint a root session on every
// passwordless device in the subnet, on every scan, and leave each one to
// expire on its own five minutes later. A discovery sweep is the last place
// that should be attempting credentials against hosts nobody has yet chosen to
// manage.
//
// The probe used instead is ubus `list`, which stock OpenWrt answers on the
// null session with no credential at all (measured the same day: 13,113 bytes
// enumerating every registered object). It needs no password guess, creates no
// session, writes no failed-login record, and cannot lock anyone out. It is
// also a far stronger fingerprint than a login failure, because it returns the
// object graph — session.login, uci.apply, system.board — which nothing but
// rpcd publishes.
//
// It reveals nothing an unauthenticated caller could not already ask for
// directly; the ubus endpoint answers this to anyone who can reach the port,
// whether or not this controller exists.
package discovery

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Tunables. All are LAN assumptions: a device that cannot complete a TCP
// handshake in a second on the same broadcast domain is not a device we could
// poll every ten seconds anyway.
const (
	// DefaultDialTimeout bounds the TCP connect. One SYN, no retransmit — on a
	// LAN a host that does not answer the first one is firewalled or absent,
	// and either way it cannot be adopted.
	DefaultDialTimeout = 1200 * time.Millisecond

	// DefaultProbeTimeout bounds the HTTP exchange once connected.
	DefaultProbeTimeout = 3 * time.Second

	// DefaultWorkers bounds concurrency.
	//
	// A sweep's wall time is set almost entirely by the addresses where nothing
	// is listening: a host that exists answers in under 5 ms on a LAN, and one
	// that does not costs the full dial timeout. So the time is
	// (addresses / workers) x DialTimeout, and measured on two /24s that came
	// out at 508 / 64 x 1.2 s = 9.6 s of an operator watching a spinner.
	// 128 halves it and is still a trivial number of concurrent TCP connects
	// for a local network.
	DefaultWorkers = 128

	// MaxHosts caps a whole sweep regardless of how many networks were asked
	// for. A guard against sweeping something enormous by accident, not a
	// tuning knob.
	MaxHosts = 4096

	// maxProbeBody bounds what one host can make us read. Every host in a
	// sweep is untrusted by definition — that is what makes it discovery — so
	// the response cannot be read into memory unbounded.
	maxProbeBody = 512 << 10
)

// Verdict is three-state on purpose, and the middle value is the one that
// matters: a host that answered without an rpcd fingerprint has not been shown
// to be "not OpenWrt". It may be OpenWrt without uhttpd-mod-ubus installed —
// which we could not manage either way, so the operational answer is the same,
// but the reported reason must not claim more than was observed.
type Verdict string

const (
	// VerdictOpenWrt means the rpcd object graph was returned.
	VerdictOpenWrt Verdict = "openwrt"
	// VerdictReachable means something answered the port and did not present a
	// usable /ubus endpoint.
	VerdictReachable Verdict = "reachable"
	// VerdictSilent means nothing completed a TCP handshake.
	VerdictSilent Verdict = "silent"
)

// Signals are what the pre-auth object list says about a device.
//
// Deliberately thin. ARCHITECTURE §6 step 1 expects a pending device to show
// "model, MAC, firmware, IP — all read from system.board / system.info pre-auth
// where possible". Measured 2026-08-14: not possible. Stock rpcd answers
// system.board on the null session with JSON-RPC -32002, Access denied. Nothing
// here invents a model from the object list; the UI says the model is unknown
// until a credential is supplied, which is the truth.
type Signals struct {
	// Objects is how many ubus objects the device published.
	Objects int `json:"objects"`
	// Radios counts distinct hostapd PHYs with a running BSS. It is what is
	// configured and up, not what silicon exists — a radio with no SSID
	// publishes no hostapd object and is invisible here.
	Radios int `json:"radios"`
	// Gateway is set when a wan interface exists.
	Gateway bool `json:"gateway"`
	// DHCP is set when a DHCP server is running.
	DHCP bool `json:"dhcp"`
	// Wireless is set when iwinfo is present, which is the wireless stack
	// whether or not any AP is currently up.
	Wireless bool `json:"wireless"`
}

// Candidate is one probed address.
type Candidate struct {
	Host    string  `json:"host"`
	Port    int     `json:"port"`
	Scheme  string  `json:"scheme"`
	Verdict Verdict `json:"verdict"`
	Signals Signals `json:"signals"`
	// Note explains a non-OpenWrt verdict in the terms actually observed.
	Note string `json:"note,omitempty"`
}

// Result is one sweep.
type Result struct {
	Found []Candidate `json:"found"`
	// Swept, Answered and Elapsed exist so an empty Found is legible. "Swept
	// 254 addresses, 9 answered, none published a ubus endpoint" is a result;
	// an empty list on its own is indistinguishable from a broken scanner.
	Swept    int      `json:"swept"`
	Answered int      `json:"answered"`
	Networks []string `json:"networks"`
	// Skipped names what was NOT swept and why. This is the field that stops a
	// silent gap from reading as an absence of devices.
	Skipped   []string `json:"skipped,omitempty"`
	ElapsedMS int64    `json:"elapsed_ms"`
}

// Options configure a sweep.
type Options struct {
	// Networks to sweep, as CIDR. Empty means "whatever LocalNetworks finds".
	Networks []string
	// Ports to probe on each host. Empty means {80}.
	Ports []int
	// HTTPS additionally probes 443 over TLS. Off by default: it doubles the
	// sweep, and a device serving only HTTPS is rare enough that asking for it
	// is better than paying for it every time.
	HTTPS bool

	Workers      int
	DialTimeout  time.Duration
	ProbeTimeout time.Duration
}

func (o Options) workers() int {
	if o.Workers > 0 {
		return o.Workers
	}
	return DefaultWorkers
}

func (o Options) dialTimeout() time.Duration {
	if o.DialTimeout > 0 {
		return o.DialTimeout
	}
	return DefaultDialTimeout
}

func (o Options) probeTimeout() time.Duration {
	if o.ProbeTimeout > 0 {
		return o.ProbeTimeout
	}
	return DefaultProbeTimeout
}

// listRequest is the probe body: ubus `list` for every object, on no session.
var listRequest = []byte(`{"jsonrpc":"2.0","id":1,"method":"list","params":["*"]}`)

// RequestBody is what one probe sends, exported so the caller can attribute the
// cost to a managed device's overhead readout rather than leaving it uncounted.
func RequestBody() []byte { return listRequest }

// Sweep probes every address in the requested networks and returns what
// answered.
func Sweep(ctx context.Context, opt Options) (*Result, error) {
	start := time.Now()

	nets, skipped, err := resolveNetworks(opt.Networks)
	if err != nil {
		return nil, err
	}
	if len(nets) == 0 {
		return &Result{
			Skipped:   skipped,
			ElapsedMS: time.Since(start).Milliseconds(),
		}, nil
	}

	targets, dropped := expand(nets)
	skipped = append(skipped, dropped...)

	ports := opt.Ports
	if len(ports) == 0 {
		ports = []int{80}
	}

	type job struct {
		host   string
		port   int
		scheme string
	}
	jobs := make(chan job)
	var (
		mu       sync.Mutex
		found    []Candidate
		answered int
	)

	var wg sync.WaitGroup
	for i := 0; i < opt.workers(); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				c := probe(ctx, j.host, j.port, j.scheme, opt)
				mu.Lock()
				if c.Verdict != VerdictSilent {
					answered++
				}
				if c.Verdict == VerdictOpenWrt {
					found = append(found, c)
				}
				mu.Unlock()
			}
		}()
	}

	swept := 0
feed:
	for _, host := range targets {
		for _, p := range ports {
			select {
			case jobs <- job{host: host, port: p, scheme: "http"}:
				swept++
			case <-ctx.Done():
				break feed
			}
		}
		if opt.HTTPS {
			select {
			case jobs <- job{host: host, port: 443, scheme: "https"}:
				swept++
			case <-ctx.Done():
				break feed
			}
		}
	}
	close(jobs)
	wg.Wait()

	sortCandidates(found)
	names := make([]string, 0, len(nets))
	for _, n := range nets {
		names = append(names, n.String())
	}
	return &Result{
		Found:     found,
		Swept:     swept,
		Answered:  answered,
		Networks:  names,
		Skipped:   skipped,
		ElapsedMS: time.Since(start).Milliseconds(),
	}, ctx.Err()
}

// Probe asks one address whether it is a manageable OpenWrt device.
//
// Exported because add-by-address wants exactly this check before an operator
// types a password into a form: "that address is not running a ubus endpoint"
// is a much better answer at the top of the form than an authentication error
// at the bottom of it.
func Probe(ctx context.Context, host string, port int, scheme string, opt Options) Candidate {
	return probe(ctx, host, port, scheme, opt)
}

func probe(ctx context.Context, host string, port int, scheme string, opt Options) Candidate {
	c := Candidate{Host: host, Port: port, Scheme: scheme, Verdict: VerdictSilent}

	// Dial first, separately. An empty /24 costs 254 short TCP connects instead
	// of 254 HTTP requests, and the connect result is what separates "silent"
	// from "answered but not ours".
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	dialer := net.Dialer{Timeout: opt.dialTimeout()}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return c
	}
	conn.Close()

	c.Verdict = VerdictReachable

	sig, err := listObjects(ctx, host, port, scheme, opt.probeTimeout())
	if err != nil {
		c.Note = ubusNote(err)
		return c
	}
	c.Verdict = VerdictOpenWrt
	c.Signals = sig
	return c
}

// ubusNote turns a probe failure into something an operator can act on, without
// claiming the host is not OpenWrt — which the probe cannot establish.
func ubusNote(err error) string {
	var he *httpStatusError
	if errors.As(err, &he) {
		if he.code == http.StatusNotFound {
			return "answered on this port but has no /ubus endpoint " +
				"(uhttpd-mod-ubus is not installed, or is not enabled)"
		}
		return fmt.Sprintf("answered on this port but /ubus returned HTTP %d", he.code)
	}
	return "answered on this port but did not publish a ubus object list"
}

type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTP %d", e.code) }

// listObjects performs the fingerprint call and reads the signals out of it.
func listObjects(ctx context.Context, host string, port int, scheme string, timeout time.Duration) (Signals, error) {
	var sig Signals

	tr := &http.Transport{
		DisableKeepAlives: true,
		ForceAttemptHTTP2: false,
	}
	if scheme == "https" {
		// Nothing is trusted at discovery time and nothing is being sent, so
		// there is nothing for a certificate to protect: the request carries no
		// credential and the response is a public object list. The pin that
		// matters is taken at adoption, on first use, and enforced from then on.
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // see comment
	}
	cl := &http.Client{
		Transport: tr,
		Timeout:   timeout,
		// A probe that follows redirects can be pointed at an arbitrary host by
		// the host being probed. Refuse: the answer we want is from the address
		// we asked.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer cl.CloseIdleConnections()

	url := fmt.Sprintf("%s://%s/ubus", scheme, net.JoinHostPort(host, strconv.Itoa(port)))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(listRequest))
	if err != nil {
		return sig, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := cl.Do(req)
	if err != nil {
		return sig, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return sig, &httpStatusError{code: resp.StatusCode}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBody))
	if err != nil {
		return sig, err
	}

	var out struct {
		Result map[string]map[string]json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return sig, err
	}
	if !isRPCD(out.Result) {
		return sig, errors.New("discovery: object list is not rpcd's")
	}
	return signalsFrom(out.Result), nil
}

// isRPCD decides whether an object list is OpenWrt's.
//
// Three independent objects rather than one, because the point of the
// fingerprint is to be sure before an address is offered to an operator as a
// device to type their router password into. session.login alone is a
// plausible name for something else to publish; session.login AND a uci
// config store AND system.board together are not.
func isRPCD(objs map[string]map[string]json.RawMessage) bool {
	sess, ok := objs["session"]
	if !ok {
		return false
	}
	if _, ok := sess["login"]; !ok {
		return false
	}
	if _, ok := objs["uci"]; !ok {
		return false
	}
	sys, ok := objs["system"]
	if !ok {
		return false
	}
	_, ok = sys["board"]
	return ok
}

func signalsFrom(objs map[string]map[string]json.RawMessage) Signals {
	sig := Signals{Objects: len(objs)}
	phys := map[string]bool{}
	for name := range objs {
		switch {
		case strings.HasPrefix(name, "hostapd."):
			// hostapd.phy0-ap0 is one BSS, not one radio. Count distinct PHYs
			// so a device with two SSIDs on one radio is not reported as two.
			iface := strings.TrimPrefix(name, "hostapd.")
			if phy, _, found := strings.Cut(iface, "-"); found {
				phys[phy] = true
			}
		case name == "iwinfo":
			sig.Wireless = true
		case name == "dnsmasq" || strings.HasPrefix(name, "dnsmasq.") || name == "dhcp":
			sig.DHCP = true
		case strings.HasPrefix(name, "network.interface."):
			if n := strings.TrimPrefix(name, "network.interface."); n == "wan" || n == "wan6" {
				sig.Gateway = true
			}
		}
	}
	sig.Radios = len(phys)
	return sig
}

// ---- target expansion ----

// resolveNetworks turns the requested CIDRs — or the host's own interfaces —
// into networks that are safe and sensible to sweep, plus a human-readable
// account of everything that was left out.
func resolveNetworks(want []string) (nets []*net.IPNet, skipped []string, err error) {
	if len(want) == 0 {
		return LocalNetworks()
	}
	for _, s := range want {
		_, n, perr := net.ParseCIDR(strings.TrimSpace(s))
		if perr != nil {
			return nil, nil, fmt.Errorf("discovery: %q is not a network in CIDR form: %w", s, perr)
		}
		if n.IP.To4() == nil {
			skipped = append(skipped, fmt.Sprintf("%s — IPv6 is not swept; "+
				"the smallest normal IPv6 subnet holds more addresses than "+
				"could be probed in a human lifetime. Add IPv6 devices by address", s))
			continue
		}
		ones, _ := n.Mask.Size()
		if ones < MinPrefix {
			skipped = append(skipped, fmt.Sprintf("%s — larger than a /%d (%d addresses); "+
				"refused so a typo cannot turn into a scan of the internet",
				s, MinPrefix, 1<<(32-ones)))
			continue
		}
		nets = append(nets, n)
	}
	return nets, skipped, nil
}

// expand lists every probeable host address, dropping whole networks that would
// take the sweep past MaxHosts rather than silently truncating one.
func expand(nets []*net.IPNet) (hosts []string, skipped []string) {
	total := 0
	for _, n := range nets {
		addrs := hostsIn(n)
		if total+len(addrs) > MaxHosts {
			skipped = append(skipped, fmt.Sprintf("%s — %d addresses would take this "+
				"sweep past the %d-address limit; sweep it on its own",
				n.String(), len(addrs), MaxHosts))
			continue
		}
		total += len(addrs)
		hosts = append(hosts, addrs...)
	}
	return hosts, skipped
}

// hostsIn enumerates the usable addresses of an IPv4 network.
func hostsIn(n *net.IPNet) []string {
	ip4 := n.IP.To4()
	if ip4 == nil {
		return nil
	}
	ones, bits := n.Mask.Size()
	if bits != 32 {
		return nil
	}
	count := 1 << (32 - ones)
	base := binaryIP(ip4)

	// /31 and /32 have no network or broadcast address to skip: a /32 is one
	// host and a /31 is a point-to-point pair.
	lo, hi := 0, count
	if ones < 31 {
		lo, hi = 1, count-1
	}
	out := make([]string, 0, hi-lo)
	for i := lo; i < hi; i++ {
		out = append(out, ipString(base+uint32(i)))
	}
	return out
}

func binaryIP(ip net.IP) uint32 {
	ip = ip.To4()
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func ipString(v uint32) string {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v)).String()
}

func sortCandidates(c []Candidate) {
	sort.Slice(c, func(i, j int) bool {
		a, b := net.ParseIP(c[i].Host), net.ParseIP(c[j].Host)
		if a != nil && b != nil {
			ai, bi := binaryIP(a), binaryIP(b)
			if ai != bi {
				return ai < bi
			}
		}
		return c[i].Port < c[j].Port
	})
}
