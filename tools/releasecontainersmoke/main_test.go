package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testOwnerPassword    = "placeholder-owner-password"
	testViewerPassword   = "placeholder-viewer-password"
	testExportPassphrase = "placeholder-export-passphrase"
	testRuntimePass      = "placeholder-runtime-passphrase"
)

func testConfig(base string) *config {
	return &config{
		BaseURL: base + "/api/v1", OwnerUsername: "release-owner",
		OwnerPassword: secretText(testOwnerPassword), ViewerUsername: "post-backup",
		ViewerPassword: secretText(testViewerPassword), ExportPassphrase: secretText(testExportPassphrase),
		RuntimePassphrase: secretText(testRuntimePass),
	}
}

func TestLoadConfigRequiresPrivateRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"base_url":"http://127.0.0.1:18082/api/v1","owner_username":"release-owner","owner_password":"placeholder-owner-password","viewer_username":"post-backup","viewer_password":"placeholder-viewer-password","export_passphrase":"placeholder-export-passphrase","runtime_passphrase":"placeholder-runtime-passphrase"}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("group-readable config was accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.clear()
	if len(cfg.OwnerPassword)+len(cfg.ViewerPassword)+len(cfg.ExportPassphrase)+len(cfg.RuntimePassphrase) != 0 {
		t.Fatal("config secrets were not cleared")
	}
	link := filepath.Join(t.TempDir(), "config-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(link); err == nil {
		t.Fatal("symlink config was accepted")
	}
}

