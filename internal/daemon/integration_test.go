//go:build integration

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/collector"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/telemetry"
)

// Integration test for the one path that mock coverage cannot prove: a
// credential sealed into the database, unsealed at connect time, and accepted by
// a real device's rpcd.
//
//	OONFEE_TEST_HOST=192.168.1.1 OONFEE_TEST_USER=oonfeewrt \
//	OONFEE_TEST_PASS=... go test -tags=integration ./internal/daemon/ -v
//
// Read-only against the device: it logs in and reads board info.

func TestIntegrationSealedCredentialOpensARealSession(t *testing.T) {
	host := os.Getenv("OONFEE_TEST_HOST")
	user := os.Getenv("OONFEE_TEST_USER")
	pass := os.Getenv("OONFEE_TEST_PASS")
	if host == "" || user == "" {
		t.Skip("set OONFEE_TEST_HOST and OONFEE_TEST_USER to run integration tests")
	}
	ctx := context.Background()

	d, err := Open(ctx, testConfig(t, "operator passphrase"), quietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	const mac = "aa:bb:cc:dd:ee:ff"
	blob, err := d.Keys.SealCredential(mac, user, pass)
	if err != nil {
		t.Fatalf("SealCredential: %v", err)
	}
	adopted := int64(1)
	dev := &store.Device{
		MAC: mac, Host: host, Name: "integration", Scheme: "http",
		AdoptedAt: &adopted, CredEnc: blob,
	}
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}

	// Reload from the database rather than reusing the in-memory struct: the
	// point of the test is that the blob survives a round trip through SQLite,
	// which is where a BLOB column could quietly become something else.
	loaded, err := d.Store.DeviceByMAC(ctx, mac)
	if err != nil {
		t.Fatalf("DeviceByMAC: %v", err)
	}
	c, err := d.Connect(ctx, loaded)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	var board struct {
		Release struct {
			Description string `json:"description"`
		} `json:"release"`
	}
	if err := c.Call(ctx, "system", "board", nil, &board); err != nil {
		t.Fatalf("system.board over the reconstituted session: %v", err)
	}
	if board.Release.Description == "" {
		t.Fatal("system.board returned no release description")
	}
	t.Logf("connected with a sealed credential: %s", board.Release.Description)

	// A credential recorded against one device must not open another's session,
	// even with the same keyring.
	other := *loaded
	other.MAC = "11:22:33:44:55:66"
	if _, err := d.Connect(ctx, &other); err == nil {
		t.Fatal("the sealed credential opened under a different device MAC")
	}
}

