package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aiden0rchad/oonfeewrt/internal/secrets"
	"github.com/aiden0rchad/oonfeewrt/internal/store"
)

// Cookie and header names. The CSRF header is custom on purpose: a browser will
// not let a cross-origin form set one, so requiring it is itself most of the
// defence, with the token comparison closing the rest.
const (
	sessionCookie = "oonfee_session"
	csrfHeader    = "X-Oonfee-CSRF" //nolint:gosec // a header name, not a credential
	csrfCookie    = "oonfee_csrf"
)

// Session lifetimes. Idle expiry keeps an abandoned browser tab from being a
// standing key; absolute expiry bounds a stolen cookie regardless of use.
const (
	sessionIdle     = 12 * time.Hour
	sessionAbsolute = 7 * 24 * time.Hour
)

// session is one signed-in operator.
type session struct {
	adminID  int64
	username string
	csrf     string
	created  time.Time
	lastSeen time.Time
}

// sessions is an in-memory session table.
//
// In memory, deliberately: sessions do not survive a restart, which is the
// correct behaviour for a controller that holds device credentials — restarting
// it already requires the operator passphrase, so a session that outlived the
// process would be a way around that.
type sessions struct {
	mu sync.Mutex
	m  map[string]*session
}

func newSessions() *sessions { return &sessions{m: map[string]*session{}} }

func (s *sessions) create(adminID int64, username string, now time.Time) (token string, sess *session, err error) {
	token, err = randomToken()
	if err != nil {
		return "", nil, err
	}
	csrf, err := randomToken()
	if err != nil {
		return "", nil, err
	}
	sess = &session{
		adminID: adminID, username: username, csrf: csrf,
		created: now, lastSeen: now,
	}
	s.mu.Lock()
	s.m[token] = sess
	s.mu.Unlock()
	return token, sess, nil
}

// get returns a live session and refreshes its idle timer.
func (s *sessions) get(token string, now time.Time) (*session, bool) {
	if token == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[token]
	if !ok {
		return nil, false
	}
	if now.Sub(sess.lastSeen) > sessionIdle || now.Sub(sess.created) > sessionAbsolute {
		delete(s.m, token)
		return nil, false
	}
	sess.lastSeen = now
	return sess, true
}

func (s *sessions) drop(token string) {
	s.mu.Lock()
	delete(s.m, token)
	s.mu.Unlock()
}

// dropAdmin ends every session belonging to one operator. A password change
// that left old sessions alive would not be a password change.
func (s *sessions) dropAdmin(adminID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for tok, sess := range s.m {
		if sess.adminID == adminID {
			delete(s.m, tok)
		}
	}
}

func (s *sessions) sweep(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for tok, sess := range s.m {
		if now.Sub(sess.lastSeen) > sessionIdle || now.Sub(sess.created) > sessionAbsolute {
			delete(s.m, tok)
		}
	}
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ---- login throttling ----

// throttle rate-limits sign-in attempts per client address.
//
// Per address rather than per username: throttling by username lets anyone lock
// a known operator out by failing on their behalf, which turns a defence into a
// denial of service.
type throttle struct {
	mu      sync.Mutex
	fails   map[string]*failCount
	max     int
	window  time.Duration
	lockout time.Duration
}

type failCount struct {
	n     int
	first time.Time
	until time.Time
}

func newThrottle() *throttle {
	return &throttle{
		fails:   map[string]*failCount{},
		max:     10,
		window:  5 * time.Minute,
		lockout: 5 * time.Minute,
	}
}

// allow reports whether an address may attempt a sign-in, and how long it must
// wait if not.
func (t *throttle) allow(addr string, now time.Time) (bool, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f := t.fails[addr]
	if f == nil {
		return true, 0
	}
	if now.Before(f.until) {
		return false, f.until.Sub(now)
	}
	if now.Sub(f.first) > t.window {
		delete(t.fails, addr)
	}
	return true, 0
}

func (t *throttle) fail(addr string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f := t.fails[addr]
	if f == nil || now.Sub(f.first) > t.window {
		f = &failCount{first: now}
		t.fails[addr] = f
	}
	f.n++
	if f.n >= t.max {
		f.until = now.Add(t.lockout)
	}
}

func (t *throttle) succeed(addr string) {
	t.mu.Lock()
	delete(t.fails, addr)
	t.mu.Unlock()
}

func (t *throttle) sweep(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for addr, f := range t.fails {
		if now.After(f.until) && now.Sub(f.first) > t.window {
			delete(t.fails, addr)
		}
	}
}

// clientAddr identifies the caller for throttling.
//
// It uses the socket's peer address and NOT X-Forwarded-For. A header any
// client can set is a rate limiter any client can bypass by varying it, which is
// worse than no limiter because it looks like one. Running behind a proxy that
// needs the real address is a deployment concern to solve explicitly, with a
// configured trusted-proxy list, rather than by trusting a header by default.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---- middleware ----

type ctxKey int

const sessionCtxKey ctxKey = iota

// sessionFrom returns the signed-in operator, if any.
func sessionFrom(ctx context.Context) (*session, bool) {
	s, ok := ctx.Value(sessionCtxKey).(*session)
	return s, ok
}

// requireAuth rejects anything without a live session, and anything mutating
// without a matching CSRF token.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "not signed in")
			return
		}
		sess, ok := s.sessions.get(c.Value, s.now())
		if !ok {
			// Clear the cookie so the browser stops presenting a dead token on
			// every request from here on.
			s.clearSessionCookies(w, r)
			writeErr(w, http.StatusUnauthorized, "session expired")
			return
		}
		if isMutating(r.Method) {
			if subtle.ConstantTimeCompare(
				[]byte(r.Header.Get(csrfHeader)), []byte(sess.csrf)) != 1 {
				writeErr(w, http.StatusForbidden,
					"missing or incorrect "+csrfHeader+" header")
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionCtxKey, sess)))
	})
}

