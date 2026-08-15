package render

import (
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/internal/capability"
	"github.com/aiden0rchad/oonfeewrt/internal/model"
)

func meshSite(m model.Mesh) model.Site {
	s := testSite()
	m.NetworkID = s.Networks[0].ID
	m.GroupID = s.Groups[0].ID
	s.Meshes = []model.Mesh{m}
	return s
}

func meshCaps(state capability.State) *capability.Registry {
	c := dualBandCaps()
	c.Set(capability.FeatMesh, state)
	return c
}

func sectionsOfMode(doc Doc, mode string) []Section {
	var out []Section
	for _, s := range doc.Sections {
		if s.Config == "wireless" && s.Values["mode"] == mode {
			out = append(out, s)
		}
	}
	return out
}

// The arrangement the whole design is for: a node carrying a mesh backhaul
// while still serving clients.
//
// Modelling mesh as a device ROLE would make these mutually exclusive, and the
// combination is exactly what "AP bridge mesh with switch support" means — an
// old router extending the network over the air while still serving clients
// and its wired ports.
func TestAMeshRendersAlongsideTheAPsNotInsteadOfThem(t *testing.T) {
	site := meshSite(model.Mesh{
		ID: 1, MeshID: "oonfee-backhaul", Band: model.Band5G,
		Key: "a-mesh-passphrase", Enabled: true,
	})
	doc, rep, err := Render(site, model.Device{ID: 7, Role: model.RoleAP},
		meshCaps(capability.Present), Existing{})
	if err != nil {
		t.Fatal(err)
	}
	aps := sectionsOfMode(doc, "ap")
	meshes := sectionsOfMode(doc, "mesh")
	if len(aps) == 0 {
		t.Error("adding a mesh removed the AP interfaces")
	}
	if len(meshes) != 1 {
		t.Fatalf("got %d mesh interfaces, want 1 (report: %+v)", len(meshes), rep)
	}
	m := meshes[0]
	if m.Values["mesh_id"] != "oonfee-backhaul" {
		t.Errorf("mesh_id = %q", m.Values["mesh_id"])
	}
	if m.Values["ssid"] != "" {
		t.Error("a mesh point has a mesh_id, not an SSID; setting both is a " +
			"different interface than the one intended")
	}
	if m.Values["mesh_fwding"] != "1" {
		t.Error("without mesh_fwding the nodes cannot relay for each other, " +
			"which is the difference between a mesh and a set of lonely APs")
	}
	if m.Values[OwnershipTag] != "1" {
		t.Error("the mesh section is not marked as ours, so un-adopt would " +
			"leave it behind")
	}
}

// SAE mandates protected management frames. Rendering one without the other
// produces peers that refuse each other for reasons nobody enjoys debugging.
func TestAnEncryptedMeshGetsSAEAndPMF(t *testing.T) {
	site := meshSite(model.Mesh{
		ID: 1, MeshID: "backhaul", Band: model.Band5G,
		Key: "a-mesh-passphrase", Enabled: true,
	})
	doc, _, err := Render(site, model.Device{ID: 7}, meshCaps(capability.Present), Existing{})
	if err != nil {
		t.Fatal(err)
	}
	m := sectionsOfMode(doc, "mesh")[0]
	if m.Values["encryption"] != "sae" {
		t.Errorf("encryption = %q, want sae", m.Values["encryption"])
	}
	if m.Values["ieee80211w"] != string(model.PMFRequired) {
		t.Errorf("ieee80211w = %q; SAE without required PMF gives peers that "+
			"refuse each other", m.Values["ieee80211w"])
	}
	if m.Values["key"] != "a-mesh-passphrase" {
		t.Error("the passphrase did not reach the section")
	}
}

// An open mesh is allowed — a trusted wired-equivalent segment is a real case —
// but the consequence is not obvious and must be said once, on the screen where
// the decision is visible.
func TestAnOpenMeshRendersAndSaysWhatThatMeans(t *testing.T) {
	site := meshSite(model.Mesh{
		ID: 1, MeshID: "open-backhaul", Band: model.Band5G, Enabled: true,
	})
	doc, rep, err := Render(site, model.Device{ID: 7}, meshCaps(capability.Present), Existing{})
	if err != nil {
		t.Fatal(err)
	}
	m := sectionsOfMode(doc, "mesh")
	if len(m) != 1 || m[0].Values["encryption"] != "none" {
		t.Fatalf("an open mesh rendered as %+v", m)
	}
	if m[0].Values["key"] != "" {
		t.Error("an open mesh carries a key")
	}
	var warned bool
	for _, om := range rep.Omissions {
		if strings.Contains(om.Reason, "unencrypted") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("nothing said the mesh is open: %+v", rep.Omissions)
	}
}

