package api

import (
	"context"
	"net/http"

	"github.com/aiden0rchad/oonfeewrt/internal/integrations"
)

type AdGuardChecker interface {
	Check(context.Context, integrations.AdGuardConfig) integrations.AdGuardResult
}

type WireGuardReader interface {
	ReadWireGuard(context.Context, int64) (integrations.WireGuardResult, error)
}

type adGuardConfigView struct {
	Configured     bool   `json:"configured"`
	URL            string `json:"url"`
	Username       string `json:"username"`
	HasPassword    bool   `json:"has_password"`
	TLSFingerprint string `json:"tls_fingerprint"`
}

func viewAdGuard(config *integrations.AdGuardConfig) adGuardConfigView {
	if config == nil {
		return adGuardConfigView{}
	}
	return adGuardConfigView{Configured: true, URL: config.URL, Username: config.Username, HasPassword: config.Password != "", TLSFingerprint: config.TLSFingerprint}
}

func (s *Server) handleAdGuardConfig(w http.ResponseWriter, r *http.Request) {
	config, err := s.Store.LoadAdGuardConfig(r.Context())
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "the integration configuration could not be read or decrypted")
		return
	}
	writeJSON(w, http.StatusOK, viewAdGuard(config))
}

func (s *Server) handleSaveAdGuard(w http.ResponseWriter, r *http.Request) {
	var request struct {
		URL            string  `json:"url"`
		Username       string  `json:"username"`
		Password       *string `json:"password"`
		TLSFingerprint string  `json:"tls_fingerprint"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	config := integrations.AdGuardConfig{URL: request.URL, Username: request.Username, TLSFingerprint: request.TLSFingerprint}
	if request.Password != nil {
		config.Password = *request.Password
	}
	if err := config.Normalize(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.lockSiteMutation(w, r) {
		return
	}
	defer s.siteMu.Unlock()
	before, err := s.Store.LoadAdGuardConfig(r.Context())
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "the previous integration configuration could not be decrypted; remove it before replacing it")
		return
	}
	if request.Password == nil {
		if before == nil || config.URL != before.URL || config.Username != before.Username || config.TLSFingerprint != before.TLSFingerprint {
			writeErr(w, http.StatusBadRequest, "explicitly provide the password (including an intentional empty value) when creating a connection or changing its server, username, or certificate pin")
			return
		}
		config.Password = before.Password
	}
	session, _ := sessionFrom(r.Context())
	if err := s.Store.SaveAdGuardConfig(r.Context(), config, session.username); err != nil {
		writeErr(w, http.StatusInternalServerError, "the integration configuration could not be encrypted, saved, or audited")
		return
	}
	writeJSON(w, http.StatusOK, viewAdGuard(&config))
}

func (s *Server) handleDeleteAdGuard(w http.ResponseWriter, r *http.Request) {
	if !s.lockSiteMutation(w, r) {
		return
	}
	defer s.siteMu.Unlock()
	session, _ := sessionFrom(r.Context())
	if err := s.Store.DeleteAdGuardConfig(r.Context(), session.username); err != nil {
		writeErr(w, http.StatusInternalServerError, "the integration configuration could not be removed or audited")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

func (s *Server) handleAdGuardCheck(w http.ResponseWriter, r *http.Request) {
	config, err := s.Store.LoadAdGuardConfig(r.Context())
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "the integration configuration could not be read or decrypted")
		return
	}
	if config == nil {
		writeErr(w, http.StatusConflict, "configure an AdGuard Home connection before checking it")
		return
	}
	if s.AdGuardChecker == nil {
		writeErr(w, http.StatusServiceUnavailable, "AdGuard Home checks are unavailable")
		return
	}
	writeJSON(w, http.StatusOK, s.AdGuardChecker.Check(r.Context(), *config))
}

func (s *Server) handleWireGuardCheck(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	device, err := s.Store.DeviceByID(r.Context(), id)
	if handleStoreErr(w, err, "device") {
		return
	}
	if !device.Adopted() {
		writeErr(w, http.StatusConflict, "complete device adoption before checking WireGuard")
		return
	}
	if s.WireGuard == nil {
		writeJSON(w, http.StatusOK, integrations.WireGuardUnavailable(id, "The router's read-only integration channel is unavailable. No installation or permission change was attempted."))
		return
	}
	result, err := s.WireGuard.ReadWireGuard(r.Context(), id)
	if err != nil {
		result = integrations.WireGuardUnavailable(id, "WireGuard evidence could not be read from the selected router.")
	}
	result.DeviceID = id
	writeJSON(w, http.StatusOK, result)
}
