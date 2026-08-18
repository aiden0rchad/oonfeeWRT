package api

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

// The site model's HTTP surface, and the pending-changes flow around it.
//
// The shape encodes Phase 2's central idea: editing the model changes nothing
// on any device. A WLAN saved here is desired state; it reaches hardware only
// when someone previews and applies. That separation is why an operator can
// build a whole configuration, look at exactly what it would do to each AP, and
// then commit it in one step — and why a mistake is a thing you notice on a
// screen rather than on a network.

// Provisioner is what the API needs from the daemon to preview and apply.
type Provisioner interface {
	Preview(ctx context.Context) (*PreviewResult, error)
	ApplySite(ctx context.Context, req ApplyRequest) (*ApplyResult, error)
}

// Change is one UCI section a preview would create, update or remove.
type Change struct {
	Action  string `json:"action"` // create|update|remove
	Config  string `json:"config"`
	Section string `json:"section"`
	// Options names the keys that would be written, without their values.
	Options []string `json:"options,omitempty"`
	// Option is set when the change removes ONE option rather than the whole
	// section, which is what an option-to-list repair and a cleared flag both
	// produce.
	//
	// Without it the preview rendered a bare "remove wireless.oowrt_bv20" in
	// the colour it uses for destruction, for a change that deletes a single
	// key and is immediately followed by a write to the same section. An
	// operator reading that sees their bridge-VLAN being deleted.
	Option string `json:"option,omitempty"`
	// TouchesKey marks a change that writes a passphrase, so the UI can say so
	// without the preview carrying the secret.
	TouchesKey bool `json:"touches_key,omitempty"`
}

// DriverDefect is a known flaw in a device's wireless driver that this
// configuration would hit.
type DriverDefect struct {
	// WLAN is the SSID that triggers it, or "" for a defect of the hardware
	// itself that no configuration causes and none can avoid.
	WLAN     string `json:"wlan,omitempty"`
	DefectID string `json:"defect_id"`
	Summary  string `json:"summary"`
	Detail   string `json:"detail"`
	// Confidence: "documented" (the device's own OpenWrt page or a maintainer),
	// "measured" (reproduced by this project), "reported" (a filed bug), or
	// "anecdotal". Never render the last one with the authority of the first.
	Confidence string `json:"confidence"`
	Severity   string `json:"severity"`
	Mitigation string `json:"mitigation,omitempty"`
	Source     string `json:"source,omitempty"`
}

// DevicePreview is what one device would do.
type DevicePreview struct {
	DeviceID int64    `json:"device_id"`
	Name     string   `json:"name"`
	Role     string   `json:"role"`
	Changes  []Change `json:"changes"`
	// Blocked means a human owns something this change would have to touch.
	// Nothing is applied to a blocked device — a partial apply around a
	// conflict is how you get half a WLAN.
	Blocked   bool     `json:"blocked"`
	Conflicts []string `json:"conflicts,omitempty"`
	// Omitted names things the site model asked for that are not on this
	// device. Absent, not failed: a WLAN asking for 6 GHz on a device with no
	// 6 GHz radio renders nothing there and says so.
	Omitted []string `json:"omitted,omitempty"`
	// Cautions are rendered, applied, and need a human decision first — an
	// unencrypted mesh anyone in range can join, a wireless bridge that is a
	// layer-2 loop if the device is also cabled.
	//
	// Split out because they used to sit in Omitted, under a heading that read
	// "not an error — the hardware or firmware cannot take it". Both of them
	// describe a network that stops working, and both were styled as the
	// reassurance directly above them.
	Cautions []string `json:"cautions,omitempty"`
	// Undetermined is what could not be established, including every section
	// left in place because nothing could be decided about it.
	//
	// The opposite of Omitted rather than a flavour of it: nothing was left
	// out, and the reason is a gap in what the controller can see. Showing
	// "the existing uplink is left exactly as it is" under a heading about
	// hardware limits says the reverse of what happened.
	Undetermined []string `json:"undetermined,omitempty"`
	// DriverDefects are settings this device WILL accept and will not honour,
	// or will break on, because its wireless driver is known to be broken in
	// that specific way.
	//
	// Distinct from Omitted, which is the controller declining to do something.
	// These ARE applied — the controller does not silently rewrite a user's
	// security settings — so this is the operator's only chance to know. Each
	// carries the confidence it is known with and a source, because wireless
	// folklore is repeated far more than it is verified.
	DriverDefects []DriverDefect `json:"driver_defects,omitempty"`
	// Drift is a section we own whose value on the device no longer matches
	// what we applied — surfaced, never silently corrected.
	Drift []string `json:"drift,omitempty"`
	// Deviations are the per-device overrides in force here. Listed on the
	// preview because a device that differs from the site model should be
	// visible as differing at exactly the moment someone is deciding what to
	// push — not only on a settings screen they may never open.
	Deviations []string `json:"deviations,omitempty"`
	// TouchesTraversal marks a change to the network or firewall config — the
	// configs carrying the path the controller reaches this device through.
	// Applying it needs an explicit acknowledgment.
	TouchesTraversal bool `json:"touches_traversal,omitempty"`
	// Error means this device could not be planned at all. The others are still
	// reported: one unreachable AP must not blank the whole screen.
	Error string `json:"error,omitempty"`
	// CapabilityCause names a recent capability change when this device has
	// something unexplained — an omission, a block, a plan that does nothing.
	// Present only when there is something to explain; a past change on a
	// device that plans cleanly is not news.
	CapabilityCause *CapabilityCause `json:"capability_cause,omitempty"`
}