// The three-state gate, and the distinction that makes it worth having.
//
// "Your device cannot do this" and "we could not find out" send an operator to
// completely different places — buy different hardware, versus widen an ACL or
// check a package. Rendering into the second would send an interface hostapd
// may refuse at startup, which surfaces as a radio that silently does not come
// up.
func TestMeshIsGatedOnCapabilityAndSaysWhichKind(t *testing.T) {
	site := meshSite(model.Mesh{
		ID: 1, MeshID: "backhaul", Band: model.Band5G,
		Key: "a-mesh-passphrase", Enabled: true,
	})
	for _, tc := range []struct {
		state capability.State
		want  string
	}{
		{capability.Absent, "wpad build does not carry"},
		{capability.NotObservable, "could not be established"},
	} {
		doc, rep, err := Render(site, model.Device{ID: 7}, meshCaps(tc.state), Existing{})
		if err != nil {
			t.Fatal(err)
		}
		if got := sectionsOfMode(doc, "mesh"); len(got) != 0 {
			t.Errorf("%s: rendered a mesh anyway: %+v", tc.state, got)
		}
		var found bool
		for _, om := range rep.Omissions {
			if strings.Contains(om.Reason, tc.want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no omission containing %q; got %+v",
				tc.state, tc.want, rep.Omissions)
		}
	}
}

// Nodes peer only with nodes on the same band, so a device without that radio
// cannot join — and the message has to say why, since "no 5 GHz radio" alone
// does not explain that adding the mesh on 2.4 would be a DIFFERENT mesh.
func TestAMeshNeedsItsOwnBandOnTheDevice(t *testing.T) {
	site := meshSite(model.Mesh{
		ID: 1, MeshID: "backhaul", Band: model.Band6G,
		Key: "a-mesh-passphrase", Enabled: true,
	})
	doc, rep, err := Render(site, model.Device{ID: 7}, meshCaps(capability.Present), Existing{})
	if err != nil {
		t.Fatal(err)
	}
	if got := sectionsOfMode(doc, "mesh"); len(got) != 0 {
		t.Fatalf("rendered a 6 GHz mesh on a 2.4/5 device: %+v", got)
	}
	var found bool
	for _, om := range rep.Omissions {
		if strings.Contains(om.Reason, "peer") && strings.Contains(om.Reason, "6g") {
			found = true
		}
	}
	if !found {
		t.Errorf("the omission does not explain the band constraint: %+v", rep.Omissions)
	}
}

// A role that does not publish WLANs does not carry a mesh either: a mesh point
// is a wireless interface.
func TestASwitchGetsNoMesh(t *testing.T) {
	site := meshSite(model.Mesh{
		ID: 1, MeshID: "backhaul", Band: model.Band5G,
		Key: "a-mesh-passphrase", Enabled: true,
	})
	doc, _, err := Render(site, model.Device{ID: 7, Role: model.RoleSwitch},
		meshCaps(capability.Present), Existing{})
	if err != nil {
		t.Fatal(err)
	}
	if got := sectionsOfMode(doc, "mesh"); len(got) != 0 {
		t.Errorf("a switch was sent a mesh interface: %+v", got)
	}
}

// A foreign section wearing our name is a conflict, exactly as for a WLAN.
func TestAForeignSectionWithTheMeshNameIsAConflict(t *testing.T) {
	site := meshSite(model.Mesh{
		ID: 1, MeshID: "backhaul", Band: model.Band5G,
		Key: "a-mesh-passphrase", Enabled: true,
	})
	existing := WirelessOnly(map[string]map[string]string{
		"oowrt_mesh1_radio0": {".type": "wifi-iface", "mode": "ap"},
	})
	_, rep, err := Render(site, model.Device{ID: 7}, meshCaps(capability.Present), existing)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.HasConflicts() {
		t.Errorf("a foreign section with our mesh name was not a conflict: %+v", rep)
	}
}

// Absent has two causes and they send an operator to opposite places.
//
// A missing wpad-mesh package is fixable by installing one. A driver that
// refuses to bring a mesh interface up is not — the daemon already supports
// mesh, so the answer is different hardware. Telling someone to install a
// package they already have is worse than saying nothing, and that is what
// this message did until a real apply exposed it.
func TestAnAbsentMeshSaysWhichKindOfAbsentItIs(t *testing.T) {
	site := meshSite(model.Mesh{
		ID: 1, MeshID: "backhaul", Band: model.Band5G,
		Key: "a-mesh-passphrase", Enabled: true,
	})

	// Cause 1: the wpad build.
	pkg := meshCaps(capability.Absent)
	_, rep, err := Render(site, model.Device{ID: 7}, pkg, Existing{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasOmissionContaining(rep, "wpad-mesh-*") {
		t.Errorf("a package-caused absence does not name the package: %+v", rep.Omissions)
	}

	// Cause 2: the driver. Same state, different reason, different advice.
	drv := meshCaps(capability.Absent)
	drv.AddQuirk(capability.Quirk{Source: "mac80211", Field: "mesh-point",
		Reason: "the driver refuses to bring a mesh interface up"})
	_, rep, err = Render(site, model.Device{ID: 7}, drv, Existing{})
	if err != nil {
		t.Fatal(err)
	}
	if hasOmissionContaining(rep, "wpad-mesh-*") {
		t.Error("a driver-caused absence tells the operator to install a " +
			"package they already have")
	}
	if !hasOmissionContaining(rep, "driver") {
		t.Errorf("a driver-caused absence does not say so: %+v", rep.Omissions)
	}
	if !hasOmissionContaining(rep, "different radio") {
		t.Errorf("nothing tells the operator what would actually fix it: %+v",
			rep.Omissions)
	}
}

func hasOmissionContaining(rep Report, sub string) bool {
	for _, om := range rep.Omissions {
		if strings.Contains(om.Reason, sub) {
			return true
		}
	}
	return false
}
