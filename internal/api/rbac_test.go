package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

func expectedProtectedRouteRoles() map[string]store.AccountRole {
	out := make(map[string]store.AccountRole)
	add := func(role store.AccountRole, patterns ...string) {
		for _, pattern := range patterns {
			out[pattern] = role
		}
	}
	add(store.RoleViewer,
		"POST /api/v1/logout",
		"GET /api/v1/session",
		"POST /api/v1/session/password",
		"POST /api/v1/session/reauth",
		"GET /api/v1/account",
		"GET /api/v1/account/sessions",
		"DELETE /api/v1/account/sessions/{session_id}",
		"GET /api/v1/devices",
		"GET /api/v1/devices/{id}",
		"GET /api/v1/devices/{id}/series",
		"GET /api/v1/devices/{id}/overhead",
		"POST /api/v1/devices/{id}/focus",
		"GET /api/v1/roaming/neighbours",
		"GET /api/v1/site/mesh-health",
		"GET /api/v1/site",
		"GET /api/v1/site/wlans/{id}",
		"GET /api/v1/site/meshes/{id}",
		"GET /api/v1/site/policies",
		"GET /api/v1/stats/{kind}",
		"GET /api/v1/clients",
		"GET /api/v1/clients/{mac}/observability",
		"GET /api/v1/events",
		"GET /api/v1/events/{id}",
		"GET /api/v1/topology",
		"GET /api/v1/topology/history",
		"GET /api/v1/radios",
		"GET /api/v1/dashboard",
		"GET /api/v1/speedtests",
		"GET /api/v1/speedtests/{id}",
		"GET /api/v1/live",
	)
	add(store.RoleOperator,
		"POST /api/v1/site/verify-on-air",
		"POST /api/v1/devices/{id}/radios/{radio}/scan",
		"POST /api/v1/speedtests",
		"POST /api/v1/speedtests/{id}/cancel",
	)
	add(store.RoleAdmin,
		"POST /api/v1/devices/{id}/poll-interval",
		"POST /api/v1/devices/{id}/name",
		"POST /api/v1/devices/adopt",
		"POST /api/v1/devices/inspect",
		"POST /api/v1/devices/{id}/unadopt",
		"POST /api/v1/devices/{id}/refresh-acl",
		"GET /api/v1/devices/{id}/capabilities/lldp",
		"POST /api/v1/devices/{id}/capabilities/lldp",
		"POST /api/v1/devices/{id}/reprobe",
		"POST /api/v1/devices/{id}/foreign/{section}/note",
		"POST /api/v1/roaming/neighbours",
		"POST /api/v1/site/name",
		"POST /api/v1/site/wlans",
		"POST /api/v1/site/wlans/{id}",
		"DELETE /api/v1/site/wlans/{id}",
		"POST /api/v1/site/meshes",
		"POST /api/v1/site/meshes/{id}",
		"DELETE /api/v1/site/meshes/{id}",
		"POST /api/v1/site/uplinks",
		"POST /api/v1/site/uplinks/{id}",
		"DELETE /api/v1/site/uplinks/{id}",
		"POST /api/v1/site/groups",
		"POST /api/v1/site/groups/{id}",
		"DELETE /api/v1/site/groups/{id}",
		"POST /api/v1/site/networks",
		"POST /api/v1/site/networks/{id}",
		"DELETE /api/v1/site/networks/{id}",
		"POST /api/v1/site/zones/{name}",
		"DELETE /api/v1/site/zones/{name}",
		"POST /api/v1/site/policies",
		"POST /api/v1/site/policies/{id}",
		"DELETE /api/v1/site/policies/{id}",
		"POST /api/v1/site/object-manager/compile",
		"POST /api/v1/clients/{mac}/policy",
		"POST /api/v1/site/devices/{id}/override",
		"GET /api/v1/site/preview",
		"POST /api/v1/site/apply",
		"GET /api/v1/site/apply/{operation_id}",
		"GET /api/v1/discovery",
		"POST /api/v1/discovery/scan",
		"GET /api/v1/diagnostics",
		"POST /api/v1/diagnostics",
		"GET /api/v1/diagnostics/{id}",
		"POST /api/v1/diagnostics/{id}/cancel",
		"GET /api/v1/diagnostics/{id}/download",
	)
	add(store.RoleOwner,
		"GET /api/v1/accounts",
		"GET /api/v1/backups",
		"GET /api/v1/backups/{id}",
		"POST /api/v1/accounts",
		"PATCH /api/v1/accounts/{id}/role",
		"PATCH /api/v1/accounts/{id}/enabled",
		"DELETE /api/v1/accounts/{id}",
		"POST /api/v1/accounts/{id}/password",
		"GET /api/v1/accounts/{id}/sessions",
		"DELETE /api/v1/accounts/{id}/sessions/{session_id}",
		"DELETE /api/v1/accounts/{id}/sessions",
		"POST /api/v1/backups",
		"POST /api/v1/backups/{id}/cancel",
		"GET /api/v1/backups/{id}/download",
		"GET /api/v1/restores",
		"POST /api/v1/restores/uploads",
		"POST /api/v1/restores/previews",
		"GET /api/v1/restores/previews/{id}",
		"POST /api/v1/restores/previews/{id}/cancel",
		"POST /api/v1/restores/previews/{id}/confirm",
		"GET /api/v1/restores/suppression",
		"POST /api/v1/restores/suppression/resume",
	)
	return out
}

