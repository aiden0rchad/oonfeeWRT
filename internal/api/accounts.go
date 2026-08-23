package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

type accountDTO struct {
	ID                 int64             `json:"id"`
	Username           string            `json:"username"`
	Role               store.AccountRole `json:"role"`
	RoleLabel          string            `json:"role_label"`
	Enabled            bool              `json:"enabled"`
	CreatedAt          int64             `json:"created_at"`
	LastLoginAt        *int64            `json:"last_login_at"`
	ActiveSessionCount int               `json:"active_session_count"`
}

type sessionDTO struct {
	ID          string `json:"id"`
	Current     bool   `json:"current"`
	CreatedAt   int64  `json:"created_at"`
	LastSeenAt  int64  `json:"last_seen_at"`
	ExpiresAt   int64  `json:"expires_at"`
	PeerAddress string `json:"peer_address"`
}

type roleOption struct {
	Value       store.AccountRole `json:"value"`
	Label       string            `json:"label"`
	Description string            `json:"description"`
}

func roleLabel(role store.AccountRole) string {
	switch role {
	case store.RoleOwner:
		return "Owner"
	case store.RoleAdmin:
		return "Administrator"
	case store.RoleOperator:
		return "Operator"
	case store.RoleViewer:
		return "Read-only"
	default:
		return "Unknown"
	}
}

func roleOptions() []roleOption {
	return []roleOption{
		{store.RoleOwner, "Owner", "Full controller access, including account management."},
		{store.RoleAdmin, "Administrator", "Configure the controller, networks, and devices; cannot manage accounts."},
		{store.RoleOperator, "Operator", "Run approved operational actions and tests; cannot change configuration or accounts."},
		{store.RoleViewer, "Read-only", "View controller state only."},
	}
}

func accountResponse(admin *store.Admin, activeSessions int) accountDTO {
	return accountDTO{
		ID: admin.ID, Username: admin.Username, Role: admin.Role,
		RoleLabel: roleLabel(admin.Role), Enabled: admin.Enabled,
		CreatedAt: admin.CreatedAt, LastLoginAt: admin.LastLogin,
		ActiveSessionCount: activeSessions,
	}
}

func sessionResponses(records []sessionRecord) []sessionDTO {
	out := make([]sessionDTO, 0, len(records))
	for _, record := range records {
		out = append(out, sessionDTO{
			ID: record.ID, Current: record.Current, CreatedAt: record.Created.Unix(),
			LastSeenAt: record.LastSeen.Unix(), ExpiresAt: record.Expires.Unix(),
			PeerAddress: record.PeerAddress,
		})
	}
	return out
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	admin, ok := s.currentAccount(w, r, sess)
	if !ok {
		return
	}
	counts := s.sessions.counts(s.now())
	writeJSON(w, http.StatusOK, map[string]any{"account": accountResponse(admin, counts[admin.ID])})
}

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	admins, err := s.Store.Admins(r.Context())
	if err != nil {
		writeCodedErr(w, http.StatusInternalServerError, "account_operation_failed",
			"could not list accounts")
		return
	}
	counts := s.sessions.counts(s.now())
	accounts := make([]accountDTO, 0, len(admins))
	for _, admin := range admins {
		accounts = append(accounts, accountResponse(admin, counts[admin.ID]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts, "roles": roleOptions()})
}

type createAccountRequest struct {
	Username string            `json:"username"`
	Password string            `json:"password"`
	Role     store.AccountRole `json:"role"`
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	var req createAccountRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := store.ValidateAccountUsername(req.Username); err != nil {
		writeCodedErr(w, http.StatusBadRequest, "invalid_username", err.Error())
		return
	}
	if !req.Role.Valid() {
		writeCodedErr(w, http.StatusBadRequest, "invalid_role", "invalid account role")
		return
	}
	if err := validateNewPassword(req.Password); err != nil {
		writeCodedErr(w, http.StatusBadRequest, "weak_password", err.Error())
		return
	}
	hash, ok := s.hashNewPassword(w, req.Password)
	if !ok {
		return
	}
	sess, _ := sessionFrom(r.Context())
	admin, err := s.Store.CreateAdmin(r.Context(), req.Username, hash, req.Role,
		accountActor(sess, r))
	if err != nil {
		s.writeAccountStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"account": accountResponse(admin, 0)})
}

type setAccountRoleRequest struct {
	Role store.AccountRole `json:"role"`
}

