package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// The live channel, IMPLEMENTATION §8.
//
// Two things it is for. It removes the polling latency from the UI — a screen
// that refreshes every ten seconds shows data up to ten seconds old, which
// defeats the point of a focused tier that polls every five. And it is where
// focus belongs: §7 specifies focus as reference-counted by this hub,
// acquired on subscribe and released on unsubscribe or disconnect.
//
// That second point is the interesting one. Before this existed, focus was a
// timed lease the browser had to renew, because a closed tab never runs cleanup
// and an un-renewed lease was the only way to notice. A WebSocket has a real
// lifetime: the connection closing IS the signal, so the lease machinery goes
// away and focus becomes exact rather than approximate.

const (
	// sendBuffer bounds what one slow client can make the server hold.
	// DEVICE-BUDGET's shape rule — never unbounded queues — applies to the
	// controller's own memory too.
	sendBuffer = 32

	// pingInterval keeps intermediaries from dropping an idle connection and
	// detects a peer that has gone away without closing.
	pingInterval = 30 * time.Second

	// writeTimeout bounds a single frame write. A client that has stopped
	// reading must not pin a goroutine.
	writeTimeout = 10 * time.Second
)

// Hub fans device updates out to subscribed browsers.
type Hub struct {
	log   *slog.Logger
	fleet Fleet

	mu    sync.Mutex
	conns map[*liveConn]struct{}
}

// NewHub builds a hub. fleet may be nil, in which case subscribing does not
// raise the poll rate — the stream still works, it is just not focused.
func NewHub(fleet Fleet, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{log: log, fleet: fleet, conns: map[*liveConn]struct{}{}}
}

// liveConn is one browser.
type liveConn struct {
	hub  *Hub
	ws   *websocket.Conn
	send chan []byte

	mu sync.Mutex
	// subs maps a subscribed device to the function that releases its focus.
	subs map[int64]func()

	dropped atomic.Int64
	closed  atomic.Bool
}

type wsMessage struct {
	Type     string `json:"type"`
	Topic    string `json:"topic,omitempty"`
	DeviceID int64  `json:"device_id,omitempty"`
}

// handleLive upgrades an authenticated request to a WebSocket.
//
// The Origin check is not optional here and is not covered by the CSRF token.
// The same-origin policy does not apply to WebSockets: any page on any origin
// can open one to this server, and the browser will attach the session cookie.
// That is cross-site WebSocket hijacking, and the only thing standing in front
// of it is refusing a handshake whose Origin is not ours.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	if s.Hub == nil {
		writeErr(w, http.StatusServiceUnavailable, "the live channel is not available")
		return
	}
	if !sameOrigin(r) {
		writeErr(w, http.StatusForbidden,
			"cross-origin WebSocket connections are not accepted")
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Belt and braces: the library performs its own Origin check against
		// Host, and sameOrigin above has already run.
		OriginPatterns: []string{r.Host},
	})
	if err != nil {
		s.Log.Debug("websocket upgrade failed", "err", err)
		return
	}
	s.Hub.serve(r.Context(), ws)
}

func (h *Hub) serve(ctx context.Context, ws *websocket.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	c := &liveConn{
		hub:  h,
		ws:   ws,
		send: make(chan []byte, sendBuffer),
		subs: map[int64]func(){},
	}
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.conns, c)
		h.mu.Unlock()
		// Releasing every focus this connection held is the whole reason focus
		// lives here: a closed tab releases it exactly, with no timer and no
		// grace period to get wrong.
		c.releaseAll()
		c.closed.Store(true)
		if n := c.dropped.Load(); n > 0 {
			h.log.Info("live client could not keep up; frames were dropped",
				"dropped", n)
		}
		ws.CloseNow()
	}()

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		c.writeLoop(ctx)
		// A write timeout or failed ping means the connection is dead even when
		// the peer never sends another frame. Wake the reader so it cannot keep
		// the connection and its focused poll subscriptions alive indefinitely.
		cancel()
	}()
	c.readLoop(ctx)
	cancel()
	<-writerDone
}