func isMutating(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// secureCookies reports whether cookies may carry the Secure attribute.
//
// Set only over TLS: a Secure cookie on a plain-HTTP listener is silently
// dropped by the browser, so an install on http://nas.local:8080 would be
// unable to sign in at all. The tradeoff is stated in the docs rather than
// papered over here.
func secureCookies(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) setSessionCookies(w http.ResponseWriter, r *http.Request, token, csrf string) {
	secure := secureCookies(r)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode,
		MaxAge: int(sessionAbsolute.Seconds()),
	})
	// Readable by script on purpose: the UI must echo it back in the header, and
	// a value the page cannot read cannot be echoed. It is not a secret in the
	// way the session cookie is — knowing it is useless without also holding the
	// session, which HttpOnly keeps out of reach.
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookie, Value: csrf, Path: "/",
		HttpOnly: false, Secure: secure, SameSite: http.SameSiteStrictMode,
		MaxAge: int(sessionAbsolute.Seconds()),
	})
}

func (s *Server) clearSessionCookies(w http.ResponseWriter, r *http.Request) {
	secure := secureCookies(r)
	for _, name := range []string{sessionCookie, csrfCookie} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: name == sessionCookie, Secure: secure,
			SameSite: http.SameSiteStrictMode,
		})
	}
}

// ---- handlers ----

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleLogin signs an operator in.
//
// Every failure returns the same message and the same status. Distinguishing
// "no such user" from "wrong password" hands an attacker a free way to
// enumerate accounts, and the operator who typed it wrong learns nothing useful
// from the difference either.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	addr := clientAddr(r)
	now := s.now()
	if ok, wait := s.throttle.allow(addr, now); !ok {
		w.Header().Set("Retry-After", itoa(int(wait.Seconds())+1))
		writeErr(w, http.StatusTooManyRequests,
			"too many sign-in attempts; try again shortly")
		return
	}

	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	admin, err := s.Store.AdminByName(r.Context(), req.Username)
	if err != nil || secrets.VerifyPassword([]byte(req.Password), admin.PassHash) != nil {
		s.throttle.fail(addr, now)
		s.logAuth(r.Context(), "auth.login_failed", "warning", req.Username, addr)
		writeErr(w, http.StatusUnauthorized, "incorrect username or password")
		return
	}
	s.throttle.succeed(addr)

	token, sess, err := s.sessions.create(admin.ID, admin.Username, now)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not start a session")
		return
	}
	_ = s.Store.TouchAdminLogin(r.Context(), admin.ID)

	// Raising the cost is worth nothing if existing accounts keep the old one,
	// and a successful sign-in is the only moment the password is in hand.
	if secrets.NeedsRehash(admin.PassHash, secrets.DefaultParams()) {
		if h, err := secrets.HashPassword([]byte(req.Password), secrets.DefaultParams()); err == nil {
			_ = s.Store.SetAdminPassword(r.Context(), admin.ID, h)
		}
	}

	s.setSessionCookies(w, r, token, sess.csrf)
	s.logAuth(r.Context(), "auth.login", "info", admin.Username, addr)
	writeJSON(w, http.StatusOK, map[string]any{
		"username": admin.Username,
		"csrf":     sess.csrf,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.drop(c.Value)
	}
	s.clearSessionCookies(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSession reports who is signed in, so a reloaded page can restore its
// state without a round trip through the login screen.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"username": sess.username,
		"csrf":     sess.csrf,
	})
}