func TestRunSmokeSequence(t *testing.T) {
	artifact := []byte("encrypted-portable-backup")
	digest := sha256.Sum256(artifact)
	backupSHA := hex.EncodeToString(digest[:])
	backupID := strings.Repeat("A", 43)
	uploadID := strings.Repeat("B", 43)
	previewID := strings.Repeat("C", 43)
	intentID := strings.Repeat("1", 32)
	planID := restoreConfirmationContract + "." + strings.Repeat("a", 64)

	var mu sync.Mutex
	setup, restored, viewerCreated, loggedIn := false, false, false, false
	requests := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		key := r.Method + " " + r.URL.Path
		requests[key]++
		write := func(status int, value any) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path != "/healthz" {
				instance := strings.Repeat("D", 43)
				if restored {
					instance = strings.Repeat("E", 43)
				}
				w.Header().Set("X-OonfeeWRT-Instance", instance)
			}
			w.WriteHeader(status)
			if err := json.NewEncoder(w).Encode(value); err != nil {
				t.Errorf("encode response: %v", err)
			}
		}
		decode := func(target any) bool {
			defer r.Body.Close()
			if err := json.NewDecoder(r.Body).Decode(target); err != nil {
				t.Errorf("decode %s: %v", key, err)
				write(http.StatusBadRequest, map[string]string{"code": "invalid_test_request"})
				return false
			}
			return true
		}
		if r.URL.Path == "/healthz" {
			write(http.StatusOK, map[string]bool{"ok": true})
			return
		}
		if r.URL.Path == "/api/v1/setup" && r.Method == http.MethodGet {
			write(http.StatusOK, map[string]bool{"needs_setup": !setup})
			return
		}
		if r.URL.Path == "/api/v1/setup" && r.Method == http.MethodPost {
			var body map[string]string
			if !decode(&body) {
				return
			}
			if body["username"] != "release-owner" || body["password"] != testOwnerPassword {
				t.Error("setup credentials changed")
			}
			setup = true
			http.SetCookie(w, &http.Cookie{Name: "oonfee_session", Value: "before", Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: "oonfee_csrf", Value: "csrf-before", Path: "/"})
			write(http.StatusCreated, map[string]string{"csrf": "csrf-before"})
			return
		}
		if r.URL.Path == "/api/v1/login" && r.Method == http.MethodPost {
			var body map[string]string
			if !decode(&body) {
				return
			}
			if !restored || body["username"] != "release-owner" || body["password"] != testOwnerPassword {
				t.Error("restored login contract changed")
			}
			loggedIn = true
			http.SetCookie(w, &http.Cookie{Name: "oonfee_session", Value: "after", Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: "oonfee_csrf", Value: "csrf-after", Path: "/"})
			write(http.StatusOK, map[string]string{"csrf": "csrf-after"})
			return
		}

		wantSession, wantCSRF := "before", "csrf-before"
		if restored {
			wantSession, wantCSRF = "after", "csrf-after"
		}
		cookie, err := r.Cookie("oonfee_session")
		if err != nil || cookie.Value != wantSession || restored && !loggedIn {
			t.Errorf("%s missing restored session", key)
		}
		if r.Method != http.MethodGet && r.Header.Get("X-Oonfee-CSRF") != wantCSRF {
			t.Errorf("%s missing CSRF binding", key)
		}

		switch key {
		case "GET /api/v1/devices":
			write(http.StatusOK, map[string]any{"devices": []any{}})
		case "POST /api/v1/session/reauth":
			var body map[string]string
			if decode(&body) && body["password"] != testOwnerPassword {
				t.Error("reauth password changed")
			}
			write(http.StatusOK, map[string]bool{"ok": true})
		case "GET /api/v1/backups":
			write(http.StatusOK, map[string]any{
				"descriptor": map[string]any{"plan_id": backupPlanID, "format": "oonfeewrt-portable-backup", "format_version": 1, "file_extension": ".oowrtbak", "encryption": "argon2id+xchacha20poly1305"},
				"disclosure": map[string]any{"router_management_calls": false, "router_changes": false, "automatic_router_apply": false, "separate_export_passphrase": true, "export_passphrase_recoverable": false},
				"limits":     map[string]any{"max_artifact_bytes": 1 << 20},
			})
		case "POST /api/v1/backups":
			var body map[string]any
			if decode(&body) && (body["plan_id"] != backupPlanID || body["export_passphrase"] != testExportPassphrase || body["confirm_export_passphrase"] != testExportPassphrase || body["acknowledge_sensitive_content"] != true) {
				t.Error("backup request lost descriptor or passphrase binding")
			}
			write(http.StatusAccepted, map[string]any{"job": map[string]any{"id": backupID, "state": "queued"}})
		case "GET /api/v1/backups/" + backupID:
			write(http.StatusOK, map[string]any{"job": map[string]any{"id": backupID, "state": "completed", "size_bytes": len(artifact), "sha256": backupSHA}})
		case "GET /api/v1/backups/" + backupID + "/download":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", strconv.Itoa(len(artifact)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(artifact)
		case "POST /api/v1/accounts":
			var body map[string]string
			if decode(&body) && (body["username"] != "post-backup" || body["password"] != testViewerPassword || body["role"] != "viewer") {
				t.Error("viewer creation changed")
			}
			viewerCreated = true
			write(http.StatusCreated, map[string]any{"account": map[string]any{"username": "post-backup", "role": "viewer", "enabled": true}})
		case "GET /api/v1/accounts":
			accounts := []map[string]any{{"username": "release-owner", "role": "owner", "enabled": true}}
			if !restored && viewerCreated {
				accounts = append(accounts, map[string]any{"username": "post-backup", "role": "viewer", "enabled": true})
			}
			write(http.StatusOK, map[string]any{"accounts": accounts})
		case "GET /api/v1/restores":
			write(http.StatusOK, map[string]any{
				"descriptor": map[string]any{"upload_content_type": restoreMediaType, "confirmation_contract": restoreConfirmationContract, "typed_confirmation": restoreTypedConfirmation},
				"disclosure": map[string]any{"router_management_calls": false, "router_changes": false, "live_controller_changes": false, "automatic_router_apply": false},
			})
		case "POST /api/v1/restores/uploads":
			body, _ := io.ReadAll(r.Body)
			if r.Header.Get("Content-Type") != restoreMediaType || r.ContentLength != int64(len(artifact)) || !bytes.Equal(body, artifact) {
				t.Error("restore upload changed")
			}
			write(http.StatusCreated, map[string]any{"upload": map[string]string{"id": uploadID}})
		case "POST /api/v1/restores/previews":
			var body map[string]string
			if decode(&body) && (body["upload_id"] != uploadID || body["export_passphrase"] != testExportPassphrase) {
				t.Error("restore preview binding changed")
			}
			write(http.StatusAccepted, map[string]any{"preview": map[string]string{"id": previewID, "upload_id": uploadID, "state": "queued"}})
		case "GET /api/v1/restores/previews/" + previewID:
			write(http.StatusOK, map[string]any{"preview": map[string]any{"id": previewID, "upload_id": uploadID, "state": "completed", "plan_id": planID, "source_schema": 19, "target_schema": 19, "counts": map[string]int{"devices": 0}}})
		case "POST /api/v1/restores/previews/" + previewID + "/confirm":
			var body map[string]any
			if decode(&body) && (body["plan_id"] != planID || body["export_passphrase"] != testExportPassphrase || body["destination_runtime_passphrase"] != testRuntimePass || body["typed_confirmation"] != "RESTORE CONTROLLER" || body["acknowledge_restart"] != true || body["acknowledge_session_revocation"] != true || body["acknowledge_router_writes_suppressed"] != true || body["acknowledge_no_automatic_router_apply"] != true) {
				t.Error("restore confirmation binding changed")
			}
			restored = true
			loggedIn = false
			write(http.StatusAccepted, map[string]any{"intent": map[string]string{"id": intentID, "state": "accepted"}})
		case "GET /api/v1/restores/suppression":
			write(http.StatusOK, map[string]any{"suppression": map[string]any{"active": true, "restore_id": intentID}})
		default:
			t.Errorf("unexpected request %s", key)
			write(http.StatusNotFound, map[string]string{"code": "not_found"})
		}
	}))
	defer server.Close()

	cfg := testConfig(server.URL)
	defer cfg.clear()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runSmoke(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if requests["GET /api/v1/devices"] != 2 || requests["POST /api/v1/session/reauth"] != 4 ||
		requests["POST /api/v1/restores/previews/"+previewID+"/confirm"] != 1 {
		t.Fatalf("incomplete sequence: %#v", requests)
	}
}

func TestAPIErrorDoesNotEchoResponseBody(t *testing.T) {
	secret := "response-body-secret-must-not-leak"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"code":"failed","error":"`+secret+`"}`)
	}))
	defer server.Close()
	client, err := newAPIClient(server.URL + "/api/v1")
	if err != nil {
		t.Fatal(err)
	}
	err = client.requestJSON(context.Background(), http.MethodGet, "/devices", nil, nil, http.StatusOK)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe error: %v", err)
	}
}
