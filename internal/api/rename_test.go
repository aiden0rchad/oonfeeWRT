package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// setCaps gives a seeded device a capability record, which is where the default
// name comes from.
func setCaps(t *testing.T, h *harness, id int64, caps string) {
	t.Helper()
	if _, err := h.db.SQL().ExecContext(context.Background(),
		`UPDATE devices SET caps_json=? WHERE id=?`, caps, id); err != nil {
		t.Fatal(err)
	}
}

func renamedEvents(t *testing.T, h *harness) int {
	t.Helper()
	var n int
	if err := h.db.SQL().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM events WHERE event='device.renamed'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The default device name is the board model — "TP-Link Archer C6 v2" rather
// than "ap-192-168-1-2" — because that is what someone recognises looking at a
// shelf of routers. It stops working the moment a site has two of the same
// model, so the name must be editable, and for a long time it was not: there
// was no rename in the store, the API or the UI.
func TestRenameADevice(t *testing.T) {
	h, _ := harnessWithEnroller(t)
	dev := h.seedDevice("TP-Link Archer C6 v2 (US)", true, nil)

	w := h.do(http.MethodPost, "/api/v1/devices/"+itoa(int(dev.ID))+"/name",
		map[string]any{"name": "hallway"})
	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	var res struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Name != "hallway" {
		t.Errorf("response says %q", res.Name)
	}
	got, err := h.db.DeviceByID(context.Background(), dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "hallway" {
		t.Errorf("stored name is %q, want %q", got.Name, "hallway")
	}
	// Auditable: a rename changes what every later log line calls the device,
	// so the line that renamed it has to be findable.
	if renamedEvents(t, h) != 1 {
		t.Error("no audit event was written for the rename")
	}
}

// Clearing the field restores the name the device reports for itself, rather
// than being refused. That is the useful reading of an empty box, and it is
// adoption's own fallback chain — so "undo my rename" needs no extra control.
func TestClearingANameRestoresTheDeviceModel(t *testing.T) {
	h, _ := harnessWithEnroller(t)
	dev := h.seedDevice("renamed-by-hand", true, nil)
	setCaps(t, h, dev.ID, `{"Board":{"Model":"TP-Link Archer C6 v2 (US)"}}`)

	w := h.do(http.MethodPost, "/api/v1/devices/"+itoa(int(dev.ID))+"/name",
		map[string]any{"name": "   "})
	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	got, err := h.db.DeviceByID(context.Background(), dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "TP-Link Archer C6 v2 (US)" {
		t.Errorf("clearing the name gave %q, want the board model back", got.Name)
	}
}

// With no model recorded either, the MAC is the last resort — never an empty
// name, which renders as a row nobody can identify or click.
func TestClearingANameWithNoModelFallsBackToTheMAC(t *testing.T) {
	h, _ := harnessWithEnroller(t)
	dev := h.seedDevice("renamed-by-hand-2", true, nil)
	setCaps(t, h, dev.ID, `{}`)

	w := h.do(http.MethodPost, "/api/v1/devices/"+itoa(int(dev.ID))+"/name",
		map[string]any{"name": ""})
	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	got, err := h.db.DeviceByID(context.Background(), dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != dev.MAC {
		t.Errorf("fell back to %q, want the MAC %q", got.Name, dev.MAC)
	}
	if strings.TrimSpace(got.Name) == "" {
		t.Error("a device was left with a blank name")
	}
}
