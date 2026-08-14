package api

import (
	"net/http"
	"net/url"
	"strings"
)

// sameOrigin reports whether a mutating request came from this origin.
//
// This exists because the CSRF token only protects routes behind requireAuth,
// and /setup and /login are deliberately in front of it. /setup is the sharp
// one: on a fresh controller it creates the administrator account, so any page
// the operator visits could have claimed the install out from under them.
//
// Browsers state the answer directly in Sec-Fetch-Site. Where that is missing
// (older browsers) Origin and then Referer stand in. A request carrying none of
// the three is allowed: that is a non-browser client — curl, a script — which
// is not a CSRF vector at all, since anything that can set arbitrary headers
// can already forge whatever check we invent. Blocking those would break
// automation without protecting anyone.
func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		// "none" is a user-initiated navigation — typed URL, bookmark.
		return true
	case "cross-site", "same-site":
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		return originMatchesHost(o, r)
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		return originMatchesHost(ref, r)
	}
	return true
}

func originMatchesHost(raw string, r *http.Request) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// requireSameOrigin wraps the unauthenticated mutating routes.
func requireSameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			writeErr(w, http.StatusForbidden,
				"cross-origin requests are not accepted on this endpoint")
			return
		}
		next(w, r)
	}
}