func (s *Server) handleSetAccountRole(w http.ResponseWriter, r *http.Request) {
	id, ok := accountPathID(w, r)
	if !ok {
		return
	}
	var req setAccountRoleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !req.Role.Valid() {
		writeCodedErr(w, http.StatusBadRequest, "invalid_role", "invalid account role")
		return
	}
	sess, _ := sessionFrom(r.Context())
	admin, err := s.Store.SetAdminRole(r.Context(), id, req.Role, accountActor(sess, r))
	if err != nil {
		s.writeAccountStoreError(w, r, err)
		return
	}
	dropped := s.sessions.dropAdmin(id)
	signedOut := revokedCurrent(dropped, sess.id)
	if signedOut {
		s.clearSessionCookies(w, r)
	}
	s.auditSessionRevocation(r, "auth.sessions_revoked", sess, id, admin.Username,
		"owner", dropped)
	writeJSON(w, http.StatusOK, map[string]any{
		"account": accountResponse(admin, 0), "revoked_sessions": len(dropped),
		"signed_out": signedOut,
	})
}

type setAccountEnabledRequest struct {
	Enabled bool `json:"enabled"`
}

func (s *Server) handleSetAccountEnabled(w http.ResponseWriter, r *http.Request) {
	id, ok := accountPathID(w, r)
	if !ok {
		return
	}
	var req setAccountEnabledRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	admin, err := s.Store.SetAdminEnabled(r.Context(), id, req.Enabled, accountActor(sess, r))
	if err != nil {
		s.writeAccountStoreError(w, r, err)
		return
	}
	var dropped []sessionRecord
	if !req.Enabled {
		dropped = s.sessions.dropAdmin(id)
	}
	signedOut := revokedCurrent(dropped, sess.id)
	if signedOut {
		s.clearSessionCookies(w, r)
	}
	if len(dropped) > 0 {
		s.auditSessionRevocation(r, "auth.sessions_revoked", sess, id,
			admin.Username, "owner", dropped)
	}
	counts := s.sessions.counts(s.now())
	writeJSON(w, http.StatusOK, map[string]any{
		"account": accountResponse(admin, counts[id]), "revoked_sessions": len(dropped),
		"signed_out": signedOut,
	})
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := accountPathID(w, r)
	if !ok {
		return
	}
	target, err := s.Store.AdminByID(r.Context(), id)
	if err != nil {
		s.writeAccountStoreError(w, r, err)
		return
	}
	sess, _ := sessionFrom(r.Context())
	if err := s.Store.DeleteAdmin(r.Context(), id, accountActor(sess, r)); err != nil {
		s.writeAccountStoreError(w, r, err)
		return
	}
	dropped := s.sessions.dropAdmin(id)
	signedOut := revokedCurrent(dropped, sess.id)
	if signedOut {
		s.clearSessionCookies(w, r)
	}
	s.auditSessionRevocation(r, "auth.sessions_revoked", sess, id,
		target.Username, "owner", dropped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "revoked_sessions": len(dropped), "signed_out": signedOut,
	})
}

type resetAccountPasswordRequest struct {
	NewPassword string `json:"new_password"`
}

func (s *Server) handleResetAccountPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := accountPathID(w, r)
	if !ok {
		return
	}
	var req resetAccountPasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := validateNewPassword(req.NewPassword); err != nil {
		writeCodedErr(w, http.StatusBadRequest, "weak_password", err.Error())
		return
	}
	target, err := s.Store.AdminByID(r.Context(), id)
	if err != nil {
		s.writeAccountStoreError(w, r, err)
		return
	}
	hash, ok := s.hashNewPassword(w, req.NewPassword)
	if !ok {
		return
	}
	sess, _ := sessionFrom(r.Context())
	if err := s.Store.ResetAdminPassword(r.Context(), id, hash, accountActor(sess, r)); err != nil {
		s.writeAccountStoreError(w, r, err)
		return
	}
	dropped := s.sessions.dropAdmin(id)
	signedOut := revokedCurrent(dropped, sess.id)
	if signedOut {
		s.clearSessionCookies(w, r)
	}
	s.auditSessionRevocation(r, "auth.sessions_revoked", sess, id,
		target.Username, "owner", dropped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "revoked_sessions": len(dropped), "signed_out": signedOut,
	})
}

func (s *Server) handleAccountSessions(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessionResponses(
		s.sessions.list(sess.adminID, sess.id, s.now()))})
}

func (s *Server) handleAdminSessions(w http.ResponseWriter, r *http.Request) {
	id, ok := accountPathID(w, r)
	if !ok {
		return
	}
	if _, err := s.Store.AdminByID(r.Context(), id); err != nil {
		s.writeAccountStoreError(w, r, err)
		return
	}
	sess, _ := sessionFrom(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessionResponses(
		s.sessions.list(id, sess.id, s.now()))})
}

