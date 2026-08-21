package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

// seedMeshDeps creates the network and group a mesh must reference.
func seedMeshDeps(t *testing.T, h *harness) (netID, groupID int) {
	t.Helper()
	ctx := context.Background()
	n := &model.Network{Name: "lan", VLAN: 1, CIDR: "192.168.1.1/24", Enabled: true}
	if err := h.db.SaveNetwork(ctx, n); err != nil {
		t.Fatal(err)
	}
	g := &model.APGroup{Name: "all"}
	if err := h.db.SaveGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	return n.ID, g.ID
}

// The passphrase never appears in any read response.
//
// Sharper here than for a WLAN: an open mesh is joinable by anyone in radio
// range, with access to the network behind it, so "is this secured" is the most
// important thing the view carries. A signed-in browser can edit this metadata
// without becoming a plaintext credential export path.
func TestMeshReadsOmitTheKeyButSayWhetherThereIsOne(t *testing.T) {
	h := newHarness(t)
	h.setup()
	netID, groupID := seedMeshDeps(t, h)

	w := h.do(http.MethodPost, "/api/v1/site/meshes", map[string]any{
		"mesh_id": "backhaul", "network_id": netID, "group_id": groupID,
		"band": "5g", "key": "a-mesh-passphrase", "enabled": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); contains(body, "a-mesh-passphrase") {
		t.Errorf("the save response echoed the passphrase: %s", body)
	}

	w = h.do(http.MethodGet, "/api/v1/site", nil)
	var site struct {
		Meshes []struct {
			ID      int    `json:"id"`
			MeshID  string `json:"mesh_id"`
			Band    string `json:"band"`
			HasKey  bool   `json:"has_key"`
			Key     string `json:"key"`
			Enabled bool   `json:"enabled"`
		} `json:"meshes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &site); err != nil {
		t.Fatal(err)
	}
	if len(site.Meshes) != 1 {
		t.Fatalf("meshes = %+v, want one", site.Meshes)
	}
	m := site.Meshes[0]
	if m.Key != "" {
		t.Errorf("the site listing carries the passphrase: %q", m.Key)
	}
	if !m.HasKey {
		t.Error("has_key is false for an encrypted mesh; the list cannot tell " +
			"an operator whether anyone in range can join")
	}
	if m.Band != "5g" || m.MeshID != "backhaul" {
		t.Errorf("mesh = %+v", m)
	}

	// The single-mesh endpoint is redacted too. Authentication protects the
	// controller, but should not turn a browser response into a reusable secret.
	for _, suffix := range []string{"", "?reveal=1"} {
		w = h.do(http.MethodGet, "/api/v1/site/meshes/"+itoa(m.ID)+suffix, nil)
		var one struct {
			Key    string `json:"key"`
			HasKey bool   `json:"has_key"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &one); err != nil {
			t.Fatal(err)
		}
		if one.Key != "" || !one.HasKey || contains(w.Body.String(), "a-mesh-passphrase") {
			t.Errorf("single-mesh response %q exposed or hid key state: %s",
				suffix, w.Body.String())
		}
	}
	h.cookies, h.csrf = nil, ""
	w = h.do(http.MethodGet, "/api/v1/site/meshes/"+itoa(m.ID)+"?reveal=1", nil)
	if w.Code != http.StatusUnauthorized || contains(w.Body.String(), "a-mesh-passphrase") {
		t.Errorf("unauthenticated mesh read = %d %s, want redacted 401", w.Code, w.Body.String())
	}
}