func TestProtectedRoutesHaveExhaustiveRoleChecks(t *testing.T) {
	srv := &Server{}
	routes := append(srv.protectedRoutes(), srv.reauthenticatedRoutes()...)
	expected := expectedProtectedRouteRoles()
	if len(routes) != len(expected) || len(routes) != 101 {
		t.Fatalf("protected routes=%d expected=%d", len(routes), len(expected))
	}
	seen := make(map[string]bool, len(routes))
	roles := []store.AccountRole{
		store.RoleViewer, store.RoleOperator, store.RoleAdmin, store.RoleOwner, "invalid",
	}
	for _, route := range routes {
		wantRole, ok := expected[route.pattern]
		if !ok {
			t.Errorf("unreviewed protected route %q", route.pattern)
			continue
		}
		if seen[route.pattern] {
			t.Errorf("duplicate protected route %q", route.pattern)
		}
		seen[route.pattern] = true
		if route.role != wantRole {
			t.Errorf("%s role=%s want=%s", route.pattern, route.role, wantRole)
		}

		for _, actual := range roles {
			t.Run(fmt.Sprintf("%s/%s", route.pattern, actual), func(t *testing.T) {
				hit := false
				handler := srv.requireRole(route.role, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					hit = true
					w.WriteHeader(http.StatusNoContent)
				}))
				req := httptest.NewRequest(http.MethodGet, "/", nil)
				req = req.WithContext(context.WithValue(req.Context(), sessionCtxKey,
					&session{role: actual}))
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, req)
				allowed := roleAllows(actual, route.role)
				if hit != allowed {
					t.Fatalf("handler hit=%v want=%v status=%d", hit, allowed, w.Code)
				}
				if !allowed && w.Code != http.StatusForbidden {
					t.Fatalf("denial status=%d want=403", w.Code)
				}
			})
		}
	}
	for pattern := range expected {
		if !seen[pattern] {
			t.Errorf("protected route is not registered: %q", pattern)
		}
	}
}

func TestEveryOwnerSensitiveMutationRequiresRecentReauthentication(t *testing.T) {
	want := map[string]bool{
		"POST /api/v1/accounts":                              true,
		"PATCH /api/v1/accounts/{id}/role":                   true,
		"PATCH /api/v1/accounts/{id}/enabled":                true,
		"DELETE /api/v1/accounts/{id}":                       true,
		"POST /api/v1/accounts/{id}/password":                true,
		"DELETE /api/v1/accounts/{id}/sessions/{session_id}": true,
		"DELETE /api/v1/accounts/{id}/sessions":              true,
		"POST /api/v1/backups":                               true,
		"POST /api/v1/backups/{id}/cancel":                   true,
		"GET /api/v1/backups/{id}/download":                  true,
		"POST /api/v1/restores/uploads":                      true,
		"POST /api/v1/restores/previews":                     true,
		"POST /api/v1/restores/previews/{id}/cancel":         true,
		"POST /api/v1/restores/previews/{id}/confirm":        true,
		"POST /api/v1/restores/suppression/resume":           true,
	}
	routes := (&Server{}).reauthenticatedRoutes()
	if len(routes) != len(want) {
		t.Fatalf("reauthenticated routes=%d want=%d", len(routes), len(want))
	}
	for _, route := range routes {
		if !want[route.pattern] {
			t.Errorf("unreviewed reauthenticated route %q", route.pattern)
		}
		if route.role != store.RoleOwner {
			t.Errorf("%s role=%s want owner", route.pattern, route.role)
		}
		if method, _, _ := strings.Cut(route.pattern, " "); method == http.MethodGet &&
			route.pattern != "GET /api/v1/backups/{id}/download" {
			t.Errorf("read-only route unexpectedly requires reauthentication: %s", route.pattern)
		}
	}
}

