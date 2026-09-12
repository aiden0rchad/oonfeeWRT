package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"image/png"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aiden0rchad/oonfeewrt/ui"
)

func TestEmbeddedUIAppAssetsAndCaching(t *testing.T) {
	files, err := ui.FS()
	if errors.Is(err, ui.ErrNotBuilt) {
		t.Skip("requires npm --prefix ui run build; covered by make check and release-build CI")
	}
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&Daemon{}).mountUI(mux)
	request := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		return response
	}
	for _, path := range []string{"/", "/accounts", "/manifest.webmanifest", "/sw.js", "/app-icon-192.png", "/app-icon-512.png"} {
		t.Run(path, func(t *testing.T) {
			response := request(path)
			if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-cache" {
				t.Fatalf("asset response: status=%d cache=%q", response.Code, response.Header().Get("Cache-Control"))
			}
		})
	}
	manifest := request("/manifest.webmanifest")
	if contentType := manifest.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/manifest+json") && !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("manifest served with an invalid content type: %q", contentType)
	}
	var app struct {
		ID       string `json:"id"`
		StartURL string `json:"start_url"`
		Scope    string `json:"scope"`
		Display  string `json:"display"`
		Icons    []struct {
			Source  string `json:"src"`
			Sizes   string `json:"sizes"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	if err := json.Unmarshal(manifest.Body.Bytes(), &app); err != nil {
		t.Fatalf("manifest route returned non-JSON: %v", err)
	}
	if app.ID != "/" || app.StartURL != "/" || app.Scope != "/" || app.Display != "standalone" || len(app.Icons) != 2 {
		t.Fatalf("wrong controller app manifest: %+v", app)
	}
	for _, icon := range app.Icons {
		response := request("/" + icon.Source)
		config, err := png.DecodeConfig(bytes.NewReader(response.Body.Bytes()))
		if err != nil {
			t.Fatalf("manifest icon is not a served PNG: %v", err)
		}
		want := 192
		if icon.Sizes == "512x512" {
			want = 512
		} else if icon.Sizes != "192x192" {
			t.Fatalf("unexpected icon size %q", icon.Sizes)
		}
		if config.Width != want || config.Height != want || response.Header().Get("Content-Type") != "image/png" || !strings.Contains(icon.Purpose, "maskable") {
			t.Fatalf("manifest icon dimensions/type/purpose mismatch: %+v", icon)
		}
	}
	worker := request("/sw.js")
	if !strings.Contains(worker.Header().Get("Content-Type"), "javascript") || !strings.Contains(worker.Body.String(), "self.addEventListener('fetch'") {
		t.Fatal("service worker route returned the SPA or a non-JavaScript response")
	}
	assets, err := fs.Glob(files, "assets/*.js")
	if err != nil || len(assets) == 0 {
		t.Fatalf("embedded UI has no JavaScript build assets: %v", err)
	}
	if response := request("/" + assets[0]); response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatal("hashed assets lost immutable caching")
	}
	if response := request("/api/v1/does-not-exist"); response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), "<html") {
		t.Fatal("unknown API route fell back to the SPA")
	}
}
