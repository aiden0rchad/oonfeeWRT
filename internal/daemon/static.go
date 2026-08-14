package daemon

import (
	"bytes"
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/aiden0rchad/oonfeewrt/ui"
)

// mountUI serves the embedded single-page app.
//
// Three behaviours worth stating.
//
// index.html is served from memory rather than through http.FileServerFS.
// That server redirects any path ending in "/index.html" back to the directory,
// which turns every page load into a 301 loop when index.html is also the
// fallback for client routes. Writing the bytes sidesteps the whole convention.
//
// Unknown paths fall back to index.html, because the app owns its routes and a
// deep link must not 404 on a hard refresh. The fallback stops at /api/ — an
// unknown API path is a genuine 404, and returning HTML there would turn a typo
// into a JSON parse error three layers from the cause.
//
// Hashed assets are cached forever and index.html is not cached at all. Vite
// puts a content hash in every asset filename, so those are immutable by
// construction; the HTML that names them must never be stale, or an upgrade
// leaves a browser asking for files that no longer exist.
func (d *Daemon) mountUI(mux *http.ServeMux) {
	files, err := ui.FS()
	if err != nil {
		d.mountMissingUI(mux, err)
		return
	}
	index, err := fs.ReadFile(files, "index.html")
	if err != nil {
		d.mountMissingUI(mux, err)
		return
	}
	// One timestamp for the process lifetime: the content cannot change while
	// the binary runs, so this makes conditional requests work without a stat.
	started := time.Now()
	fileServer := http.FileServerFS(files)

	serveIndex := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, "index.html", started, bytes.NewReader(index))
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		clean := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if clean == "" || clean == "." || clean == "index.html" {
			serveIndex(w, r)
			return
		}
		if _, err := fs.Stat(files, clean); err != nil {
			// A client route, not a missing file — unless it is an API path,
			// where a 404 is the honest answer.
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.NotFound(w, r)
				return
			}
			serveIndex(w, r)
			return
		}
		if strings.HasPrefix(clean, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		fileServer.ServeHTTP(w, r)
	})
}

// mountMissingUI explains itself rather than 404ing. A blank page gives an
// operator nothing to act on; this names the command that fixes it.
func (d *Daemon) mountMissingUI(mux *http.ServeMux, cause error) {
	if errors.Is(cause, ui.ErrNotBuilt) {
		d.Log.Warn("no UI is embedded in this binary; the API is still served",
			"fix", "npm --prefix ui run build")
	} else {
		d.Log.Error("could not open the embedded UI", "err", cause)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("oonfeeWRT: this binary was built without the UI.\n" +
			"The API is available under /api/v1/.\n" +
			"To include it: npm --prefix ui run build, then rebuild.\n"))
	})
}