// CapabilityCause is a probable reason a device is not rendering what the site
// model asks for.
//
// Deliberately a *probable* cause, and worded that way in the UI. The preview
// knows a WLAN was omitted and knows a radio disappeared an hour ago; it does
// not know they are the same fact. Asserting the link would be a guess, and
// omitting it entirely leaves the operator reading "device has no 5 GHz radio"
// with no way to tell their own misconfiguration from a fault that appeared on
// Tuesday. The same sentence describes both.
type CapabilityCause struct {
	// At is when the change was recorded.
	At int64 `json:"at"`
	// Changes are the actionable ones only — a capability that merely became
	// unobservable did not stop the device doing anything, and offering it as
	// the cause would send someone to fix an ACL that is not the problem.
	Changes []string `json:"changes"`
}

// PreviewResult is the whole fleet's pending change.
type PreviewResult struct {
	SiteName string          `json:"site_name"`
	Devices  []DevicePreview `json:"devices"`
	// SiteErrors are model-level problems. When these are present no device was
	// planned, because the same error would otherwise appear once per device.
	SiteErrors []string `json:"site_errors,omitempty"`
}

// ApplyRequest optionally narrows an apply to specific devices.
type ApplyRequest struct {
	// DeviceIDs limits the apply. Empty means every adopted device.
	DeviceIDs []int64 `json:"device_ids,omitempty"`
	// AcknowledgeTraversal is required when a change touches the network or
	// firewall config of a device — IMPLEMENTATION §6's traversal
	// acknowledgment.
	//
	// Those are the configs that carry the path the controller reaches the
	// device through. A change there is applied with a rollback armed like any
	// other, and the rollback is exactly what protects it — but an operator
	// should know they are editing the road before driving down it, rather than
	// finding out from a device that stopped answering.
	AcknowledgeTraversal bool `json:"acknowledge_traversal,omitempty"`
}

// DeviceApply is one device's outcome.
type DeviceApply struct {
	DeviceID int64  `json:"device_id"`
	Name     string `json:"name"`
	// Outcome is the apply engine's, verbatim: applied, reverted, or unknown.
	// Unknown is the one that needs a human — it means the confirm never landed
	// and we could not establish what the device did with the change.
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
	Changes int    `json:"changes,omitempty"`
}

// ApplyResult reports the run.
type ApplyResult struct {
	Devices []DeviceApply `json:"devices"`
	// Aborted marks a run stopped by a failure. The devices after AbortedAfter
	// in the queue were not touched.
	Aborted      bool   `json:"aborted"`
	AbortedAfter string `json:"aborted_after,omitempty"`
}

// ---- wire types for the model ----