// handleSetup enrols the first operator.
//
// It works exactly once, and only while no account exists. There is no default
// credential to change afterwards, which is the point — a shipped default that
// nobody rotates is the single most common way a self-hosted controller ends up
// on the public internet with a known password.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	n, err := s.Store.AdminCount(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not read the account table")
		return
	}
	if n > 0 {
		writeErr(w, http.StatusConflict, "an administrator account already exists")
		return
	}
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := validateCredential(req.Username, req.Password); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := secrets.HashPassword([]byte(req.Password), secrets.DefaultParams())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not hash the password")
		return
	}
	admin, err := s.Store.CreateAdmin(r.Context(), req.Username, hash)
	if err != nil {
		writeErr(w, http.StatusConflict, "could not create the account")
		return
	}
	token, sess, err := s.sessions.create(admin.ID, admin.Username, s.now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not start a session")
		return
	}
	s.setSessionCookies(w, r, token, sess.csrf)
	s.logAuth(r.Context(), "auth.admin_created", "info", admin.Username, clientAddr(r))
	writeJSON(w, http.StatusCreated, map[string]any{
		"username": admin.Username,
		"csrf":     sess.csrf,
	})
}

// handleSetupState tells the UI whether to show the setup screen or the login
// screen. It is unauthenticated and says nothing beyond that one bit.
func (s *Server) handleSetupState(w http.ResponseWriter, r *http.Request) {
	n, err := s.Store.AdminCount(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not read the account table")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"needs_setup": n == 0})
}

type passwordChange struct {
	Current string `json:"current_password"`
	New     string `json:"new_password"`
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	var req passwordChange
	if !decodeJSON(w, r, &req) {
		return
	}
	admin, err := s.Store.AdminByName(r.Context(), sess.username)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not read the account")
		return
	}
	// The current password is required even though the caller is already signed
	// in: it is what stops a borrowed session from becoming permanent ownership
	// of the account.
	if err := secrets.VerifyPassword([]byte(req.Current), admin.PassHash); err != nil {
		s.logAuth(r.Context(), "auth.password_change_failed", "warning",
			admin.Username, clientAddr(r))
		writeErr(w, http.StatusUnauthorized, "current password is incorrect")
		return
	}
	if err := validateCredential(admin.Username, req.New); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := secrets.HashPassword([]byte(req.New), secrets.DefaultParams())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not hash the password")
		return
	}
	if err := s.Store.SetAdminPassword(r.Context(), admin.ID, hash); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not save the password")
		return
	}
	// Every session, including this one. A password change that leaves the old
	// sessions alive has not actually changed anything for whoever had one.
	s.sessions.dropAdmin(admin.ID)
	s.clearSessionCookies(w, r)
	s.logAuth(r.Context(), "auth.password_changed", "info", admin.Username, clientAddr(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "message": "password changed; sign in again",
	})
}

// minPasswordLen is a floor, not a policy. Composition rules (a digit, a
// symbol) push people toward predictable substitutions and are not applied.
const minPasswordLen = 12

func validateCredential(username, password string) error {
	if strings.TrimSpace(username) == "" {
		return errors.New("username is required")
	}
	if len(username) > 64 {
		return errors.New("username is too long")
	}
	if len([]rune(password)) < minPasswordLen {
		return errors.New("password must be at least 12 characters")
	}
	if len(password) > 1024 {
		// argon2 will hash anything, but an unbounded input is an unbounded
		// amount of work on an unauthenticated endpoint.
		return errors.New("password is too long")
	}
	return nil
}

func (s *Server) logAuth(ctx context.Context, event, severity, username, addr string) {
	_ = s.Store.LogEvent(ctx, store.Event{
		Category: "audit", Severity: severity, Event: event,
		Detail: map[string]any{"username": username, "addr": addr},
	})
}
