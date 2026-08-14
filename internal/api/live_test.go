package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// liveHarness runs the API on a real listener, since a WebSocket needs one.
func liveHarness(t *testing.T) (*harness, string) {
	t.Helper()
	h := newHarness(t)
	h.setup()
	srv := httptest.NewServer(h.mux)
	t.Cleanup(srv.Close)
	return h, srv.URL
}

func dialLive(t *testing.T, h *harness, base string) *websocket.Conn {
	t.Helper()
	hdr := http.Header{}
	for _, c := range h.cookies {
		hdr.Add("Cookie", c.Name+"="+c.Value)
	}
	hdr.Set("Origin", strings.Replace(base, "http://", "http://", 1))
	ws, _, err := websocket.Dial(context.Background(),
		strings.Replace(base, "http://", "ws://", 1)+"/api/v1/live",
		&websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	return ws
}

func send(t *testing.T, ws *websocket.Conn, v any) {
	t.Helper()
	blob, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageText, blob); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func recv(t *testing.T, ws *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode %q: %v", data, err)
	}
	return m
}

// The same-origin policy does not apply to WebSockets: any page anywhere can
// open one and the browser attaches the session cookie. Refusing a foreign
// Origin is the only thing standing in front of cross-site WebSocket hijacking,
// and the CSRF token does not cover it — the upgrade is a GET.
func TestLiveRejectsCrossOriginHandshake(t *testing.T) {
	h, base := liveHarness(t)
	hdr := http.Header{}
	for _, c := range h.cookies {
		hdr.Add("Cookie", c.Name+"="+c.Value)
	}
	hdr.Set("Origin", "http://evil.example")
	_, _, err := websocket.Dial(context.Background(),
		strings.Replace(base, "http://", "ws://", 1)+"/api/v1/live",
		&websocket.DialOptions{HTTPHeader: hdr})
	if err == nil {
		t.Fatal("a cross-origin WebSocket handshake was accepted")
	}
}

func TestLiveRequiresASession(t *testing.T) {
	_, base := liveHarness(t)
	_, _, err := websocket.Dial(context.Background(),
		strings.Replace(base, "http://", "ws://", 1)+"/api/v1/live", nil)
	if err == nil {
		t.Fatal("an unauthenticated WebSocket was accepted")
	}
}

// Focus is reference-counted by the hub (IMPLEMENTATION §7): acquired on
// subscribe, released on unsubscribe. That is what lets the lease timer go
// away — a connection has a real lifetime, a lease only has a guess.
func TestSubscribeAcquiresFocusAndUnsubscribeReleasesIt(t *testing.T) {
	h, base := liveHarness(t)
	dev := h.seedDevice("ap-ws", true, nil)
	ws := dialLive(t, h, base)

	send(t, ws, map[string]any{"type": "subscribe", "topic": "device.stats",
		"device_id": dev.ID})
	if m := recv(t, ws); m["type"] != "subscribed" {
		t.Fatalf("got %v, want a subscribed ack", m)
	}
	if n := h.fleet.focusCount(dev.ID); n != 1 {
		t.Fatalf("focus count = %d after subscribe, want 1", n)
	}

	// Re-subscribing must be idempotent, or a reconnecting client stacks focus
	// it can never release.
	send(t, ws, map[string]any{"type": "subscribe", "topic": "device.stats",
		"device_id": dev.ID})
	time.Sleep(100 * time.Millisecond)
	if n := h.fleet.focusCount(dev.ID); n != 1 {
		t.Fatalf("focus count = %d after re-subscribing, want 1", n)
	}

	send(t, ws, map[string]any{"type": "unsubscribe", "topic": "device.stats",
		"device_id": dev.ID})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.fleet.focusCount(dev.ID) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("focus count = %d after unsubscribe, want 0", h.fleet.focusCount(dev.ID))
}

// A closed tab never runs cleanup. The connection closing IS the release, which
// is the whole reason focus lives in the hub rather than on a renewal timer.
func TestDisconnectReleasesFocus(t *testing.T) {
	h, base := liveHarness(t)
	dev := h.seedDevice("ap-wsd", true, nil)
	ws := dialLive(t, h, base)

	send(t, ws, map[string]any{"type": "subscribe", "topic": "device.stats",
		"device_id": dev.ID})
	recv(t, ws)
	if n := h.fleet.focusCount(dev.ID); n != 1 {
		t.Fatalf("focus count = %d, want 1", n)
	}

	ws.CloseNow()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.fleet.focusCount(dev.ID) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("focus count = %d after disconnect, want 0", h.fleet.focusCount(dev.ID))
}

func TestPublishReachesOnlySubscribers(t *testing.T) {
	h, base := liveHarness(t)
	a := h.seedDevice("ap-one-ws", true, nil)
	b := h.seedDevice("ap-two-ws-x", true, nil)
	ws := dialLive(t, h, base)

	send(t, ws, map[string]any{"type": "subscribe", "topic": "device.stats",
		"device_id": a.ID})
	recv(t, ws)

	// A device we did not subscribe to must not arrive.
	h.srv.Hub.Publish(b.ID, map[string]any{"type": "stats", "device_id": b.ID})
	h.srv.Hub.Publish(a.ID, map[string]any{"type": "stats", "device_id": a.ID})

	m := recv(t, ws)
	if m["type"] != "stats" {
		t.Fatalf("got %v", m)
	}
	if int64(m["device_id"].(float64)) != a.ID {
		t.Fatalf("received an update for device %v, which was not subscribed", m["device_id"])
	}
}

// A stalled browser must not stall the poll loop that feeds it, and must not
// grow the controller's memory. Live state supersedes itself, so dropping is
// the right failure.
func TestSlowClientDropsRatherThanBlocking(t *testing.T) {
	h, base := liveHarness(t)
	dev := h.seedDevice("ap-slow-ws", true, nil)
	ws := dialLive(t, h, base)
	send(t, ws, map[string]any{"type": "subscribe", "topic": "device.stats",
		"device_id": dev.ID})
	recv(t, ws)

	// Never read again; publish far more than the buffer holds. If push
	// blocked, this would hang and the test would time out.
	done := make(chan struct{})
	go func() {
		for i := 0; i < sendBuffer*20; i++ {
			h.srv.Hub.Publish(dev.ID, map[string]any{"type": "stats", "n": i})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Publish blocked on a client that stopped reading")
	}
}

func TestUnknownTopicIsReported(t *testing.T) {
	h, base := liveHarness(t)
	ws := dialLive(t, h, base)
	send(t, ws, map[string]any{"type": "subscribe", "topic": "nonsense"})
	if m := recv(t, ws); m["type"] != "error" {
		t.Fatalf("got %v, want an error", m)
	}
	// Still usable afterwards.
	send(t, ws, map[string]any{"type": "ping"})
	if m := recv(t, ws); m["type"] != "pong" {
		t.Fatalf("connection unusable after a bad message: %v", m)
	}
}

func TestHubCloseReleasesEverything(t *testing.T) {
	h, base := liveHarness(t)
	dev := h.seedDevice("ap-close-ws", true, nil)
	ws := dialLive(t, h, base)
	send(t, ws, map[string]any{"type": "subscribe", "topic": "device.stats",
		"device_id": dev.ID})
	recv(t, ws)

	h.srv.Hub.Close()
	if n := h.fleet.focusCount(dev.ID); n != 0 {
		t.Fatalf("focus count = %d after the hub closed, want 0", n)
	}
	if n := h.srv.Hub.Connections(); n != 0 {
		t.Fatalf("%d connections survived Close", n)
	}
}
