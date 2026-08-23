package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

func TestSessionManagementMetadataDoesNotLeakCredentials(t *testing.T) {
	h := newHarness(t)
	now := time.Unix(1_700_000_000, 0)
	h.srv.Now = func() time.Time { return now }
	h.setup()
	bearer := cookieValue(h.cookies, sessionCookie)
	if bearer == "" {
		t.Fatal("setup did not issue a session cookie")
	}

	w := h.do(http.MethodGet, "/api/v1/account/sessions", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Sessions []sessionDTO `json:"sessions"`
	}
	decodeResponse(t, w, &body)
	if len(body.Sessions) != 1 {
		t.Fatalf("sessions=%+v", body.Sessions)
	}
	sess := body.Sessions[0]
	if !sess.Current || sess.ID == "" || sess.ID == bearer || sess.ID == h.csrf {
		t.Fatalf("unsafe session metadata: %+v", sess)
	}
	if sess.PeerAddress != "192.0.2.10" || sess.CreatedAt != now.Unix() ||
		sess.LastSeenAt != now.Unix() || sess.ExpiresAt != now.Add(sessionIdle).Unix() {
		t.Fatalf("session metadata=%+v", sess)
	}
	if strings.Contains(w.Body.String(), bearer) || strings.Contains(w.Body.String(), h.csrf) {
		t.Fatalf("session list leaked a credential: %s", w.Body.String())
	}

	w = h.do(http.MethodGet, "/api/v1/account", nil)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "pass_hash") {
		t.Fatalf("account response status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestClientAddressUsesOnlyNormalizedSocketPeer(t *testing.T) {
	for _, tc := range []struct {
		name, remote, want string
	}{
		{"IPv4", "192.0.2.44:1234", "192.0.2.44"},
		{"mapped IPv4", "[::ffff:192.0.2.44]:1234", "192.0.2.44"},
		{"IPv6", "[2001:db8::44]:1234", "2001:db8::44"},
		{"bare IP", "192.0.2.45", "192.0.2.45"},
		{"invalid port", "192.0.2.46:not-a-port", "unknown"},
		{"malformed", "attacker supplied text", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.remote
			r.Header.Set("Forwarded", "for=203.0.113.8")
			r.Header.Set("X-Forwarded-For", "203.0.113.9")
			if got := clientAddr(r); got != tc.want {
				t.Fatalf("clientAddr=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestOwnSessionRevocationIsAccountScopedAndAuditedWithoutIdentifiers(t *testing.T) {
	h := newHarness(t)
	h.setup()
	owner, viewer := seedAccount(t, h, "viewer", store.RoleViewer)
	ownerRecords := h.srv.sessions.list(owner.ID, "", h.srv.now())
	if len(ownerRecords) != 1 {
		t.Fatalf("owner sessions=%+v", ownerRecords)
	}
	token, viewerSession, err := h.srv.sessions.create(viewer.ID, viewer.Username,
		viewer.Role, "198.51.100.20", h.srv.now())
	if err != nil {
		t.Fatal(err)
	}
	h.cookies = []*http.Cookie{{Name: sessionCookie, Value: token}}
	h.csrf = viewerSession.csrf

	w := h.do(http.MethodDelete,
		"/api/v1/account/sessions/"+ownerRecords[0].ID, nil)
	assertCodedError(t, w, http.StatusNotFound, "session_not_found")
	if _, ok := h.srv.sessions.get(token, h.srv.now()); !ok {
		t.Fatal("cross-account revoke removed the caller session")
	}

	w = h.do(http.MethodDelete,
		"/api/v1/account/sessions/"+viewerSession.id, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("revoke status=%d body=%s", w.Code, w.Body.String())
	}
	if body := responseMap(t, w); body["signed_out"] != true || body["revoked"] != float64(1) {
		t.Fatalf("revoke response=%v", body)
	}
	if w := h.do(http.MethodGet, "/api/v1/account", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session status=%d body=%s", w.Code, w.Body.String())
	}

	var detail string
	if err := h.db.SQL().QueryRowContext(context.Background(), `SELECT detail_json FROM events
WHERE event='auth.session_revoked' ORDER BY id DESC LIMIT 1`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{token, viewerSession.id, viewerSession.csrf} {
		if strings.Contains(detail, forbidden) {
			t.Fatalf("session audit leaked %q: %s", forbidden, detail)
		}
	}
}

func TestReauthenticationWindowAndSharedPasswordThrottle(t *testing.T) {
	h := newHarness(t)
	now := time.Unix(1_700_000_000, 0)
	h.srv.Now = func() time.Time { return now }
	h.setup()
	owner, err := h.db.AdminByName(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	lowHash, err := secrets.HashPassword([]byte(testPassword),
		secrets.Params{Time: 1, MemoryKiB: 64, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetAdminPassword(context.Background(), owner.ID, lowHash); err != nil {
		t.Fatal(err)
	}

	now = now.Add(reauthValidity + time.Second)
	w := h.do(http.MethodPost, "/api/v1/accounts", map[string]any{
		"username": "blocked", "password": testPassword, "role": store.RoleViewer,
	})
	assertCodedError(t, w, http.StatusPreconditionRequired, "reauth_required")
	w = h.do(http.MethodPost, "/api/v1/session/reauth", map[string]string{"password": testPassword})
	if w.Code != http.StatusOK || responseMap(t, w)["reauthenticated_until"] != float64(now.Add(reauthValidity).Unix()) {
		t.Fatalf("reauth status=%d body=%s", w.Code, w.Body.String())
	}
	now = now.Add(reauthValidity)
	w = h.do(http.MethodPost, "/api/v1/accounts", map[string]any{
		"username": "still-blocked", "password": testPassword, "role": store.RoleViewer,
	})
	assertCodedError(t, w, http.StatusPreconditionRequired, "reauth_required")

	wrong := "definitely-the-wrong-password"
	for i := 0; i < credentialFailureLimit-1; i++ {
		w = h.do(http.MethodPost, "/api/v1/session/reauth", map[string]string{"password": wrong})
		assertCodedError(t, w, http.StatusUnauthorized, "incorrect_password")
	}
	w = h.do(http.MethodPost, "/api/v1/session/password", map[string]string{
		"current_password": wrong, "new_password": "another-long-password",
	})
	assertCodedError(t, w, http.StatusUnauthorized, "incorrect_password")
	w = h.do(http.MethodPost, "/api/v1/session/reauth", map[string]string{"password": wrong})
	assertCodedError(t, w, http.StatusTooManyRequests, "too_many_attempts")

	now = now.Add(credentialLockout + time.Second)
	h.srv.hashing <- struct{}{}
	h.srv.hashing <- struct{}{}
	w = h.do(http.MethodPost, "/api/v1/session/reauth", map[string]string{"password": wrong})
	<-h.srv.hashing
	<-h.srv.hashing
	assertCodedError(t, w, http.StatusServiceUnavailable, "password_hash_capacity")
	for i := 0; i < credentialFailureLimit; i++ {
		w = h.do(http.MethodPost, "/api/v1/session/reauth", map[string]string{"password": wrong})
		assertCodedError(t, w, http.StatusUnauthorized, "incorrect_password")
	}
	w = h.do(http.MethodPost, "/api/v1/session/reauth", map[string]string{"password": wrong})
	assertCodedError(t, w, http.StatusTooManyRequests, "too_many_attempts")

	var audits string
	rows, err := h.db.SQL().QueryContext(context.Background(), `SELECT detail_json FROM events
WHERE event IN ('auth.reauthenticated','auth.reauthentication_failed','auth.password_change_failed')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var detail string
		if err := rows.Scan(&detail); err != nil {
			t.Fatal(err)
		}
		audits += detail
	}
	if strings.Contains(audits, wrong) || strings.Contains(audits, testPassword) {
		t.Fatalf("credential audit leaked password: %s", audits)
	}
}

func TestAccountCreationListingAndOwnerBoundary(t *testing.T) {
	h := newHarness(t)
	h.setup()
	w := h.do(http.MethodPost, "/api/v1/accounts", map[string]any{
		"username": "reader", "password": testPassword, "role": store.RoleViewer,
	})
	if w.Code != http.StatusCreated || strings.Contains(w.Body.String(), testPassword) ||
		strings.Contains(w.Body.String(), "pass_hash") {
		t.Fatalf("create status=%d body=%s", w.Code, w.Body.String())
	}
	var created struct {
		Account accountDTO `json:"account"`
	}
	decodeResponse(t, w, &created)
	if created.Account.Username != "reader" || created.Account.RoleLabel != "Read-only" ||
		!created.Account.Enabled || created.Account.ActiveSessionCount != 0 {
		t.Fatalf("created account=%+v", created.Account)
	}

	w = h.do(http.MethodGet, "/api/v1/accounts", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", w.Code, w.Body.String())
	}
	var listed struct {
		Accounts []accountDTO `json:"accounts"`
		Roles    []roleOption `json:"roles"`
	}
	decodeResponse(t, w, &listed)
	if len(listed.Accounts) != 2 || len(listed.Roles) != 4 ||
		listed.Roles[0].Label != "Owner" || listed.Roles[3].Label != "Read-only" {
		t.Fatalf("account list=%+v roles=%+v", listed.Accounts, listed.Roles)
	}

	w = h.do(http.MethodPost, "/api/v1/accounts", map[string]any{
		"username": "READER", "password": testPassword, "role": store.RoleViewer,
	})
	assertCodedError(t, w, http.StatusConflict, "username_unavailable")
	for _, tc := range []struct {
		body map[string]any
		code string
	}{
		{map[string]any{"username": ".bad", "password": testPassword, "role": store.RoleViewer}, "invalid_username"},
		{map[string]any{"username": "valid", "password": testPassword, "role": "root"}, "invalid_role"},
		{map[string]any{"username": "valid", "password": "short", "role": store.RoleViewer}, "weak_password"},
	} {
		w = h.do(http.MethodPost, "/api/v1/accounts", tc.body)
		assertCodedError(t, w, http.StatusBadRequest, tc.code)
	}

	reader, err := h.db.AdminByName(context.Background(), "reader")
	if err != nil {
		t.Fatal(err)
	}
	token, sess, err := h.srv.sessions.create(reader.ID, reader.Username, reader.Role,
		"192.0.2.99", h.srv.now())
	if err != nil {
		t.Fatal(err)
	}
	h.cookies = []*http.Cookie{{Name: sessionCookie, Value: token}}
	h.csrf = sess.csrf
	w = h.do(http.MethodGet, "/api/v1/accounts", nil)
	assertCodedError(t, w, http.StatusForbidden, "insufficient_role")
}

func TestOwnerAccountMutationsCommitBeforeRevokingSessions(t *testing.T) {
	for _, tc := range []struct {
		name, method, suffix string
		body                 map[string]any
		verify               func(*testing.T, *harness, *store.Admin)
	}{
		{"role", http.MethodPatch, "/role", map[string]any{"role": store.RoleOperator},
			func(t *testing.T, h *harness, target *store.Admin) {
				got, err := h.db.AdminByID(context.Background(), target.ID)
				if err != nil || got.Role != store.RoleOperator {
					t.Fatalf("role account=%+v err=%v", got, err)
				}
			}},
		{"disable", http.MethodPatch, "/enabled", map[string]any{"enabled": false},
			func(t *testing.T, h *harness, target *store.Admin) {
				got, err := h.db.AdminByID(context.Background(), target.ID)
				if err != nil || got.Enabled {
					t.Fatalf("disabled account=%+v err=%v", got, err)
				}
			}},
		{"delete", http.MethodDelete, "", nil,
			func(t *testing.T, h *harness, target *store.Admin) {
				if _, err := h.db.AdminByID(context.Background(), target.ID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("deleted account error=%v", err)
				}
			}},
		{"reset password", http.MethodPost, "/password", map[string]any{"new_password": "replacement-password"},
			func(t *testing.T, h *harness, target *store.Admin) {
				got, err := h.db.AdminByID(context.Background(), target.ID)
				if err != nil || secrets.VerifyPassword([]byte("replacement-password"), got.PassHash) != nil {
					t.Fatalf("reset account=%+v err=%v", got, err)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.setup()
			_, target := seedAccount(t, h, "target", store.RoleViewer)
			token, _, err := h.srv.sessions.create(target.ID, target.Username, target.Role,
				"198.51.100.30", h.srv.now())
			if err != nil {
				t.Fatal(err)
			}
			w := h.do(tc.method, fmt.Sprintf("/api/v1/accounts/%d%s", target.ID, tc.suffix), tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if _, ok := h.srv.sessions.get(token, h.srv.now()); ok {
				t.Fatal("target session survived a committed security mutation")
			}
			tc.verify(t, h, target)
		})
	}
}

func TestFailedOwnerMutationPreservesTargetAndCurrentSessions(t *testing.T) {
	h := newHarness(t)
	h.setup()
	owner, target := seedAccount(t, h, "target", store.RoleViewer)
	targetToken, _, err := h.srv.sessions.create(target.ID, target.Username, target.Role,
		"198.51.100.31", h.srv.now())
	if err != nil {
		t.Fatal(err)
	}
	w := h.do(http.MethodPatch, fmt.Sprintf("/api/v1/accounts/%d/role", owner.ID),
		map[string]any{"role": store.RoleAdmin})
	assertCodedError(t, w, http.StatusConflict, "last_owner")
	if w := h.do(http.MethodGet, "/api/v1/session", nil); w.Code != http.StatusOK {
		t.Fatalf("last-owner failure revoked current session: %d %s", w.Code, w.Body.String())
	}

	if _, err := h.db.SQL().ExecContext(context.Background(), `CREATE TRIGGER reject_role_audit
BEFORE INSERT ON events WHEN NEW.event='auth.account_role_changed'
BEGIN SELECT RAISE(ABORT,'audit rejected'); END`); err != nil {
		t.Fatal(err)
	}
	w = h.do(http.MethodPatch, fmt.Sprintf("/api/v1/accounts/%d/role", target.ID),
		map[string]any{"role": store.RoleOperator})
	assertCodedError(t, w, http.StatusInternalServerError, "account_operation_failed")
	if _, ok := h.srv.sessions.get(targetToken, h.srv.now()); !ok {
		t.Fatal("failed store mutation revoked the target session")
	}
	got, err := h.db.AdminByID(context.Background(), target.ID)
	if err != nil || got.Role != store.RoleViewer {
		t.Fatalf("failed mutation changed target=%+v err=%v", got, err)
	}
}

func TestOwnerSessionAdministrationEnforcesTargetMatch(t *testing.T) {
	h := newHarness(t)
	h.setup()
	_, first := seedAccount(t, h, "first", store.RoleViewer)
	_, second := seedAccount(t, h, "second", store.RoleViewer)
	firstToken, firstSession, err := h.srv.sessions.create(first.ID, first.Username, first.Role,
		"198.51.100.41", h.srv.now())
	if err != nil {
		t.Fatal(err)
	}
	secondToken, secondSession, err := h.srv.sessions.create(second.ID, second.Username, second.Role,
		"198.51.100.42", h.srv.now())
	if err != nil {
		t.Fatal(err)
	}

	w := h.do(http.MethodGet, fmt.Sprintf("/api/v1/accounts/%d/sessions", first.ID), nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), firstSession.id) ||
		strings.Contains(w.Body.String(), secondSession.id) {
		t.Fatalf("target sessions status=%d body=%s", w.Code, w.Body.String())
	}
	w = h.do(http.MethodDelete,
		fmt.Sprintf("/api/v1/accounts/%d/sessions/%s", first.ID, secondSession.id), nil)
	assertCodedError(t, w, http.StatusNotFound, "session_not_found")
	if _, ok := h.srv.sessions.get(secondToken, h.srv.now()); !ok {
		t.Fatal("mismatched target revoke removed another account's session")
	}
	w = h.do(http.MethodDelete,
		fmt.Sprintf("/api/v1/accounts/%d/sessions/%s", first.ID, firstSession.id), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("single revoke status=%d body=%s", w.Code, w.Body.String())
	}
	if _, ok := h.srv.sessions.get(firstToken, h.srv.now()); ok {
		t.Fatal("target session survived single revoke")
	}
	w = h.do(http.MethodDelete, fmt.Sprintf("/api/v1/accounts/%d/sessions", second.ID), nil)
	if w.Code != http.StatusOK || responseMap(t, w)["revoked"] != float64(1) {
		t.Fatalf("bulk revoke status=%d body=%s", w.Code, w.Body.String())
	}
	if _, ok := h.srv.sessions.get(secondToken, h.srv.now()); ok {
		t.Fatal("target session survived bulk revoke")
	}
}

func TestHTTPBoundaryRequiresReauthForEveryOwnerMutation(t *testing.T) {
	h := newHarness(t)
	now := time.Unix(1_700_000_000, 0)
	h.srv.Now = func() time.Time { return now }
	h.setup()
	_, target := seedAccount(t, h, "target", store.RoleViewer)
	targetToken, targetSession, err := h.srv.sessions.create(target.ID, target.Username,
		target.Role, "198.51.100.50", now)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(reauthValidity + time.Second)
	for _, route := range h.srv.reauthenticatedRoutes() {
		method, path, _ := strings.Cut(route.pattern, " ")
		path = strings.ReplaceAll(path, "{id}", strconvFormat(target.ID))
		path = strings.ReplaceAll(path, "{session_id}", targetSession.id)
		w := h.do(method, path, nil)
		assertCodedError(t, w, http.StatusPreconditionRequired, "reauth_required")
	}
	if _, ok := h.srv.sessions.get(targetToken, now); !ok {
		t.Fatal("step-up rejection changed target sessions")
	}
	if got, err := h.db.AdminByID(context.Background(), target.ID); err != nil ||
		!got.Enabled || got.Role != store.RoleViewer {
		t.Fatalf("step-up rejection changed target=%+v err=%v", got, err)
	}
}

func TestInactiveOwnerStoreErrorRevokesActorButPreservesTarget(t *testing.T) {
	h := newHarness(t)
	h.setup()
	owner, target := seedAccount(t, h, "target", store.RoleViewer)
	targetToken, _, err := h.srv.sessions.create(target.ID, target.Username, target.Role,
		"198.51.100.60", h.srv.now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.SQL().ExecContext(context.Background(),
		`UPDATE admins SET enabled=0 WHERE id=?`, owner.ID); err != nil {
		t.Fatal(err)
	}
	w := h.do(http.MethodPatch, fmt.Sprintf("/api/v1/accounts/%d/role", target.ID),
		map[string]any{"role": store.RoleOperator})
	assertCodedError(t, w, http.StatusUnauthorized, "not_signed_in")
	if w := h.do(http.MethodGet, "/api/v1/session", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("inactive actor session survived: %d %s", w.Code, w.Body.String())
	}
	if _, ok := h.srv.sessions.get(targetToken, h.srv.now()); !ok {
		t.Fatal("inactive actor error revoked the target session")
	}
}

func TestLoginCannotOutrunAccountSecurityMutation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(context.Context, *harness, *store.Admin, *store.Admin) error
	}{
		{"disable", func(ctx context.Context, h *harness, owner, target *store.Admin) error {
			_, err := h.db.SetAdminEnabled(ctx, target.ID, false,
				store.AccountActor{AdminID: owner.ID, Username: owner.Username})
			return err
		}},
		{"role change", func(ctx context.Context, h *harness, owner, target *store.Admin) error {
			_, err := h.db.SetAdminRole(ctx, target.ID, store.RoleOperator,
				store.AccountActor{AdminID: owner.ID, Username: owner.Username})
			return err
		}},
		{"password reset", func(ctx context.Context, h *harness, owner, target *store.Admin) error {
			hash, err := secrets.HashPassword([]byte("replacement-password"),
				secrets.Params{Time: 1, MemoryKiB: 64, Threads: 1})
			if err != nil {
				return err
			}
			return h.db.ResetAdminPassword(ctx, target.ID, hash,
				store.AccountActor{AdminID: owner.ID, Username: owner.Username})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.setup()
			owner, target := seedAccount(t, h, "target", store.RoleViewer)
			h.cookies, h.csrf = nil, ""

			verified := make(chan struct{})
			resume := make(chan struct{})
			h.srv.afterLoginPasswordVerified = func() {
				close(verified)
				<-resume
			}
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				result <- h.do(http.MethodPost, "/api/v1/login", map[string]string{
					"username": target.Username, "password": testPassword,
				})
			}()
			<-verified
			if err := tc.mutate(context.Background(), h, owner, target); err != nil {
				close(resume)
				t.Fatal(err)
			}
			// Account handlers revoke only after the store commit. At this point
			// no target session exists, which is the race the login postcondition
			// must close.
			h.srv.sessions.dropAdmin(target.ID)
			close(resume)

			w := <-result
			if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "csrf") {
				t.Fatalf("stale login status=%d body=%s", w.Code, w.Body.String())
			}
			for _, cookie := range w.Result().Cookies() {
				if cookie.Name == sessionCookie && cookie.Value != "" {
					t.Fatalf("stale login issued a session cookie")
				}
			}
			if got := h.srv.sessions.counts(h.srv.now())[target.ID]; got != 0 {
				t.Fatalf("stale login left %d session(s)", got)
			}
			var successfulLogins int
			if err := h.db.SQL().QueryRowContext(context.Background(),
				`SELECT COUNT(*) FROM events WHERE event='auth.login'`).Scan(&successfulLogins); err != nil {
				t.Fatal(err)
			}
			if successfulLogins != 0 {
				t.Fatalf("stale login emitted %d success audit(s)", successfulLogins)
			}
			got, err := h.db.AdminByID(context.Background(), target.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.LastLogin != nil {
				t.Fatalf("stale login updated last_login=%d", *got.LastLogin)
			}
		})
	}
}

func seedAccount(t *testing.T, h *harness, username string,
	role store.AccountRole) (*store.Admin, *store.Admin) {
	t.Helper()
	owner, err := h.db.AdminByName(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := h.db.CreateAdmin(context.Background(), username, owner.PassHash, role,
		store.AccountActor{AdminID: owner.ID, Username: owner.Username})
	if err != nil {
		t.Fatal(err)
	}
	return owner, admin
}

func cookieValue(cookies []*http.Cookie, name string) string {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie.Value
		}
	}
	return ""
}

func decodeResponse(t *testing.T, w *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
}

func responseMap(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	decodeResponse(t, w, &out)
	return out
}

func assertCodedError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d want=%d body=%s", w.Code, status, w.Body.String())
	}
	if got := responseMap(t, w)["code"]; got != code {
		t.Fatalf("error code=%v want=%s body=%s", got, code, w.Body.String())
	}
}

func strconvFormat(id int64) string { return fmt.Sprintf("%d", id) }
