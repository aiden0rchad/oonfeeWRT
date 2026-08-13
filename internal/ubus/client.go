package ubus

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// nullSession is the all-zero token rpcd accepts for session.login only.
const nullSession = "00000000000000000000000000000000"

// Measured on hardware (docs/IMPLEMENTATION.md §14), and the reason the
// defaults below are what they are:
//
//   - uhttpd drops an idle keep-alive connection at 20s. A 60s baseline poll
//     therefore never reuses a connection, and that is fine — reconnecting is
//     cheap over HTTP (~0.5ms) and ~15.8ms over TLS.
//   - the ubus session idle timer is 300s and ANY call refreshes it, so the
//     token comfortably outlives the socket. Never re-login just because the
//     connection dropped.
//   - a batch of 550 calls / 65KB was accepted with per-call cost flat from
//     ~10 calls up. Chunk on bytes, not on call count.
const (
	KeepAliveIdle  = 20 * time.Second
	SessionIdle    = 300 * time.Second
	maxBatchBytes  = 48 << 10 // conservative against the ~65KB observed ceiling
	defaultTimeout = 15 * time.Second
)

// Invocation is one ubus call inside a batch.
type Invocation struct {
	Object string
	Method string
	Args   any
}

// Result is one outcome from a batch, in request order.
type Result struct {
	Status Status
	Data   json.RawMessage
	Err    error
}

// Client is a connection to one device's /ubus endpoint.
//
// Safe for concurrent use. One Client owns one ubus session; use FreshSession
// when you need an independent one (see its doc — this is not optional in the
// apply path).
type Client struct {
	baseURL string
	http    *http.Client

	// pinnedCert is the SHA-256 of the DER-encoded leaf certificate. Devices
	// serve a self-signed cert (CN=OpenWrt), so chain validation is
	// meaningless; we pin on first use instead.
	pinnedCert string

	mu      sync.Mutex
	session string
	user    string
	pass    string

	// confirmWindow suppresses BOTH the session refresh and the transport
	// retry. uci.confirm is bound to the session that applied, so a re-login
	// inside the window guarantees the change reverts — a "retry" there is
	// worse than the error it is papering over.
	confirmWindow bool

	nextID int
}

// Options configure a Client.
type Options struct {
	Host       string // "192.168.1.1" or "192.168.1.1:8080"
	HTTPS      bool
	PinnedCert string        // hex SHA-256 of the DER leaf; empty = trust on first use
	Timeout    time.Duration // per-request; 0 uses defaultTimeout
}

// New builds a Client. It performs no I/O.
func New(opt Options) *Client {
	scheme := "http"
	if opt.HTTPS {
		scheme = "https"
	}
	timeout := opt.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	c := &Client{
		baseURL:    fmt.Sprintf("%s://%s/ubus", scheme, opt.Host),
		pinnedCert: opt.PinnedCert,
		session:    nullSession,
	}
	tr := &http.Transport{
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     KeepAliveIdle,
		ForceAttemptHTTP2:   false,
	}
	if opt.HTTPS {
		tr.TLSClientConfig = &tls.Config{
			// The device's certificate is self-signed with CN=OpenWrt, so the
			// standard chain check can only ever fail. Verify by pin instead.
			InsecureSkipVerify: true, //nolint:gosec // pinned in verifyPin
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
				return c.verifyPin(raw)
			},
		}
	}
	c.http = &http.Client{Transport: tr, Timeout: timeout}
	return c
}

func (c *Client) verifyPin(rawCerts [][]byte) error {
	if len(rawCerts) == 0 {
		return errors.New("ubus: device presented no certificate")
	}
	sum := sha256.Sum256(rawCerts[0])
	got := hex.EncodeToString(sum[:])

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pinnedCert == "" {
		// Trust on first use. Adoption records this; every later connection
		// must match it, and a mismatch is surfaced loudly rather than
		// silently re-pinned — a device that changes cert has been reflashed
		// or replaced, and the operator needs to say which.
		c.pinnedCert = got
		return nil
	}
	if got != c.pinnedCert {
		return fmt.Errorf("ubus: certificate fingerprint changed "+
			"(pinned %s, got %s) — device reflashed, replaced, or intercepted",
			c.pinnedCert[:16], got[:16])
	}
	return nil
}

// PinnedCert returns the fingerprint in use, for persisting at adoption.
func (c *Client) PinnedCert() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pinnedCert
}

// Session returns the current token. The apply engine needs this to prove that
// confirm goes out on the same session that applied.
func (c *Client) Session() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session
}

// BeginConfirmWindow suppresses session refresh and transport retry until the
// returned func is called. Hold it for the whole apply→confirm sequence.
func (c *Client) BeginConfirmWindow() (end func()) {
	c.mu.Lock()
	c.confirmWindow = true
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			c.confirmWindow = false
			c.mu.Unlock()
		})
	}
}

// FreshSession returns a second Client against the same device with its own
// independent ubus session, sharing the certificate pin.
//
// This is not a convenience. rpcd scopes staged UCI deltas to the session
// token, and a rollback restores /etc/config while leaving the applying
// session's delta in place — so the session that applied a change reads back
// the value it FAILED to set, indefinitely. Post-apply verification that does
// not use a fresh session reports every failed apply as a success. Closing the
// connection does not help; the token scopes the delta, not the socket.
func (c *Client) FreshSession(ctx context.Context) (*Client, error) {
	c.mu.Lock()
	opts := Options{
		Host:       hostFromURL(c.baseURL),
		HTTPS:      len(c.baseURL) > 5 && c.baseURL[:5] == "https",
		PinnedCert: c.pinnedCert,
		Timeout:    c.http.Timeout,
	}
	user, pass := c.user, c.pass
	c.mu.Unlock()

	other := New(opts)
	if err := other.Login(ctx, user, pass); err != nil {
		return nil, fmt.Errorf("fresh session: %w", err)
	}
	return other, nil
}

