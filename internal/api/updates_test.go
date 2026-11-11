package api

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/store"
	"github.com/aiden0rchad/oonfeewrt/internal/updates"
)

type stubUpdateChecker struct {
	mu           sync.Mutex
	calls        int
	refreshCalls int
	versions     []string
	status       updates.Status
}

func (s *stubUpdateChecker) Check(_ context.Context, version string) updates.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.versions = append(s.versions, version)
	return s.status
}

func (s *stubUpdateChecker) Refresh(ctx context.Context, version string) updates.Status {
	s.mu.Lock()
	s.refreshCalls++
	s.mu.Unlock()
	return s.Check(ctx, version)
}

func (s *stubUpdateChecker) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func TestUpdatesRequireAuthenticationAndUseExecutableVersionForEveryRole(t *testing.T) {
	h := newHarness(t)
	checker := &stubUpdateChecker{status: updates.Status{
		State: updates.StateAvailable, CurrentVersion: "v1.2.3", LatestVersion: "1.2.4",
		LatestTag: "v1.2.4", ReleaseURL: updates.ReleaseOrigin + "v1.2.4", UpdateAvailable: true,
		Message: "A newer release is available.",
	}}
	h.srv.ControllerVersion = "v1.2.3-actual-executable"
	h.srv.UpdateChecker = checker
	h.mux = h.srv.Routes()

	if w := h.do(http.MethodGet, "/api/v1/updates", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d body=%s", w.Code, w.Body.String())
	}
	if checker.callCount() != 0 {
		t.Fatal("unauthenticated request reached update checker")
	}

	if w := h.do(http.MethodPost, "/api/v1/updates/check", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated manual status=%d", w.Code)
	}
	h.setup()
	owner, err := h.db.AdminByName(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	roles := []store.AccountRole{store.RoleViewer, store.RoleOperator, store.RoleAdmin, store.RoleOwner}
	for _, role := range roles {
		t.Run(string(role), func(t *testing.T) {
			token, session, err := h.srv.sessions.create(owner.ID, owner.Username, role,
				"192.0.2.10", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			h.cookies = []*http.Cookie{{Name: sessionCookie, Value: token}}
			h.csrf = session.csrf
			w := h.do(http.MethodGet, "/api/v1/updates", nil)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			body := h.json(w)
			if body["state"] != string(updates.StateAvailable) || body["update_available"] != true ||
				body["latest_tag"] != "v1.2.4" {
				t.Fatalf("body=%v", body)
			}
			before := checker.callCount()
			w = h.do(http.MethodPost, "/api/v1/updates/check", nil)
			allowed := role == store.RoleAdmin || role == store.RoleOwner
			if allowed {
				if w.Code != http.StatusOK || checker.callCount() != before+1 {
					t.Fatalf("manual check status=%d calls=%d", w.Code, checker.callCount())
				}
			} else if w.Code != http.StatusForbidden || checker.callCount() != before {
				t.Fatalf("forbidden manual check status=%d calls=%d", w.Code, checker.callCount())
			}
		})
	}
	if checker.callCount() != len(roles)+2 || checker.refreshCalls != 2 {
		t.Fatalf("checker calls=%d want=%d", checker.callCount(), len(roles)+2)
	}
	for _, version := range checker.versions {
		if version != "v1.2.3-actual-executable" {
			t.Fatalf("checker received %q", version)
		}
	}
}

func TestUpdatesReturnTypedUnavailableStatusWhenCheckerMissing(t *testing.T) {
	h := newHarness(t)
	h.srv.ControllerVersion = "v1.2.3"
	h.srv.UpdateChecker = nil
	h.mux = h.srv.Routes()
	h.setup()
	w := h.do(http.MethodGet, "/api/v1/updates", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := h.json(w)
	errorBody, ok := body["error"].(map[string]any)
	if body["state"] != string(updates.StateUnavailable) || body["update_available"] != false ||
		body["current_version"] != "v1.2.3" || !ok || errorBody["code"] != "checker_unavailable" {
		t.Fatalf("body=%v", body)
	}
}

func TestUpdateInstallKindNormalisationAndMissingManualChecker(t *testing.T) {
	h := newHarness(t)
	h.setup()
	for _, checker := range []UpdateChecker{nil, &stubUpdateChecker{status: updates.Status{State: updates.StateUnknown}}} {
		h.srv.UpdateChecker = checker
		for _, kind := range []string{"container", "binary", "unknown", "", "CONTAINER", "environment-value"} {
			h.srv.ControllerInstallKind = kind
			want := kind
			if want != "container" && want != "binary" {
				want = "unknown"
			}
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				path := "/api/v1/updates"
				if method == http.MethodPost {
					path += "/check"
				}
				w := h.do(method, path, nil)
				body := h.json(w)
				if w.Code != http.StatusOK || body["install_kind"] != want {
					t.Fatalf("%s kind=%q status=%d body=%v", method, kind, w.Code, body)
				}
				if checker == nil && body["error"].(map[string]any)["code"] != "checker_unavailable" {
					t.Fatalf("missing checker error=%v", body)
				}
			}
		}
	}
}
