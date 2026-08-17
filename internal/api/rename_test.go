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

// The length limit belongs to what the OPERATOR typed, and to nothing else.
//
// It was checked after the fallbacks, which applied it to a string the caller
// never supplied: a device whose board model ran past the limit could not have
// its name cleared at all, and the refusal quoted a length the request did not
// have. The fallback is machine-derived, so it is trimmed rather than rejected
// — an unusable name is a worse answer than a shortened one.
func TestClearingANameSurvivesAnOverlongModel(t *testing.T) {
	h, _ := harnessWithEnroller(t)
	dev := h.seedDevice("renamed-by-hand-3", true, nil)
	long := strings.Repeat("VeryLongBoardName", 12) // ~204 chars
	caps, err := json.Marshal(map[string]any{"Board": map[string]any{"Model": long}})
	if err != nil {
		t.Fatal(err)
	}
	setCaps(t, h, dev.ID, string(caps))

	w := h.do(http.MethodPost, "/api/v1/devices/"+itoa(int(dev.ID))+"/name",
		map[string]any{"name": ""})
	if w.Code != http.StatusOK {
		t.Fatalf("clearing the name was refused: %d %s", w.Code, w.Body.String())
	}
	got, err := h.db.DeviceByID(context.Background(), dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Name) > 120 {
		t.Errorf("stored a %d-character name", len(got.Name))
	}
	if !strings.HasPrefix(long, got.Name) {
		t.Errorf("the trimmed name %q is not a prefix of the model", got.Name)
	}
}

// And a name the operator actually typed over the limit is still refused,
// rather than silently shortened into something they did not choose.
func TestAnOverlongTypedNameIsRefused(t *testing.T) {
	h, _ := harnessWithEnroller(t)
	dev := h.seedDevice("renamed-by-hand-4", true, nil)

	w := h.do(http.MethodPost, "/api/v1/devices/"+itoa(int(dev.ID))+"/name",
		map[string]any{"name": strings.Repeat("x", 121)})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", w.Code, w.Body.String())
	}
	got, err := h.db.DeviceByID(context.Background(), dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed-by-hand-4" {
		t.Errorf("a refused rename changed the name to %q", got.Name)
	}
}