// A write-back with no key must not silently open the mesh.
//
// The list omits the key, so a UI that reads a mesh and saves it back sends an
// empty one. Treating that as "make it open" converts an encrypted backhaul
// into one anybody in range can join — from an edit that never mentioned
// security.
func TestSavingAMeshWithoutAKeyKeepsItEncrypted(t *testing.T) {
	h := newHarness(t)
	h.setup()
	netID, groupID := seedMeshDeps(t, h)

	w := h.do(http.MethodPost, "/api/v1/site/meshes", map[string]any{
		"mesh_id": "backhaul", "network_id": netID, "group_id": groupID,
		"band": "5g", "key": "a-mesh-passphrase", "enabled": true,
	})
	var saved struct {
		Mesh struct {
			ID int `json:"id"`
		} `json:"mesh"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}

	// Rename only, as a UI round-trip would.
	w = h.do(http.MethodPost, "/api/v1/site/meshes/"+itoa(saved.Mesh.ID),
		map[string]any{
			"mesh_id": "backhaul-2", "network_id": netID, "group_id": groupID,
			"band": "5g", "enabled": true,
		})
	if w.Code != http.StatusOK {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		Mesh struct {
			MeshID string `json:"mesh_id"`
			HasKey bool   `json:"has_key"`
		} `json:"mesh"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Mesh.HasKey {
		t.Error("a keyless update opened the mesh; anyone in radio range can " +
			"now join and reach the network behind it")
	}
	if out.Mesh.MeshID != "backhaul-2" {
		t.Errorf("the rename did not land: %+v", out.Mesh)
	}
}

func TestMeshKeyCanOnlyBeClearedExplicitly(t *testing.T) {
	h := newHarness(t)
	h.setup()
	netID, groupID := seedMeshDeps(t, h)
	w := h.do(http.MethodPost, "/api/v1/site/meshes", map[string]any{
		"mesh_id": "backhaul", "network_id": netID, "group_id": groupID,
		"band": "5g", "key": "a-mesh-passphrase", "enabled": true,
	})
	var saved struct {
		Mesh struct {
			ID int `json:"id"`
		} `json:"mesh"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/site/meshes/" + itoa(saved.Mesh.ID)
	w = h.do(http.MethodPost, path, map[string]any{
		"mesh_id": "backhaul", "network_id": netID, "group_id": groupID,
		"band": "5g", "key": "replacement", "clear_key": true, "enabled": true,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("key plus clear_key status=%d, want 400", w.Code)
	}
	w = h.do(http.MethodPost, path, map[string]any{
		"mesh_id": "backhaul", "network_id": netID, "group_id": groupID,
		"band": "5g", "clear_key": true, "enabled": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("explicit clear: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		Mesh struct {
			HasKey bool   `json:"has_key"`
			Key    string `json:"key"`
		} `json:"mesh"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Mesh.HasKey || out.Mesh.Key != "" || contains(w.Body.String(), "a-mesh-passphrase") {
		t.Fatalf("explicit clear returned or retained key state: %s", w.Body.String())
	}
	site, err := h.db.Site(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(site.Meshes) != 1 || !site.Meshes[0].Open() {
		t.Fatal("explicit clear did not make the stored mesh open")
	}
}

// Model problems are reported, not refused: a half-built mesh is a normal
// intermediate state on a settings screen, and nothing reaches a device until
// an apply — which is what refuses.
func TestAnInvalidMeshIsReportedRatherThanRejected(t *testing.T) {
	h := newHarness(t)
	h.setup()
	netID, groupID := seedMeshDeps(t, h)

	w := h.do(http.MethodPost, "/api/v1/site/meshes", map[string]any{
		// No band: nodes peer only within a band, so this cannot render.
		"mesh_id": "backhaul", "network_id": netID, "group_id": groupID,
		"enabled": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want the save to succeed and report: %s",
			w.Code, w.Body.String())
	}
	var out struct {
		Problems []string `json:"problems"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Problems) == 0 {
		t.Error("a mesh with no band was saved with no problem reported")
	}
}

// Deleting from the model must not reach out to hardware.
func TestDeletingAMeshSaysItStaysOnTheDevices(t *testing.T) {
	h := newHarness(t)
	h.setup()
	netID, groupID := seedMeshDeps(t, h)

	w := h.do(http.MethodPost, "/api/v1/site/meshes", map[string]any{
		"mesh_id": "backhaul", "network_id": netID, "group_id": groupID,
		"band": "5g", "key": "a-mesh-passphrase", "enabled": true,
	})
	var saved struct {
		Mesh struct {
			ID int `json:"id"`
		} `json:"mesh"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	w = h.do(http.MethodDelete, "/api/v1/site/meshes/"+itoa(saved.Mesh.ID), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if !contains(w.Body.String(), "apply") {
		t.Errorf("the delete response does not say the interface stays until an "+
			"apply: %s", w.Body.String())
	}
	w = h.do(http.MethodDelete, "/api/v1/site/meshes/"+itoa(saved.Mesh.ID), nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("deleting twice = %d, want 404", w.Code)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