// The whole Phase 0 path, end to end against real hardware: a credential sealed
// into the keyring, an adopted device in the inventory, the collector polling
// it, and the sink recording what it learned.
func TestIntegrationCollectorPollsARealDevice(t *testing.T) {
	host := os.Getenv("OONFEE_TEST_HOST")
	user := os.Getenv("OONFEE_TEST_USER")
	pass := os.Getenv("OONFEE_TEST_PASS")
	if host == "" || user == "" {
		t.Skip("set OONFEE_TEST_HOST and OONFEE_TEST_USER to run integration tests")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, err := Open(ctx, testConfig(t, "operator passphrase"), quietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	const mac = "02:00:00:00:00:01"
	blob, err := d.Keys.SealCredential(mac, user, pass)
	if err != nil {
		t.Fatalf("SealCredential: %v", err)
	}
	at := int64(1)
	dev := &store.Device{MAC: mac, Host: host, Name: "wrt3200acm", Scheme: "http",
		AdoptedAt: &at, CredEnc: blob}
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}

	if err := d.StartCollector(ctx, collector.Options{
		Baseline: 500 * time.Millisecond, Focused: 200 * time.Millisecond,
		Log: quietLogger(),
	}); err != nil {
		t.Fatalf("StartCollector: %v", err)
	}

	// last_seen moving is the observable proof that a poll completed, went
	// through the sink, and reached the database.
	var seen int64
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		got, err := d.Store.DeviceByMAC(ctx, mac)
		if err != nil {
			t.Fatalf("DeviceByMAC: %v", err)
		}
		if got.LastSeen != nil && *got.LastSeen > 0 {
			seen = *got.LastSeen
			if got.PollState != string(collector.Baseline) {
				t.Errorf("poll_state = %q, want %q", got.PollState, collector.Baseline)
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if seen == 0 {
		t.Fatal("the device was never marked as seen; no poll completed")
	}
	t.Logf("polled a real device through the full stack; last_seen=%d", seen)

	// No unreachable events: the device answered, so the sink must not have
	// recorded a failure alongside the success.
	events, err := d.Store.RecentEvents(ctx, 50)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	for _, e := range events {
		if e.Event == "device.unreachable" {
			t.Errorf("a reachable device logged %s: %+v", e.Event, e.Detail)
		}
	}

	// Focus must raise the tier and take effect promptly, not on the next
	// baseline interval.
	release := d.Focus(dev.ID)
	defer release()
	if tier, ok := d.collectorRef().Tier(dev.ID); !ok || tier != collector.Focused {
		t.Fatalf("tier after Focus = %q (known=%v), want %q", tier, ok, collector.Focused)
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, err := d.Store.DeviceByMAC(ctx, mac)
		if err != nil {
			t.Fatalf("DeviceByMAC: %v", err)
		}
		if got.PollState == string(collector.Focused) {
			t.Log("focused polling reached the device and the database")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("no focused poll was recorded after Focus")
}

// Phase 1 end to end against real hardware: poll a device, roll the samples up
// into SQLite, and serve them through the authenticated API.
func TestIntegrationTelemetryReachesTheAPI(t *testing.T) {
	host := os.Getenv("OONFEE_TEST_HOST")
	user := os.Getenv("OONFEE_TEST_USER")
	pass := os.Getenv("OONFEE_TEST_PASS")
	if host == "" || user == "" {
		t.Skip("set OONFEE_TEST_HOST and OONFEE_TEST_USER to run integration tests")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, err := Open(ctx, testConfig(t, "operator passphrase"), quietLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	const mac = "02:00:00:00:00:02"
	blob, err := d.Keys.SealCredential(mac, user, pass)
	if err != nil {
		t.Fatal(err)
	}
	at := int64(1)
	dev := &store.Device{MAC: mac, Host: host, Name: "wrt3200acm", Scheme: "http",
		AdoptedAt: &at, CredEnc: blob}
	if err := d.Store.UpsertDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}

	served := make(chan error, 1)
	go func() { served <- d.Serve(ctx) }()
	if _, err := waitForHealthz(d.Addr()); err != nil {
		t.Fatalf("healthz: %v", err)
	}

	// Poll fast, and roll up on a window short enough to finish a test. The
	// shipped window is five minutes, which is right in production and useless
	// in a test that has to observe a completed one.
	d.Samples = telemetry.New(telemetry.Options{Window: time.Second, Capacity: 256})
	if err := d.StartCollector(ctx, collector.Options{
		Baseline: 400 * time.Millisecond, Focused: 200 * time.Millisecond,
		Log: quietLogger(),
	}); err != nil {
		t.Fatalf("StartCollector: %v", err)
	}

	base := "http://" + d.Addr()
	client := &http.Client{Timeout: 10 * time.Second}

	// Enrol an operator and keep the session, exactly as a browser would.
	jar, csrf := apiSetup(t, client, base)
	client.Jar = jar

	// Wait for several polls before flushing. A rate series needs TWO readings
	// of its counter before it produces anything, so a test that flushes after
	// the first poll would conclude interface throughput was never collected.
	m := telemetry.NewMaintainer(d.Store, d.Samples, quietLogger())
	var flushed int
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		// Flush a window that has certainly closed.
		m.Now = func() time.Time { return time.Now().Add(2 * time.Second) }
		m.Tick(ctx)
		var n int
		if err := d.Store.SQL().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM series WHERE kind = ?`,
			string(telemetry.KindIfaceRx)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
	}
	if err := d.Store.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rollup_5m`).Scan(&flushed); err != nil {
		t.Fatal(err)
	}
	if flushed == 0 {
		t.Fatal("no telemetry reached the database")
	}
	t.Logf("%d rollup rows from a real device", flushed)

	// The series index must report what was actually collected.
	var idx struct {
		Series map[string][]string `json:"series"`
	}
	apiGet(t, client, base+fmt.Sprintf("/api/v1/devices/%d/series", dev.ID), &idx)
	if len(idx.Series) == 0 {
		t.Fatal("the series index is empty after a successful flush")
	}
	t.Logf("series collected: %v", idx.Series)
	for _, want := range []string{"sys_load1", "sys_mem_pct", "iface_rx_bps"} {
		if _, ok := idx.Series[want]; !ok {
			t.Errorf("no %s series was collected", want)
		}
	}

	// And the points must come back through /stats.
	var series store.Series
	apiGet(t, client, base+fmt.Sprintf(
		"/api/v1/stats/sys_load1?device_id=%d&from=%d&to=%d",
		dev.ID, time.Now().Add(-time.Hour).Unix(), time.Now().Add(time.Hour).Unix()), &series)
	if len(series.Points) == 0 {
		t.Fatal("/stats returned no points for a series that exists")
	}
	if series.Res != "5m" {
		t.Errorf("resolution = %q, want 5m for a one-hour range", series.Res)
	}
	t.Logf("sys_load1: %d point(s), first avg=%.3f", len(series.Points), series.Points[0].Avg)

	// Interface throughput is the series that needed the counter arithmetic.
	if keys, ok := idx.Series["iface_rx_bps"]; ok && len(keys) > 0 {
		apiGet(t, client, base+fmt.Sprintf(
			"/api/v1/stats/iface_rx_bps?device_id=%d&key=%s&from=%d&to=%d",
			dev.ID, keys[0], time.Now().Add(-time.Hour).Unix(),
			time.Now().Add(time.Hour).Unix()), &series)
		for _, p := range series.Points {
			if p.Avg < 0 {
				t.Errorf("%s: negative throughput %v — the counter delta went backwards",
					keys[0], p.Avg)
			}
			if p.Max > 2.5e9 {
				t.Errorf("%s: %v B/s exceeds any plausible link rate", keys[0], p.Max)
			}
		}
		t.Logf("iface_rx_bps[%s]: %d point(s)", keys[0], len(series.Points))
	}

	// The dashboard must agree with the device list.
	var dash struct {
		Devices                 struct{ Total, Online int } `json:"devices"`
		WirelessClients         int                         `json:"wireless_clients"`
		WirelessClientsComplete bool                        `json:"wireless_clients_complete"`
		UnknownOn               []string                    `json:"wireless_clients_unknown_on"`
		KnownDevices            int                         `json:"known_devices"`
		ActiveDevices           int                         `json:"active_devices"`
		UpstreamDevices         int                         `json:"upstream_devices"`
		UnscopedDevices         int                         `json:"unscoped_devices"`
	}
	apiGet(t, client, base+"/api/v1/dashboard", &dash)
	if dash.Devices.Total != 1 || dash.Devices.Online != 1 {
		t.Errorf("dashboard device counts = %+v, want 1 total / 1 online", dash.Devices)
	}
	// The device answered every poll above, so its row-scoped count is a complete
	// fleet total. The numeric field remains available when coverage is partial;
	// this flag is what licenses the dashboard to present it as the total.
	if !dash.WirelessClientsComplete {
		t.Fatalf("wireless client total is incomplete after successful polls; unreadable on: %v",
			dash.UnknownOn)
	}
	// The client inventory comes from the baseline poll, so a live LAN has some
	// hosts SOMEWHERE. Asserted across all three scopes rather than on the local
	// count alone: the reference device is WAN-facing behind another router, so
	// most of what it can see is upstream, and a run that happened to observe
	// only upstream neighbours would be a correct result, not a failure.
	seen := dash.KnownDevices + dash.UpstreamDevices + dash.UnscopedDevices
	if seen == 0 {
		t.Error("no LAN devices recorded; the host-hint sources did not populate")
	}
	t.Logf("dashboard: %d device(s) online, %d wireless client(s), "+
		"%d on this network (%d active), %d upstream, %d unplaced",
		dash.Devices.Online, dash.WirelessClients, dash.KnownDevices,
		dash.ActiveDevices, dash.UpstreamDevices, dash.UnscopedDevices)

	// The dashboard headline and the client grid answer the same question and
	// must not answer it differently. They did: the dashboard counted rows and
	// the grid counted scopes, so on this device one screen said 14 devices and
	// the other listed 3 — both captioned as this network's.
	var cl struct {
		Clients []struct {
			MAC        string `json:"mac"`
			Scope      string `json:"scope"`
			Connection string `json:"connection"`
		} `json:"clients"`
		Total  int `json:"total"`
		Facets struct {
			Scope      []store.Facet `json:"scope"`
			Connection []store.Facet `json:"connection"`
		} `json:"facets"`
	}
	// A limit above the fleet's client count, so the page IS the table and the
	// server's counts can be checked against the rows for once. Above that size
	// the rail is the only thing that knows the totals, which is the point of
	// counting it server-side — but it is also why it has to be verified here.
	apiGet(t, client, base+"/api/v1/clients?all=1&limit=5000", &cl)

	scopeFacet := map[string]int{}
	for _, f := range cl.Facets.Scope {
		scopeFacet[f.Value] = f.Count
	}
	connFacet := map[string]int{}
	for _, f := range cl.Facets.Connection {
		connFacet[f.Value] = f.Count
	}

	// The rail's counts must match the rows it is filtering, or the numbers are
	// decoration.
	fromRows := map[string]int{}
	connRows := map[string]int{}
	for _, c := range cl.Clients {
		fromRows[c.Scope]++
		connRows[c.Connection]++
	}
	if cl.Total != len(cl.Clients) {
		t.Errorf("total = %d but %d rows returned under a limit of 5000",
			cl.Total, len(cl.Clients))
	}
	for _, scope := range []string{"local", "upstream", "unknown"} {
		if scopeFacet[scope] != fromRows[scope] {
			t.Errorf("scope %q: rail says %d, rows say %d",
				scope, scopeFacet[scope], fromRows[scope])
		}
	}
	// The connection rail is computed in SQL and the rows' connection field in
	// Go, from the same station telemetry. On real data they must still agree.
	for _, conn := range []string{"wireless", "unknown"} {
		if connFacet[conn] != connRows[conn] {
			t.Errorf("connection %q: rail says %d, rows say %d — the SQL "+
				"derivation and the row enrichment disagree on live data",
				conn, connFacet[conn], connRows[conn])
		}
	}
	if scopeFacet["local"] != dash.KnownDevices {
		t.Errorf("the client list says %d local, the dashboard says %d on this "+
			"network — the two screens disagree about the same question",
			scopeFacet["local"], dash.KnownDevices)
	}
	// The regression itself: with anything on the other side of the router
	// present, the headline must be strictly smaller than the row count.
	if elsewhere := len(cl.Clients) - fromRows["local"]; elsewhere > 0 &&
		dash.KnownDevices >= len(cl.Clients) {
		t.Errorf("%d of %d known hosts are not on this network, but the "+
			"dashboard still reports %d", elsewhere, len(cl.Clients),
			dash.KnownDevices)
	}
	t.Logf("clients: %d row(s) of %d, scopes %v, connection %v",
		len(cl.Clients), cl.Total, scopeFacet, connFacet)

	// And a real second page: the total and the facets must not move when the
	// window does, which is the whole reason they are counted in SQL.
	if len(cl.Clients) > 1 {
		var p2 struct {
			Clients []struct {
				MAC string `json:"mac"`
			} `json:"clients"`
			Total  int `json:"total"`
			Facets struct {
				Scope []store.Facet `json:"scope"`
			} `json:"facets"`
		}
		apiGet(t, client, base+"/api/v1/clients?all=1&limit=1&offset=1", &p2)
		if len(p2.Clients) != 1 {
			t.Errorf("page 2 held %d rows, want 1", len(p2.Clients))
		}
		if p2.Total != cl.Total {
			t.Errorf("total changed with the page: %d then %d", cl.Total, p2.Total)
		}
		p2Scope := map[string]int{}
		for _, f := range p2.Facets.Scope {
			p2Scope[f.Value] = f.Count
		}
		for k, v := range scopeFacet {
			if p2Scope[k] != v {
				t.Errorf("scope %q counted %d on the full list and %d on a "+
					"one-row page — the rail is counting the page", k, v, p2Scope[k])
			}
		}
		if p2.Clients[0].MAC == cl.Clients[0].MAC {
			t.Error("offset=1 returned the same row as offset=0")
		}
	}

	_ = csrf
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Serve did not return")
	}
}

func apiSetup(t *testing.T, client *http.Client, base string) (http.CookieJar, string) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client.Jar = jar
	body := strings.NewReader(`{"username":"admin","password":"integration-test-password"}`)
	resp, err := client.Post(base+"/api/v1/setup", "application/json", body)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("setup: %d %s", resp.StatusCode, b)
	}
	var out struct {
		CSRF string `json:"csrf"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return jar, out.CSRF
}

func apiGet(t *testing.T, client *http.Client, url string, into any) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: %d %s", url, resp.StatusCode, b)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("GET %s: decode: %v", url, err)
	}
}