// wlanView omits the passphrase.
//
// A list of every WLAN is exactly the response that ends up cached, screenshot
// or pasted into a support thread. HasKey answers the only question a list
// screen actually asks — "is this secured" — and the key itself is available
// from the single-WLAN endpoint, deliberately one explicit request away.
type wlanView struct {
	ID        int      `json:"id"`
	SSID      string   `json:"ssid"`
	NetworkID int      `json:"network_id"`
	GroupID   int      `json:"group_id"`
	Bands     []string `json:"bands"`
	Mode      string   `json:"security_mode"`
	PMF       string   `json:"pmf"`
	HasKey    bool     `json:"has_key"`
	Key       string   `json:"key,omitempty"`
	Roaming   struct {
		FT         bool `json:"ft"`
		FTOverDS   bool `json:"ft_over_ds"`
		KV         bool `json:"kv"`
		FTWithPSK2 bool `json:"ft_with_psk2"`
	} `json:"roaming"`
	Hidden   bool `json:"hidden"`
	Isolate  bool `json:"isolate"`
	MaxAssoc int  `json:"max_assoc"`
	// AllowUplink lets devices join this network as a 4-address bridge rather
	// than as a client. Off unless asked for: it changes what the access points
	// accept from the air.
	AllowUplink bool `json:"allow_uplink"`
	Enabled     bool `json:"enabled"`
}

func viewWLAN(w model.WLAN, reveal bool) wlanView {
	v := wlanView{
		ID: w.ID, SSID: w.SSID, NetworkID: w.NetworkID, GroupID: w.GroupID,
		Mode: string(w.Security.Mode), PMF: string(w.Security.PMF),
		HasKey: w.Security.Key != "", Enabled: w.Enabled,
		Hidden: w.Options.Hidden, Isolate: w.Options.Isolate,
		MaxAssoc: w.Options.MaxAssoc, AllowUplink: w.Options.AllowUplink,
	}
	v.Bands = make([]string, 0, len(w.Bands))
	for _, b := range w.Bands {
		v.Bands = append(v.Bands, string(b))
	}
	v.Roaming.FT = w.Roaming.FT
	v.Roaming.FTOverDS = w.Roaming.FTOverDS
	v.Roaming.KV = w.Roaming.KV
	v.Roaming.FTWithPSK2 = w.Roaming.FTWithPSK2
	if reveal {
		v.Key = w.Security.Key
	}
	return v
}

func (v wlanView) toModel() model.WLAN {
	w := model.WLAN{
		ID: v.ID, SSID: strings.TrimSpace(v.SSID),
		NetworkID: v.NetworkID, GroupID: v.GroupID,
		Enabled: v.Enabled,
		Security: model.Security{
			Mode: model.SecurityMode(v.Mode),
			Key:  v.Key,
			PMF:  model.PMF(v.PMF),
		},
		Options: model.WLANOptions{
			Hidden: v.Hidden, Isolate: v.Isolate, MaxAssoc: v.MaxAssoc,
			AllowUplink: v.AllowUplink,
		},
	}
	w.Roaming.FT = v.Roaming.FT
	w.Roaming.FTOverDS = v.Roaming.FTOverDS
	w.Roaming.KV = v.Roaming.KV
	w.Roaming.FTWithPSK2 = v.Roaming.FTWithPSK2
	for _, b := range v.Bands {
		w.Bands = append(w.Bands, model.Band(b))
	}
	return w
}

// overrideView is one device's deviation from the site model.
type overrideView struct {
	DeviceID int64  `json:"device_id"`
	WLANID   int    `json:"wlan_id"`
	Key      string `json:"key"`
	Value    string `json:"value"`
	// Describe is the sentence the UI shows. Built server-side so the reason a
	// device differs reads the same everywhere it appears.
	Describe string `json:"describe"`
}

type groupView struct {
	ID        int     `json:"id"`
	Name      string  `json:"name"`
	DeviceIDs []int64 `json:"device_ids"`
}

type networkView struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	VLAN    int    `json:"vlan"`
	CIDR    string `json:"cidr"`
	Zone    string `json:"zone"`
	Enabled bool   `json:"enabled"`
}

// ---- handlers ----