func (s *Server) handleRevokeOwnSession(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	s.handleRevokeSession(w, r, sess.adminID, sess.username, "self")
}

func (s *Server) handleRevokeAdminSession(w http.ResponseWriter, r *http.Request) {
	id, ok := accountPathID(w, r)
	if !ok {
		return
	}
	target, err := s.Store.AdminByID(r.Context(), id)
	if err != nil {
		s.writeAccountStoreError(w, r, err)
		return
	}
	s.handleRevokeSession(w, r, id, target.Username, "owner")
}

func (s *Server) handleRevokeSession(w http.ResponseWriter, r *http.Request,
	adminID int64, username, scope string) {
	managementID := r.PathValue("session_id")
	if managementID == "" || len(managementID) > 128 {
		writeCodedErr(w, http.StatusNotFound, "session_not_found", "session not found")
		return
	}
	dropped, ok := s.sessions.dropID(adminID, managementID)
	if !ok {
		writeCodedErr(w, http.StatusNotFound, "session_not_found", "session not found")
		return
	}
	sess, _ := sessionFrom(r.Context())
	signedOut := dropped.ID == sess.id
	if signedOut {
		s.clearSessionCookies(w, r)
	}
	s.auditSessionRevocation(r, "auth.session_revoked", sess, adminID,
		username, scope, []sessionRecord{dropped})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "revoked": 1, "signed_out": signedOut,
	})
}

func (s *Server) handleRevokeAdminSessions(w http.ResponseWriter, r *http.Request) {
	id, ok := accountPathID(w, r)
	if !ok {
		return
	}
	target, err := s.Store.AdminByID(r.Context(), id)
	if err != nil {
		s.writeAccountStoreError(w, r, err)
		return
	}
	sess, _ := sessionFrom(r.Context())
	dropped := s.sessions.dropAdmin(id)
	signedOut := revokedCurrent(dropped, sess.id)
	if signedOut {
		s.clearSessionCookies(w, r)
	}
	s.auditSessionRevocation(r, "auth.sessions_revoked", sess, id,
		target.Username, "owner", dropped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "revoked": len(dropped), "signed_out": signedOut,
	})
}

type reauthRequest struct {
	Password string `json:"password"`
}

func (s *Server) handleReauth(w http.ResponseWriter, r *http.Request) {
	var req reauthRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Password) > 1024 {
		writeCodedErr(w, http.StatusBadRequest, "invalid_request", "password is too long")
		return
	}
	sess, _ := sessionFrom(r.Context())
	now := s.now()
	allowed, wait, live := s.sessions.allowCredentialAttempt(sess, now)
	if !live {
		s.clearSessionCookies(w, r)
		writeCodedErr(w, http.StatusUnauthorized, "not_signed_in", "session expired")
		return
	}
	if !allowed {
		w.Header().Set("Retry-After", itoa(int(wait.Seconds())+1))
		writeCodedErr(w, http.StatusTooManyRequests, "too_many_attempts",
			"too many password attempts; try again shortly")
		return
	}
	admin, err := s.Store.AdminByName(r.Context(), sess.username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.sessions.dropAdmin(sess.adminID)
			s.clearSessionCookies(w, r)
			writeCodedErr(w, http.StatusUnauthorized, "not_signed_in", "account is unavailable")
			return
		}
		writeCodedErr(w, http.StatusInternalServerError, "account_operation_failed",
			"could not read the account")
		return
	}
	var verified bool
	if !s.withHashSlot(func() { verified = s.verifyPassword(admin, req.Password) }) {
		w.Header().Set("Retry-After", "2")
		writeCodedErr(w, http.StatusServiceUnavailable, "password_hash_capacity",
			"busy; try again shortly")
		return
	}
	if !verified {
		s.sessions.failCredentialAttempt(sess, now)
		s.auditReauthentication(r, "auth.reauthentication_failed", "warning", sess)
		writeCodedErr(w, http.StatusUnauthorized, "incorrect_password", "password is incorrect")
		return
	}
	until, live := s.sessions.succeedCredentialAttempt(sess, now, true)
	if !live {
		s.clearSessionCookies(w, r)
		writeCodedErr(w, http.StatusUnauthorized, "not_signed_in", "session expired")
		return
	}
	s.auditReauthentication(r, "auth.reauthenticated", "info", sess)
	writeJSON(w, http.StatusOK, map[string]any{"reauthenticated_until": until.Unix()})
}

