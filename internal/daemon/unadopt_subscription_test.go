package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aiden0rchad/oonfeewrt/internal/api"
	"github.com/aiden0rchad/oonfeewrt/internal/collector"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

func serveAuthenticatedDaemon(t *testing.T, d *Daemon) (*http.Client, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- d.Serve(ctx) }()
	if _, err := waitForHealthz(d.Addr()); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not stop")
		}
	})

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	base := "http://" + d.Addr()
	resp, err := client.Post(base+"/api/v1/setup", "application/json",
		strings.NewReader(`{"username":"admin","password":"integration-test-password"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("setup = %d: %s", resp.StatusCode, body)
	}
	return client, base
}

func dialDaemonLive(t *testing.T, client *http.Client, base string) *websocket.Conn {
	t.Helper()
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	header := http.Header{"Origin": []string{base}}
	for _, cookie := range client.Jar.Cookies(parsed) {
		header.Add("Cookie", cookie.Name+"="+cookie.Value)
	}
	ws, _, err := websocket.Dial(context.Background(),
		strings.Replace(base, "http://", "ws://", 1)+"/api/v1/live",
		&websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	return ws
}

func writeLiveMessage(t *testing.T, ws *websocket.Conn, message any) {
	t.Helper()
	data, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}
}

func readLiveUntil(t *testing.T, ws *websocket.Conn, match func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		_, data, err := ws.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("read live message: %v", err)
		}
		var message map[string]any
		if err := json.Unmarshal(data, &message); err != nil {
			t.Fatal(err)
		}
		if match(message) {
			return message
		}
	}
	t.Fatal("matching live message did not arrive")
	return nil
}

func TestFailedUnadoptPreservesLiveSubscriptionAndFocus(t *testing.T) {
	ctx := context.Background()
	d := openDaemon(t)
	mac := "02:00:00:00:30:01"
	credential, err := d.Keys.SealCredential(mac, "controller", "secret")
	if err != nil {
		t.Fatal(err)
	}
	adopted := int64(1)
	device := &store.Device{MAC: mac, Host: "127.0.0.1", Port: 1,
		Name: "retryable", Role: "ap", Scheme: "http", AdoptedAt: &adopted,
		CredEnc: credential}
	if err := d.Store.UpsertDevice(ctx, device); err != nil {
		t.Fatal(err)
	}
	if err := d.Store.RecordOwned(ctx, []store.OwnedSection{{
		DeviceID: device.ID, Config: "wireless", Section: "managed",
		RenderedHash: "known", AppliedAt: 1,
	}}); err != nil {
		t.Fatal(err)
	}

	// A registered but not started collector gives the live subscription a real
	// focus lease without introducing asynchronous network polls into this test.
	fleet := collector.New(d.sink(), collector.Options{Log: quietLogger()})
	fleet.Add(d.target(device))
	d.mu.Lock()
	d.collector = fleet
	d.mu.Unlock()

	client, base := serveAuthenticatedDaemon(t, d)
	ws := dialDaemonLive(t, client, base)
	writeLiveMessage(t, ws, map[string]any{
		"type": "subscribe", "topic": "device.stats", "device_id": device.ID,
	})
	readLiveUntil(t, ws, func(message map[string]any) bool {
		return message["type"] == "subscribed"
	})
	if tier, ok := fleet.Tier(device.ID); !ok || tier != collector.Focused {
		t.Fatalf("subscription tier = %q,%v, want focused", tier, ok)
	}

	result, err := d.Unadopt(ctx, api.UnadoptRequest{DeviceID: device.ID})
	if err == nil || result == nil || result.Removed {
		t.Fatalf("failed unadopt = %+v, %v", result, err)
	}
	if tier, ok := fleet.Tier(device.ID); !ok || tier != collector.Focused || fleet.Quiesced(device.ID) {
		t.Fatalf("failed unadopt lost or stranded focus: tier=%q known=%v quiesced=%v",
			tier, ok, fleet.Quiesced(device.ID))
	}

	d.api.Hub.Publish(device.ID, map[string]any{
		"type": "stats", "device_id": device.ID, "marker": "after-failed-unadopt",
	})
	readLiveUntil(t, ws, func(message map[string]any) bool {
		return message["marker"] == "after-failed-unadopt"
	})
}