func (s *Server) handleSite(w http.ResponseWriter, r *http.Request) {
	site, err := s.Store.Site(r.Context())
	if handleStoreErr(w, err, "site") {
		return
	}
	wlans := make([]wlanView, 0, len(site.WLANs))
	for _, x := range site.WLANs {
		wlans = append(wlans, viewWLAN(x, false))
	}
	meshes := make([]meshView, 0, len(site.Meshes))
	for _, x := range site.Meshes {
		meshes = append(meshes, viewMesh(x, false))
	}
	uplinks := make([]uplinkView, 0, len(site.Uplinks))
	for _, x := range site.Uplinks {
		uplinks = append(uplinks, uplinkView{
			ID: x.ID, DeviceID: x.DeviceID, WLANID: x.WLANID,
			Band: string(x.Band), Enabled: x.Enabled,
		})
	}
	groups := make([]groupView, 0, len(site.Groups))
	for _, g := range site.Groups {
		ids := g.DeviceIDs
		if ids == nil {
			ids = []int64{}
		}
		groups = append(groups, groupView{ID: g.ID, Name: g.Name, DeviceIDs: ids})
	}
	nets := make([]networkView, 0, len(site.Networks))
	for _, n := range site.Networks {
		nets = append(nets, networkView{ID: n.ID, Name: n.Name, VLAN: n.VLAN,
			CIDR: n.CIDR, Zone: n.Zone, Enabled: n.Enabled})
	}
	problems := []string{}
	for _, e := range site.Validate() {
		problems = append(problems, e.Error())
	}
	// Every deviation, listed. The risk of per-device overrides is not any one
	// of them; it is a fleet that drifts apart device by device until nobody
	// can say what is deployed. So they are surfaced wherever the site model
	// is, not hidden behind a per-device screen.
	ssidOf := map[int]string{}
	for _, x := range site.WLANs {
		ssidOf[x.ID] = x.SSID
	}
	deviations := []overrideView{}
	for deviceID, list := range site.Overrides {
		for _, o := range list {
			deviations = append(deviations, overrideView{
				DeviceID: deviceID, WLANID: o.WLANID, Key: string(o.Key),
				Value: o.Value, Describe: o.Describe(ssidOf[o.WLANID]),
			})
		}
	}
	sort.Slice(deviations, func(i, j int) bool {
		if deviations[i].DeviceID != deviations[j].DeviceID {
			return deviations[i].DeviceID < deviations[j].DeviceID
		}
		if deviations[i].WLANID != deviations[j].WLANID {
			return deviations[i].WLANID < deviations[j].WLANID
		}
		return deviations[i].Key < deviations[j].Key
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"name":      site.Name,
		"wlans":     wlans,
		"meshes":    meshes,
		"uplinks":   uplinks,
		"groups":    groups,
		"networks":  nets,
		"problems":  problems,
		"overrides": deviations,
		"overridable": []string{
			string(model.OverrideDisabled), string(model.OverrideHidden),
			string(model.OverrideIsolate), string(model.OverrideMaxAssoc),
		},
		"override_note": "SSID, passphrase, security mode and roaming are " +
			"deliberately not overridable. Keeping them identical across every AP " +
			"is what a controller is for, and a client roaming between APs that " +
			"disagree about them does not fail cleanly — it fails intermittently",
		// The UUID is shown because it is what makes roaming consistent across
		// APs, and an operator debugging fast transition needs to see that
		// every device derived its mobility domain from the same seed.
		"uuid": site.UUID,
	})
}

func (s *Server) handleSiteName(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "the site needs a name")
		return
	}
	if err := s.Store.SetSiteName(r.Context(), strings.TrimSpace(req.Name)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": req.Name})
}

func (s *Server) handleGetWLAN(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	site, err := s.Store.Site(r.Context())
	if handleStoreErr(w, err, "site") {
		return
	}
	for _, x := range site.WLANs {
		if x.ID == id {
			// The passphrase is returned only from this endpoint and only when
			// asked for explicitly, so it never rides along in a list.
			writeJSON(w, http.StatusOK, viewWLAN(x, r.URL.Query().Get("reveal") == "1"))
			return
		}
	}
	writeErr(w, http.StatusNotFound, "no such WLAN")
}

