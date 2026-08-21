package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/aiden0rchad/oonfeewrt/internal/model"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

// meshView never returns the passphrase. Key is a write-only request field, for
// the same reason wlanView uses one: a list of
// every mesh is exactly the response that ends up cached, screenshot, or pasted
// into a support thread. HasKey answers the question a list screen asks — "is
// this secured" — without making a signed-in browser a credential export path.
//
// The consequence is sharper here than for a WLAN. An open mesh is joinable by
// anyone in radio range, with access to the network behind it, so the
// distinction between "encrypted" and "not" is the single most important thing
// this view carries.
type meshView struct {
	ID        int    `json:"id"`
	MeshID    string `json:"mesh_id"`
	NetworkID int    `json:"network_id"`
	GroupID   int    `json:"group_id"`
	// Band is singular, not a list. Mesh nodes peer only with nodes on the same
	// band, so publishing "one mesh" on two bands is two disjoint backhauls —
	// see model.Mesh.
	Band     string `json:"band"`
	HasKey   bool   `json:"has_key"`
	Key      string `json:"key,omitempty"`
	ClearKey bool   `json:"clear_key,omitempty"`
	Enabled  bool   `json:"enabled"`
}

func viewMesh(m model.Mesh) meshView {
	v := meshView{
		ID: m.ID, MeshID: m.MeshID, NetworkID: m.NetworkID, GroupID: m.GroupID,
		Band: string(m.Band), HasKey: !m.Open(), Enabled: m.Enabled,
	}
	return v
}

func (v meshView) toModel() model.Mesh {
	return model.Mesh{
		ID: v.ID, MeshID: strings.TrimSpace(v.MeshID),
		NetworkID: v.NetworkID, GroupID: v.GroupID,
		Band: model.Band(v.Band), Key: v.Key, Enabled: v.Enabled,
	}
}

// handleGetMesh returns editable mesh metadata but never its passphrase.
func (s *Server) handleGetMesh(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	site, err := s.Store.Site(r.Context())
	if handleStoreErr(w, err, "site") {
		return
	}
	for _, m := range site.Meshes {
		if m.ID == id {
			writeJSON(w, http.StatusOK, viewMesh(m))
			return
		}
	}
	writeErr(w, http.StatusNotFound, "no such mesh")
}

func (s *Server) handleSaveMesh(w http.ResponseWriter, r *http.Request) {
	if !s.lockSiteMutation(w, r) {
		return
	}
	defer s.siteMu.Unlock()
	var v meshView
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
	if v.ClearKey && v.Key != "" {
		writeErr(w, http.StatusBadRequest, "key and clear_key are mutually exclusive")
		return
	}
	m := v.toModel()
	if err := s.Store.SaveMeshWithOptions(r.Context(), &m,
		store.SaveMeshOptions{ClearKey: v.ClearKey}); err != nil {
		if err == store.ErrNotFound {
			writeErr(w, http.StatusNotFound, "no such mesh")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Validated after saving and reported rather than refused, like a WLAN: a
	// half-built mesh is a normal intermediate state on a settings screen, and
	// nothing reaches a device until an apply — which is what refuses.
	site, _ := s.Store.Site(r.Context())
	problems := []string{}
	for _, e := range site.Validate() {
		problems = append(problems, e.Error())
	}
	s.logSiteChange(r.Context(), "site.mesh_saved", map[string]any{
		"mesh": m.ID, "mesh_id": m.MeshID, "band": string(m.Band),
		// Whether it is encrypted is worth auditing; the key is not.
		"encrypted": !m.Open(),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"mesh": viewMesh(m), "problems": problems,
	})
}

func (s *Server) handleDeleteMesh(w http.ResponseWriter, r *http.Request) {
	if !s.lockSiteMutation(w, r) {
		return
	}
	defer s.siteMu.Unlock()
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.Store.DeleteMesh(r.Context(), id); err != nil {
		if err == store.ErrNotFound {
			writeErr(w, http.StatusNotFound, "no such mesh")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logSiteChange(r.Context(), "site.mesh_deleted", map[string]any{"mesh": id})
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": id,
		"note": "removed from the site model. The mesh interface stays on the " +
			"devices until you preview and apply, which is what prunes it — a " +
			"delete that reached out to hardware immediately would be an apply " +
			"nobody previewed",
	})
}