// readLoop handles subscriptions until the peer goes away.
func (c *liveConn) readLoop(ctx context.Context) {
	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			return // disconnect, of any kind
		}
		if typ != websocket.MessageText {
			continue
		}
		var m wsMessage
		if err := json.Unmarshal(data, &m); err != nil {
			c.push(map[string]any{"type": "error", "error": "malformed message"})
			continue
		}
		switch m.Type {
		case "subscribe":
			c.subscribe(m)
		case "unsubscribe":
			c.unsubscribe(m)
		case "ping":
			c.push(map[string]any{"type": "pong"})
		default:
			c.push(map[string]any{"type": "error", "error": "unknown message type"})
		}
	}
}

func (c *liveConn) subscribe(m wsMessage) {
	switch m.Topic {
	case "device.stats":
		if m.DeviceID <= 0 {
			c.push(map[string]any{"type": "error", "error": "device_id is required"})
			return
		}
		c.mu.Lock()
		if _, already := c.subs[m.DeviceID]; already {
			c.mu.Unlock()
			return // idempotent: re-subscribing must not stack focus
		}
		release := func() {}
		if c.hub.fleet != nil {
			release = c.hub.fleet.Focus(m.DeviceID)
		}
		c.subs[m.DeviceID] = release
		c.mu.Unlock()
		c.push(map[string]any{"type": "subscribed", "topic": m.Topic,
			"device_id": m.DeviceID})
	default:
		c.push(map[string]any{"type": "error", "error": "unknown topic"})
	}
}

func (c *liveConn) unsubscribe(m wsMessage) {
	switch m.Topic {
	case "device.stats":
		c.mu.Lock()
		release := c.subs[m.DeviceID]
		delete(c.subs, m.DeviceID)
		c.mu.Unlock()
		if release != nil {
			release()
		}
	}
}

func (c *liveConn) releaseAll() {
	c.mu.Lock()
	subs := c.subs
	c.subs = map[int64]func(){}
	c.mu.Unlock()
	for _, release := range subs {
		release()
	}
}

// writeLoop is the only writer. A WebSocket connection cannot be written from
// two goroutines, so every frame goes through here.
func (c *liveConn) writeLoop(ctx context.Context) {
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-c.send:
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.ws.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				return
			}
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.ws.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// push queues a message, dropping it if the client is not keeping up.
//
// Dropping rather than blocking or growing is the rule: this is live state, so
// a frame the client never saw is superseded by the next one within a poll
// interval. Blocking would let one stalled browser stall the poll loop that
// feeds it, and an unbounded queue would let it consume the controller.
func (c *liveConn) push(v any) {
	if c.closed.Load() {
		return
	}
	blob, err := json.Marshal(v)
	if err != nil {
		return
	}
	select {
	case c.send <- blob:
	default:
		c.dropped.Add(1)
	}
}

// Publish sends a device update to whoever asked for that device.
func (h *Hub) Publish(deviceID int64, msg any) {
	h.mu.Lock()
	conns := make([]*liveConn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()

	for _, c := range conns {
		c.mu.Lock()
		_, wants := c.subs[deviceID]
		c.mu.Unlock()
		if wants {
			c.push(msg)
		}
	}
}

// ForgetDevice removes subscriptions bound to a reusable inventory ID. A
// browser that was watching the removed router must not silently begin
// receiving a different router's snapshots if SQLite assigns it the same ID.
func (h *Hub) ForgetDevice(deviceID int64) {
	h.mu.Lock()
	conns := make([]*liveConn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()

	for _, c := range conns {
		c.mu.Lock()
		release := c.subs[deviceID]
		delete(c.subs, deviceID)
		c.mu.Unlock()
		if release != nil {
			release()
		}
	}
}

// Connections reports how many live clients are attached, for the overhead
// readout and for tests.
func (h *Hub) Connections() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}

// Close drops every connection, releasing the focus they hold.
func (h *Hub) Close() {
	h.mu.Lock()
	conns := make([]*liveConn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.conns = map[*liveConn]struct{}{}
	h.mu.Unlock()
	for _, c := range conns {
		c.releaseAll()
		c.closed.Store(true)
		c.ws.CloseNow()
	}
}