func TestRoutesEnforceRepresentativeRoleBoundaries(t *testing.T) {
	h := newHarness(t)
	h.setup()
	device := h.seedDevice("rbac-ap", true, nil)
	owner, err := h.db.AdminByName(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, method, path string
		role               store.AccountRole
		forbidden          bool
	}{
		{"viewer read", http.MethodGet, "/api/v1/devices", store.RoleViewer, false},
		{"viewer focus", http.MethodPost, fmt.Sprintf("/api/v1/devices/%d/focus", device.ID), store.RoleViewer, false},
		{"viewer operator write", http.MethodPost, "/api/v1/speedtests", store.RoleViewer, true},
		{"viewer sensitive read", http.MethodGet, "/api/v1/discovery", store.RoleViewer, true},
		{"operator transient scan", http.MethodPost, fmt.Sprintf("/api/v1/devices/%d/radios/radio0/scan", device.ID), store.RoleOperator, false},
		{"operator admin write", http.MethodPost, "/api/v1/site/name", store.RoleOperator, true},
		{"admin sensitive read", http.MethodGet, "/api/v1/discovery", store.RoleAdmin, false},
		{"owner admin write", http.MethodPost, "/api/v1/site/name", store.RoleOwner, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			token, sess, err := h.srv.sessions.create(owner.ID, owner.Username, tc.role,
				"192.0.2.10", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			h.cookies = []*http.Cookie{{Name: sessionCookie, Value: token}}
			h.csrf = sess.csrf
			w := h.do(tc.method, tc.path, nil)
			if tc.forbidden && w.Code != http.StatusForbidden {
				t.Fatalf("status=%d want=403 body=%s", w.Code, w.Body.String())
			}
			if !tc.forbidden && w.Code == http.StatusForbidden {
				t.Fatalf("authorized route returned 403: %s", w.Body.String())
			}
		})
	}
}