func hostFromURL(u string) string {
	s := u
	if i := len("https://"); len(s) > i && s[:i] == "https://" {
		s = s[i:]
	} else if i := len("http://"); len(s) > i && s[:i] == "http://" {
		s = s[i:]
	}
	if i := len(s) - len("/ubus"); i > 0 && s[i:] == "/ubus" {
		s = s[:i]
	}
	return s
}

// Login authenticates and stores the session token.
func (c *Client) Login(ctx context.Context, user, pass string) error {
	c.mu.Lock()
	c.session = nullSession
	c.user, c.pass = user, pass
	c.mu.Unlock()

	var out struct {
		Session string `json:"ubus_rpc_session"`
		Timeout int    `json:"timeout"`
	}
	err := c.call(ctx, "session", "login",
		map[string]string{"username": user, "password": pass}, &out, false)
	if err != nil {
		return err
	}
	if out.Session == "" {
		return errors.New("ubus: login returned no session token")
	}
	c.mu.Lock()
	c.session = out.Session
	c.mu.Unlock()
	return nil
}

// Close releases idle connections. It does NOT destroy the ubus session, which
// survives for SessionIdle regardless of the socket.
func (c *Client) Close() {
	c.http.CloseIdleConnections()
}

// Destroy ends the ubus session server-side, then closes. Use for short-lived
// verification sessions so tokens do not linger for their full 300s.
func (c *Client) Destroy(ctx context.Context) {
	_ = c.call(ctx, "session", "destroy", struct{}{}, nil, false)
	c.Close()
}

// Call performs one ubus invocation and decodes the payload into out.
//
// Retry policy, which is entirely dictated by measured device behaviour:
//
//   - ubus status != 0  -> *StatusError, never retried. The session is fine and
//     the target is refused; re-authenticating changes nothing.
//   - JSON-RPC -32002   -> ambiguous (dead session OR ungranted method). Re-login
//     ONCE and retry. Still failing -> *DeniedError{Retried: true}, a permanent
//     capability gap.
//   - inside a confirm window -> neither retry happens, because a token refresh
//     mid-window guarantees the device reverts.
func (c *Client) Call(ctx context.Context, object, method string, args, out any) error {
	return c.call(ctx, object, method, args, out, true)
}

func (c *Client) call(ctx context.Context, object, method string, args, out any, mayRetry bool) error {
	err := c.callOnce(ctx, object, method, args, out)

	var denied *DeniedError
	if !errors.As(err, &denied) {
		return err
	}

	c.mu.Lock()
	inWindow := c.confirmWindow
	user, pass := c.user, c.pass
	c.mu.Unlock()

	if !mayRetry || inWindow || user == "" {
		return err
	}

	// Exactly one re-login, then exactly one retry. If it fails again the ACL
	// simply does not grant this method, and looping would hammer the device
	// forever over a permanent authorization gap.
	if lerr := c.Login(ctx, user, pass); lerr != nil {
		return err // report the original denial, not the login failure
	}
	if err2 := c.callOnce(ctx, object, method, args, out); err2 != nil {
		var d2 *DeniedError
		if errors.As(err2, &d2) {
			d2.Retried = true
			return d2
		}
		return err2
	}
	return nil
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcResponse struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) callOnce(ctx context.Context, object, method string, args, out any) error {
	if args == nil {
		args = struct{}{}
	}
	c.mu.Lock()
	c.nextID++
	req := rpcRequest{JSONRPC: "2.0", ID: c.nextID, Method: "call",
		Params: []any{c.session, object, method, args}}
	c.mu.Unlock()

	var resp rpcResponse
	if err := c.post(ctx, req, &resp); err != nil {
		return err
	}
	status, data, err := decodeFrame(object, method, &resp)
	if err != nil {
		return err
	}
	if status != StatusOK {
		return &StatusError{Object: object, Method: method, Status: status}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("ubus %s.%s: decode payload: %w", object, method, err)
		}
	}
	return nil
}

// decodeFrame turns one JSON-RPC response into (status, payload).
//
// The whole denial-channel distinction lives here: an error object means rpcd
// refused to proxy at all, while a result array means it proxied and the object
// answered — including when the answer is "no".
func decodeFrame(object, method string, r *rpcResponse) (Status, json.RawMessage, error) {
	if r.Error != nil {
		if r.Error.Code == rpcErrAccessDenied {
			return 0, nil, &DeniedError{Object: object, Method: method}
		}
		return 0, nil, &ProtocolError{Code: r.Error.Code, Message: r.Error.Message}
	}
	var frame []json.RawMessage
	if err := json.Unmarshal(r.Result, &frame); err != nil || len(frame) == 0 {
		return 0, nil, &ProtocolError{Code: 0,
			Message: fmt.Sprintf("%s.%s: malformed result frame", object, method)}
	}
	var status Status
	if err := json.Unmarshal(frame[0], &status); err != nil {
		return 0, nil, &ProtocolError{Code: 0,
			Message: fmt.Sprintf("%s.%s: non-numeric status", object, method)}
	}
	if len(frame) > 1 {
		return status, frame[1], nil
	}
	return status, nil, nil
}

func (c *Client) post(ctx context.Context, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.postRaw(ctx, body, out)
}

func (c *Client) postRaw(ctx context.Context, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ubus transport: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("ubus transport: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return &ProtocolError{Code: resp.StatusCode,
			Message: fmt.Sprintf("HTTP %d: %.120s", resp.StatusCode, raw)}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &ProtocolError{Code: 0, Message: fmt.Sprintf("decode: %v", err)}
	}
	return nil
}