func (s *Server) hashNewPassword(w http.ResponseWriter, password string) (string, bool) {
	var hash string
	var hashErr error
	if !s.withHashSlot(func() {
		hash, hashErr = secrets.HashPassword([]byte(password), secrets.DefaultParams())
	}) {
		w.Header().Set("Retry-After", "2")
		writeCodedErr(w, http.StatusServiceUnavailable, "password_hash_capacity",
			"busy; try again shortly")
		return "", false
	}
	if hashErr != nil {
		writeCodedErr(w, http.StatusInternalServerError, "account_operation_failed",
			"could not hash the password")
		return "", false
	}
	return hash, true
}

func (s *Server) currentAccount(w http.ResponseWriter, r *http.Request,
	sess *session) (*store.Admin, bool) {
	admin, err := s.Store.AdminByID(r.Context(), sess.adminID)
	if errors.Is(err, store.ErrNotFound) || err == nil && !admin.Enabled {
		s.sessions.dropAdmin(sess.adminID)
		s.clearSessionCookies(w, r)
		writeCodedErr(w, http.StatusUnauthorized, "not_signed_in", "account is unavailable")
		return nil, false
	}
	if err != nil {
		writeCodedErr(w, http.StatusInternalServerError, "account_operation_failed",
			"could not read the account")
		return nil, false
	}
	return admin, true
}

func accountActor(sess *session, r *http.Request) store.AccountActor {
	return store.AccountActor{AdminID: sess.adminID, Username: sess.username, Address: clientAddr(r)}
}

func accountPathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeCodedErr(w, http.StatusNotFound, "account_not_found", "account not found")
		return 0, false
	}
	return id, true
}

func revokedCurrent(records []sessionRecord, currentID string) bool {
	for _, record := range records {
		if record.ID == currentID {
			return true
		}
	}
	return false
}

func (s *Server) writeAccountStoreError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrInvalidUsername):
		writeCodedErr(w, http.StatusBadRequest, "invalid_username", err.Error())
	case errors.Is(err, store.ErrInvalidRole):
		writeCodedErr(w, http.StatusBadRequest, "invalid_role", "invalid account role")
	case errors.Is(err, store.ErrAccountExists):
		writeCodedErr(w, http.StatusConflict, "username_unavailable",
			"username is already in use or reserved")
	case errors.Is(err, store.ErrLastOwner):
		writeCodedErr(w, http.StatusConflict, "last_owner",
			"the last enabled owner cannot be disabled, demoted, or deleted")
	case errors.Is(err, store.ErrAccountActorInactive):
		if sess, ok := sessionFrom(r.Context()); ok {
			s.sessions.dropAdmin(sess.adminID)
		}
		s.clearSessionCookies(w, r)
		writeCodedErr(w, http.StatusUnauthorized, "not_signed_in", "account is unavailable")
	case errors.Is(err, store.ErrAccountActorForbidden):
		writeCodedErr(w, http.StatusForbidden, "insufficient_role",
			"account management requires an owner")
	case errors.Is(err, store.ErrNotFound):
		writeCodedErr(w, http.StatusNotFound, "account_not_found", "account not found")
	default:
		writeCodedErr(w, http.StatusInternalServerError, "account_operation_failed",
			"could not update the account")
	}
}

func (s *Server) auditReauthentication(r *http.Request, event, severity string, sess *session) {
	_ = s.Store.LogEvent(r.Context(), store.Event{
		Category: "audit", Severity: severity, Event: event,
		Detail: map[string]any{
			"actor_admin_id": sess.adminID, "actor_username": sess.username,
			"addr": clientAddr(r),
		},
	})
}

const sessionAuditTimeout = 2 * time.Second

func (s *Server) auditSessionRevocation(r *http.Request, event string, actor *session,
	targetAdminID int64, targetUsername, scope string, records []sessionRecord) {
	current := false
	if actor != nil {
		current = revokedCurrent(records, actor.id)
	}
	detail := map[string]any{
		"target_admin_id": targetAdminID, "target_username": targetUsername,
		"scope": scope, "revoked_count": len(records), "current": current,
		"addr": clientAddr(r),
	}
	if actor != nil {
		detail["actor_admin_id"] = actor.adminID
		detail["actor_username"] = actor.username
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), sessionAuditTimeout)
	defer cancel()
	_ = s.Store.LogEvent(ctx, store.Event{
		Category: "audit", Severity: "info", Event: event, Detail: detail,
	})
}