func TestSessionResponsesExposeCanonicalRoleOnly(t *testing.T) {
	h := newHarness(t)
	w := h.do(http.MethodPost, "/api/v1/setup",
		map[string]string{"username": "owner", "password": testPassword})
	assertSessionRoleResponse(t, w, http.StatusCreated, store.RoleOwner)
	h.csrf, _ = h.json(w)["csrf"].(string)

	w = h.do(http.MethodGet, "/api/v1/session", nil)
	assertSessionRoleResponse(t, w, http.StatusOK, store.RoleOwner)

	owner, err := h.db.AdminByName(context.Background(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.db.CreateAdmin(context.Background(), "reader", owner.PassHash, store.RoleViewer,
		store.AccountActor{AdminID: owner.ID, Username: owner.Username})
	if err != nil {
		t.Fatal(err)
	}
	h.cookies, h.csrf = nil, ""
	w = h.do(http.MethodPost, "/api/v1/login",
		map[string]string{"username": "reader", "password": testPassword})
	assertSessionRoleResponse(t, w, http.StatusOK, store.RoleViewer)
	h.csrf, _ = h.json(w)["csrf"].(string)
	w = h.do(http.MethodGet, "/api/v1/session", nil)
	assertSessionRoleResponse(t, w, http.StatusOK, store.RoleViewer)
}

func assertSessionRoleResponse(t *testing.T, w *httptest.ResponseRecorder, status int,
	role store.AccountRole) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["role"] != string(role) || body["username"] == "" || body["csrf"] == "" {
		t.Fatalf("session response=%v", body)
	}
	if body["admin_id"] == nil || body["role_label"] == "" || body["reauthenticated_until"] == nil {
		t.Fatalf("session response lacks account assurance metadata: %v", body)
	}
	if len(body) != 6 {
		t.Fatalf("session response exposed unexpected metadata: %v", body)
	}
}

func TestEventAuthorizationSeparatesGeneralAndAudit(t *testing.T) {
	h := newHarness(t)
	h.setup()
	ctx := context.Background()
	owner, err := h.db.AdminByName(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range []struct {
		name string
		role store.AccountRole
	}{{"reader", store.RoleViewer}, {"operator", store.RoleOperator}, {"administrator", store.RoleAdmin}} {
		if _, err := h.db.CreateAdmin(ctx, account.name, owner.PassHash, account.role,
			store.AccountActor{AdminID: owner.ID, Username: owner.Username}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.db.LogEvent(ctx, store.Event{
		TS: 200, Category: "system", Severity: "info", Event: "visible.general",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.LogEvent(ctx, store.Event{
		TS: 201, Category: "audit", Severity: "warning", Event: "secret.audit",
	}); err != nil {
		t.Fatal(err)
	}
	var generalID, auditID int64
	if err := h.db.SQL().QueryRowContext(ctx,
		`SELECT id FROM events WHERE event='visible.general'`).Scan(&generalID); err != nil {
		t.Fatal(err)
	}
	if err := h.db.SQL().QueryRowContext(ctx,
		`SELECT id FROM events WHERE event='secret.audit'`).Scan(&auditID); err != nil {
		t.Fatal(err)
	}

	for _, account := range []struct {
		name       string
		adminLevel bool
	}{{"reader", false}, {"operator", false}, {"administrator", true}, {"admin", true}} {
		t.Run(account.name, func(t *testing.T) {
			loginAs(t, h, account.name)
			if w := h.do(http.MethodGet, "/api/v1/events?scope=general", nil); w.Code != http.StatusOK {
				t.Fatalf("general status=%d body=%s", w.Code, w.Body.String())
			}
			if w := h.do(http.MethodGet, fmt.Sprintf("/api/v1/events/%d", generalID), nil); w.Code != http.StatusOK {
				t.Fatalf("general detail status=%d body=%s", w.Code, w.Body.String())
			}
			want := http.StatusForbidden
			if account.adminLevel {
				want = http.StatusOK
			}
			for _, path := range []string{
				"/api/v1/events?scope=audit",
				"/api/v1/events",
				fmt.Sprintf("/api/v1/events/%d", auditID),
			} {
				if w := h.do(http.MethodGet, path, nil); w.Code != want {
					t.Errorf("%s status=%d want=%d body=%s", path, w.Code, want, w.Body.String())
				}
			}
		})
	}
}

func TestDashboardRecentEventsNeverIncludesAudit(t *testing.T) {
	h := newHarness(t)
	h.setup()
	ctx := context.Background()
	for _, event := range []store.Event{
		{TS: time.Now().Unix(), Category: "system", Severity: "info", Event: "dashboard.general"},
		{TS: time.Now().Unix() + 1, Category: "audit", Severity: "warning", Event: "dashboard.audit"},
	} {
		if err := h.db.LogEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	w := h.do(http.MethodGet, "/api/v1/dashboard", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Events []store.Event `json:"recent_events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	foundGeneral := false
	for _, event := range body.Events {
		if event.Category == "audit" {
			t.Fatalf("dashboard leaked audit event: %+v", event)
		}
		foundGeneral = foundGeneral || event.Event == "dashboard.general"
	}
	if !foundGeneral {
		t.Fatalf("dashboard omitted general event: %+v", body.Events)
	}
}

func TestSessionRevocationCancelsInFlightAuthenticatedRequest(t *testing.T) {
	h := newHarness(t)
	h.setup()
	owner, err := h.db.AdminByName(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	canceled := make(chan error, 1)
	served := make(chan struct{})
	handler := h.srv.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		canceled <- r.Context().Err()
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/blocking", nil)
	for _, cookie := range h.cookies {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	go func() {
		defer close(served)
		handler.ServeHTTP(w, req)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("authenticated handler did not start")
	}
	h.srv.sessions.dropAdmin(owner.ID)
	select {
	case err := <-canceled:
		if err != context.Canceled {
			t.Fatalf("request context error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("session revocation did not cancel the request context")
	}
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("authenticated handler did not return after cancellation")
	}
}

func loginAs(t *testing.T, h *harness, username string) {
	t.Helper()
	h.cookies, h.csrf = nil, ""
	w := h.do(http.MethodPost, "/api/v1/login",
		map[string]string{"username": username, "password": testPassword})
	if w.Code != http.StatusOK {
		t.Fatalf("login %s: status=%d body=%s", username, w.Code, w.Body.String())
	}
	h.csrf, _ = h.json(w)["csrf"].(string)
}