func (s *Server) handleSaveWLAN(w http.ResponseWriter, r *http.Request) {
	var v wlanView
	if !decodeJSON(w, r, &v) {
		return
	}
	if id := r.PathValue("id"); id != "" {
		n, err := strconv.Atoi(id)
		if err != nil || n <= 0 {
			writeErr(w, http.StatusBadRequest, "invalid id")
			return
		}
		v.ID = n
	}
	m := v.toModel()
	if err := s.Store.SaveWLAN(r.Context(), &m); err != nil {
		if err == store.ErrNotFound {
			writeErr(w, http.StatusNotFound, "no such WLAN")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Validate AFTER saving, and report rather than refuse. A half-built WLAN
	// is a normal intermediate state on a settings screen — the model is not
	// pushed anywhere until an apply, and the apply is what refuses.
	site, _ := s.Store.Site(r.Context())
	problems := []string{}
	for _, e := range site.Validate() {
		problems = append(problems, e.Error())
	}
	s.logSiteChange(r.Context(), "site.wlan_saved", map[string]any{
		"wlan": m.ID, "ssid": m.SSID,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"wlan": viewWLAN(m, false), "problems": problems,
	})
}

func (s *Server) handleDeleteWLAN(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.Store.DeleteWLAN(r.Context(), id); err != nil {
		if err == store.ErrNotFound {
			writeErr(w, http.StatusNotFound, "no such WLAN")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logSiteChange(r.Context(), "site.wlan_deleted", map[string]any{"wlan": id})
	// Deleting from the model does not touch a device. The sections stay on
	// their APs until the next apply prunes them — a delete that reached out to
	// hardware immediately would be an apply nobody previewed.
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": id,
		"note": "removed from the site model. The SSID stays on air until you " +
			"apply, which is when the sections are pruned from the devices",
	})
}

func (s *Server) handleSaveGroup(w http.ResponseWriter, r *http.Request) {
	var v groupView
	if !decodeJSON(w, r, &v) {
		return
	}
	if id := r.PathValue("id"); id != "" {
		n, err := strconv.Atoi(id)
		if err != nil || n <= 0 {
			writeErr(w, http.StatusBadRequest, "invalid id")
			return
		}
		v.ID = n
	}
	g := model.APGroup{ID: v.ID, Name: strings.TrimSpace(v.Name), DeviceIDs: v.DeviceIDs}
	if err := s.Store.SaveGroup(r.Context(), &g); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.logSiteChange(r.Context(), "site.group_saved", map[string]any{
		"group": g.ID, "name": g.Name, "devices": len(g.DeviceIDs),
	})
	writeJSON(w, http.StatusOK, groupView{ID: g.ID, Name: g.Name, DeviceIDs: g.DeviceIDs})
}

func (s *Server) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.Store.DeleteGroup(r.Context(), id); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.logSiteChange(r.Context(), "site.group_deleted", map[string]any{"group": id})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

func (s *Server) handleSaveNetwork(w http.ResponseWriter, r *http.Request) {
	var v networkView
	if !decodeJSON(w, r, &v) {
		return
	}
	if id := r.PathValue("id"); id != "" {
		n, err := strconv.Atoi(id)
		if err != nil || n <= 0 {
			writeErr(w, http.StatusBadRequest, "invalid id")
			return
		}
		v.ID = n
	}
	n := model.Network{ID: v.ID, Name: strings.TrimSpace(v.Name), VLAN: v.VLAN,
		CIDR: v.CIDR, Zone: v.Zone, Enabled: v.Enabled}
	if err := s.Store.SaveNetwork(r.Context(), &n); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.logSiteChange(r.Context(), "site.network_saved", map[string]any{
		"network": n.ID, "name": n.Name, "vlan": n.VLAN,
	})
	writeJSON(w, http.StatusOK, networkView{ID: n.ID, Name: n.Name, VLAN: n.VLAN,
		CIDR: n.CIDR, Zone: n.Zone, Enabled: n.Enabled})
}

func (s *Server) handleDeleteNetwork(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.Store.DeleteNetwork(r.Context(), id); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.logSiteChange(r.Context(), "site.network_deleted", map[string]any{"network": id})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

func (s *Server) handleSetOverride(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		WLANID int    `json:"wlan_id"`
		Key    string `json:"key"`
		Value  string `json:"value"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	o := model.Override{
		DeviceID: id, WLANID: req.WLANID,
		Key: model.OverrideKey(req.Key), Value: req.Value,
	}
	if !o.Key.Valid() {
		writeErr(w, http.StatusBadRequest, "\""+req.Key+"\" is not an "+
			"overridable setting. SSID, passphrase, security mode and roaming are "+
			"deliberately not overridable — keeping them identical across every AP "+
			"is what a controller is for")
		return
	}
	if req.WLANID <= 0 {
		writeErr(w, http.StatusBadRequest, "wlan_id is required")
		return
	}
	if err := s.Store.SetOverride(r.Context(), o); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.logSiteChange(r.Context(), "site.override_set", map[string]any{
		"device": id, "wlan": req.WLANID, "key": req.Key, "value": req.Value,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"device_id": id, "wlan_id": req.WLANID, "key": req.Key, "value": req.Value,
		"note": "recorded. Nothing changed on the device until you preview and apply",
	})
}

// ---- preview and apply ----

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	if s.Provision == nil {
		writeErr(w, http.StatusServiceUnavailable, "provisioning is not available")
		return
	}
	res, err := s.Provision.Preview(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	if s.Provision == nil {
		writeErr(w, http.StatusServiceUnavailable, "provisioning is not available")
		return
	}
	var req ApplyRequest
	if r.ContentLength > 0 {
		if !decodeJSON(w, r, &req) {
			return
		}
	}
	res, err := s.Provision.ApplySite(r.Context(), req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// logSiteChange records every edit to the desired state.
//
// The site model is the thing that eventually reaches hardware, so "who changed
// the guest SSID and when" has to be answerable afterwards. Values are never
// logged — a passphrase in the audit trail is a passphrase in a log file.
func (s *Server) logSiteChange(ctx context.Context, event string, detail map[string]any) {
	_ = s.Store.LogEvent(ctx, store.Event{
		Category: "audit", Severity: "info", Event: event, Detail: detail,
	})
}

// uplinkView is one device's wireless uplink over the wire.
//
// No credentials of any kind, and none to omit: an uplink joins by referencing
// a WLAN, so the SSID, the passphrase and the security mode all live in one
// place and this carries only the reference. That is not a serialisation
// convenience — two copies of a passphrase drift, and a bridge whose key no
// longer matches the network it bridges to fails the way a client with a stale
// password fails, intermittently and at the worst moment.
type uplinkView struct {
	ID       int    `json:"id"`
	DeviceID int64  `json:"device_id"`
	WLANID   int    `json:"wlan_id"`
	Band     string `json:"band"`
	Enabled  bool   `json:"enabled"`
}

func (v uplinkView) toModel() model.Uplink {
	return model.Uplink{
		ID: v.ID, DeviceID: v.DeviceID, WLANID: v.WLANID,
		Band: model.Band(v.Band), Enabled: v.Enabled,
	}
}

// handleSaveUplink creates or updates a device's wireless uplink.
//
// Validated against the whole site before it is stored, not after. An uplink is
// only meaningful in relation to a WLAN — whether that network is enabled,
// whether it accepts bridges, whether it is published on the requested band —
// and storing one that cannot work would put a row in the site model whose only
// effect is a rendered station that never associates.
func (s *Server) handleSaveUplink(w http.ResponseWriter, r *http.Request) {
	var v uplinkView
	if !decodeJSON(w, r, &v) {
		return
	}
	if id := r.PathValue("id"); id != "" {
		n, err := strconv.Atoi(id)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "uplink id must be a number")
			return
		}
		v.ID = n
	}
	site, err := s.Store.Site(r.Context())
	if handleStoreErr(w, err, "site") {
		return
	}
	u := v.toModel()
	if errs := u.Validate(site); len(errs) > 0 {
		writeErr(w, http.StatusBadRequest, errs[0].Error())
		return
	}
	if err := s.Store.SaveUplink(r.Context(), &u); handleStoreErr(w, err, "uplink") {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"uplink": uplinkView{ID: u.ID, DeviceID: u.DeviceID, WLANID: u.WLANID,
			Band: string(u.Band), Enabled: u.Enabled},
		// Said on the way out, every time, because the controller cannot see
		// the far end of a cable and this is the one hazard it cannot check.
		"note": "this device will reach the network over the air once applied. " +
			"If it is ALSO connected by ethernet to the same network that is a " +
			"layer-2 loop, and OpenWrt bridges ship with STP off — so nothing " +
			"will break it and the symptom is a network that stops working " +
			"rather than an error",
	})
}

// handleDeleteUplink removes an uplink.
func (s *Server) handleDeleteUplink(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "uplink id must be a number")
		return
	}
	if err := s.Store.DeleteUplink(r.Context(), id); handleStoreErr(w, err, "uplink") {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": id,
		// The delete is the dangerous half, so the warning is on this response
		// rather than only on the create. On a device with no cable the station
		// this removes IS the route to it — which is why the apply engine
		// counts pruning it as touching the management path and will ask for an
		// explicit acknowledgment.
		"note": "applying this removes the station interface. If that device " +
			"has no cable it is how the controller reaches it, so the apply " +
			"will ask you to acknowledge that you are editing the road before " +
			"driving down it",
	})
}
