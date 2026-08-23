package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
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
	reauthValidity  = 5 * time.Minute

	credentialFailureLimit  = 5
	credentialFailureWindow = 5 * time.Minute
	credentialLockout       = 5 * time.Minute
)

// session is one signed-in operator.
type session struct {
	id                string
	adminID           int64
	username          string
	role              store.AccountRole
	csrf              string
	peerAddress       string
	created           time.Time
	lastSeen          time.Time
	reauthenticatedAt time.Time
	credentialFails   failCount
	done              chan struct{}
	revoke            sync.Once
}

func (s *session) close() { s.revoke.Do(func() { close(s.done) }) }

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

func (s *sessions) create(adminID int64, username string, role store.AccountRole,
	peerAddress string, now time.Time) (token string, sess *session, err error) {
	if !role.Valid() {
		return "", nil, store.ErrInvalidRole
	}
	for {
		token, err = randomToken()
		if err != nil {
			return "", nil, err
		}
		managementID, err := randomToken()
		if err != nil {
			return "", nil, err
		}
		csrf, err := randomToken()
		if err != nil {
			return "", nil, err
		}
		sess = &session{
			id: managementID, adminID: adminID, username: username, role: role,
			csrf: csrf, peerAddress: peerAddress, created: now, lastSeen: now,
			reauthenticatedAt: now, done: make(chan struct{}),
		}
		s.mu.Lock()
		_, tokenExists := s.m[token]
		idExists := s.managementIDExistsLocked(managementID)
		if !tokenExists && !idExists {
			s.m[token] = sess
			s.mu.Unlock()
			return token, sess, nil
		}
		s.mu.Unlock()
	}
}

func (s *sessions) managementIDExistsLocked(id string) bool {
	for _, sess := range s.m {
		if sess.id == id {
			return true
		}
	}
	return false
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
	if sessionExpired(sess, now) {
		delete(s.m, token)
		sess.close()
		return nil, false
	}
	sess.lastSeen = now
	return sess, true
}

type sessionRecord struct {
	ID          string
	AdminID     int64
	Username    string
	Role        store.AccountRole
	PeerAddress string
	Created     time.Time
	LastSeen    time.Time
	Expires     time.Time
	Current     bool
}

func sessionExpired(sess *session, now time.Time) bool {
	return now.Sub(sess.lastSeen) > sessionIdle || now.Sub(sess.created) > sessionAbsolute
}

func sessionExpiry(sess *session) time.Time {
	idle := sess.lastSeen.Add(sessionIdle)
	absolute := sess.created.Add(sessionAbsolute)
	if absolute.Before(idle) {
		return absolute
	}
	return idle
}

func sessionRecordLocked(sess *session, currentID string) sessionRecord {
	return sessionRecord{
		ID: sess.id, AdminID: sess.adminID, Username: sess.username, Role: sess.role,
		PeerAddress: sess.peerAddress, Created: sess.created, LastSeen: sess.lastSeen,
		Expires: sessionExpiry(sess), Current: sess.id == currentID,
	}
}

func (s *sessions) liveLocked(want *session) bool {
	for _, sess := range s.m {
		if sess == want {
			return true
		}
	}
	return false
}

func (s *sessions) pruneLocked(now time.Time) {
	for token, sess := range s.m {
		if sessionExpired(sess, now) {
			delete(s.m, token)
			sess.close()
		}
	}
}

func (s *sessions) drop(token string) (sessionRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.m[token]; sess != nil {
		delete(s.m, token)
		sess.close()
		return sessionRecordLocked(sess, sess.id), true
	}
	return sessionRecord{}, false
}

// dropAdmin ends every session belonging to one operator. A password change
// that left old sessions alive would not be a password change.
func (s *sessions) dropAdmin(adminID int64) []sessionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var dropped []sessionRecord
	for tok, sess := range s.m {
		if sess.adminID == adminID {
			delete(s.m, tok)
			sess.close()
			dropped = append(dropped, sessionRecordLocked(sess, ""))
		}
	}
	return dropped
}

func (s *sessions) dropID(adminID int64, managementID string) (sessionRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, sess := range s.m {
		if sess.adminID == adminID && sess.id == managementID {
			delete(s.m, token)
			sess.close()
			return sessionRecordLocked(sess, ""), true
		}
	}
	return sessionRecord{}, false
}

func (s *sessions) list(adminID int64, currentID string, now time.Time) []sessionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	var records []sessionRecord
	for _, sess := range s.m {
		if sess.adminID == adminID {
			records = append(records, sessionRecordLocked(sess, currentID))
		}
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Current != records[j].Current {
			return records[i].Current
		}
		if !records[i].LastSeen.Equal(records[j].LastSeen) {
			return records[i].LastSeen.After(records[j].LastSeen)
		}
		return records[i].ID < records[j].ID
	})
	return records
}

func (s *sessions) counts(now time.Time) map[int64]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	counts := make(map[int64]int)
	for _, sess := range s.m {
		counts[sess.adminID]++
	}
	return counts
}

func (s *sessions) allowCredentialAttempt(sess *session, now time.Time) (bool, time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.liveLocked(sess) {
		return false, 0, false
	}
	failure := &sess.credentialFails
	if now.Before(failure.until) {
		return false, failure.until.Sub(now), true
	}
	if !failure.first.IsZero() && now.Sub(failure.first) >= credentialFailureWindow {
		*failure = failCount{}
	}
	return true, 0, true
}

func (s *sessions) failCredentialAttempt(sess *session, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.liveLocked(sess) {
		return
	}
	failure := &sess.credentialFails
	if failure.first.IsZero() || now.Sub(failure.first) >= credentialFailureWindow {
		*failure = failCount{first: now}
	}
	failure.n++
	if failure.n >= credentialFailureLimit {
		failure.until = now.Add(credentialLockout)
	}
}

func (s *sessions) succeedCredentialAttempt(sess *session, now time.Time,
	markReauthenticated bool) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.liveLocked(sess) {
		return time.Time{}, false
	}
	sess.credentialFails = failCount{}
	if markReauthenticated {
		sess.reauthenticatedAt = now
	}
	return sess.reauthenticatedAt.Add(reauthValidity), true
}

func (s *sessions) reauthenticatedUntil(sess *session, now time.Time) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.liveLocked(sess) {
		return time.Time{}, false
	}
	until := sess.reauthenticatedAt.Add(reauthValidity)
	return until, now.Before(until)
}

func (s *sessions) sweep(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
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
	raw := strings.TrimSpace(r.RemoteAddr)
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		host = strings.Trim(raw, "[]")
	} else if port == "" {
		return "unknown"
	} else if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return "unknown"
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return "unknown"
	}
	return addr.Unmap().String()
}

// ---- password work ----

// hashSlots bounds how many argon2id derivations run at once.
//
// The login throttle limits the RATE of completed attempts; it does nothing
// about CONCURRENT ones, because it records a failure only after the hash has
// finished. Each derivation allocates a 64 MiB arena (DefaultParams), so an
// unauthenticated caller opening a few hundred simultaneous logins could ask
// the process for tens of gigabytes — against a documented steady-state
// envelope of 256 MB.
//
// Two at a time caps that at ~128 MiB. This is a single-operator controller;
// genuine concurrent sign-ins are approximately one, and the cost of being
// wrong in this direction is a 503 rather than an OOM.
const hashSlots = 2

// withHashSlot runs fn holding a hashing slot, or reports that none was free.
func (s *Server) withHashSlot(fn func()) bool {
	select {
	case s.hashing <- struct{}{}:
		defer func() { <-s.hashing }()
		fn()
		return true
	default:
		return false
	}
}

// verifyPassword checks a password, spending the same work whether or not the
// account exists.
//
// The naive form short-circuits: `err != nil || VerifyPassword(...)` never
// hashes anything when the username is unknown, so an unknown account answers
// in microseconds and a known one in tens of milliseconds. The status and body
// are identical — the test asserts that — but the clock is not, and account
// enumeration is exactly what identical responses were meant to prevent.
//
// So the unknown-account path verifies against a fixed dummy hash generated at
// startup with the same parameters, and throws the result away.
func (s *Server) verifyPassword(admin *store.Admin, password string) bool {
	if admin == nil {
		_ = secrets.VerifyPassword([]byte(password), s.dummyHash)
		return false
	}
	return secrets.VerifyPassword([]byte(password), admin.PassHash) == nil
}

// ---- middleware ----

type ctxKey int

const sessionCtxKey ctxKey = iota

// sessionFrom returns the signed-in operator, if any.
func sessionFrom(ctx context.Context) (*session, bool) {
	s, ok := ctx.Value(sessionCtxKey).(*session)
	return s, ok
}

func roleAllows(actual, required store.AccountRole) bool {
	rank := func(role store.AccountRole) int {
		switch role {
		case store.RoleViewer:
			return 1
		case store.RoleOperator:
			return 2
		case store.RoleAdmin:
			return 3
		case store.RoleOwner:
			return 4
		default:
			return 0
		}
	}
	return rank(required) > 0 && rank(actual) >= rank(required)
}

func sessionHasRole(ctx context.Context, required store.AccountRole) bool {
	sess, ok := sessionFrom(ctx)
	return ok && roleAllows(sess.role, required)
}

func (s *Server) requireRole(required store.AccountRole, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !sessionHasRole(r.Context(), required) {
			writeCodedErr(w, http.StatusForbidden, "insufficient_role",
				"insufficient account permissions")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireRecentReauth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, ok := sessionFrom(r.Context())
		if !ok {
			writeCodedErr(w, http.StatusUnauthorized, "not_signed_in", "not signed in")
			return
		}
		if _, ok := s.sessions.reauthenticatedUntil(sess, s.now()); !ok {
			writeCodedErr(w, http.StatusPreconditionRequired, "reauth_required",
				"recent password confirmation is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireAuth rejects anything without a live session, and anything mutating
// without a matching CSRF token.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			writeCodedErr(w, http.StatusUnauthorized, "not_signed_in", "not signed in")
			return
		}
		sess, ok := s.sessions.get(c.Value, s.now())
		if !ok {
			// Clear the cookie so the browser stops presenting a dead token on
			// every request from here on.
			s.clearSessionCookies(w, r)
			writeCodedErr(w, http.StatusUnauthorized, "not_signed_in", "session expired")
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
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		go func(requestDone <-chan struct{}) {
			select {
			case <-sess.done:
				cancel()
			case <-requestDone:
			}
		}(ctx.Done())
		ctx = context.WithValue(ctx, sessionCtxKey, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
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
	if err != nil {
		admin = nil // verified against the dummy hash below, for constant work
	}

	var ok bool
	if !s.withHashSlot(func() { ok = s.verifyPassword(admin, req.Password) }) {
		w.Header().Set("Retry-After", "2")
		writeErr(w, http.StatusServiceUnavailable,
			"too many sign-ins in progress; try again shortly")
		return
	}
	if !ok {
		s.throttle.fail(addr, now)
		s.logAuth(r.Context(), "auth.login_failed", "warning", req.Username, addr)
		writeErr(w, http.StatusUnauthorized, "incorrect username or password")
		return
	}
	if s.afterLoginPasswordVerified != nil {
		s.afterLoginPasswordVerified()
	}

	token, sess, err := s.sessions.create(admin.ID, admin.Username, admin.Role, addr, now)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not start a session")
		return
	}
	current, currentErr := s.Store.AdminByID(r.Context(), admin.ID)
	if currentErr != nil || !current.Enabled || current.Username != admin.Username ||
		current.Role != admin.Role || subtle.ConstantTimeCompare(
		[]byte(current.PassHash), []byte(admin.PassHash)) != 1 {
		s.sessions.drop(token)
		if currentErr != nil && !errors.Is(currentErr, store.ErrNotFound) {
			writeErr(w, http.StatusInternalServerError, "could not start a session")
			return
		}
		s.throttle.fail(addr, now)
		s.logAuth(r.Context(), "auth.login_failed", "warning", req.Username, addr)
		writeErr(w, http.StatusUnauthorized, "incorrect username or password")
		return
	}
	s.throttle.succeed(addr)
	_ = s.Store.TouchAdminLogin(r.Context(), admin.ID)

	// Raising the cost is worth nothing if existing accounts keep the old one,
	// and a successful sign-in is the only moment the password is in hand.
	if secrets.NeedsRehash(admin.PassHash, secrets.DefaultParams()) {
		s.withHashSlot(func() {
			if h, err := secrets.HashPassword([]byte(req.Password), secrets.DefaultParams()); err == nil {
				_ = s.Store.RehashAdminPassword(r.Context(), admin.ID, h)
			}
		})
	}

	s.setSessionCookies(w, r, token, sess.csrf)
	s.logAuth(r.Context(), "auth.login", "info", admin.Username, addr)
	writeJSON(w, http.StatusOK, s.sessionPayload(sess))
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	if c, err := r.Cookie(sessionCookie); err == nil {
		if dropped, ok := s.sessions.drop(c.Value); ok {
			s.auditSessionRevocation(r, "auth.logout", sess, dropped.AdminID,
				dropped.Username, "self", []sessionRecord{dropped})
		}
	}
	s.clearSessionCookies(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSession reports who is signed in, so a reloaded page can restore its
// state without a round trip through the login screen.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	writeJSON(w, http.StatusOK, s.sessionPayload(sess))
}

func (s *Server) sessionPayload(sess *session) map[string]any {
	var reauthenticatedUntil any
	if until, ok := s.sessions.reauthenticatedUntil(sess, s.now()); ok {
		reauthenticatedUntil = until.Unix()
	}
	return map[string]any{
		"admin_id": sess.adminID, "username": sess.username, "role": sess.role,
		"role_label": roleLabel(sess.role), "csrf": sess.csrf,
		"reauthenticated_until": reauthenticatedUntil,
	}
}

// handleSetup enrols the first operator.
//
// It works exactly once, and only while no account exists. There is no default
// credential to change afterwards, which is the point — a shipped default that
// nobody rotates is the single most common way a self-hosted controller ends up
// on the public internet with a known password.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	// Same-origin is not enough before an administrator exists: an attacker can
	// make its own DNS name resolve to this LAN service in the victim's browser.
	if !directSetupHost(r.Host) {
		writeErr(w, http.StatusForbidden,
			"first-run setup requires a direct localhost or literal IP URL; open this controller by IP address and try again")
		return
	}
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
	if err := store.ValidateAccountUsername(req.Username); err != nil {
		writeErr(w, http.StatusBadRequest,
			"username must start with a letter or digit and use only ASCII letters, digits, '.', '_' or '-'")
		return
	}
	var hash string
	var hashErr error
	if !s.withHashSlot(func() {
		hash, hashErr = secrets.HashPassword([]byte(req.Password), secrets.DefaultParams())
	}) {
		w.Header().Set("Retry-After", "2")
		writeErr(w, http.StatusServiceUnavailable, "busy; try again shortly")
		return
	}
	if hashErr != nil {
		writeErr(w, http.StatusInternalServerError, "could not hash the password")
		return
	}

	// The count above is a courtesy, not the guard. It and this insert are
	// separated by an argon2id derivation of tens of milliseconds, which is
	// plenty of room for a second request to pass the same check — and two
	// different usernames would both insert cleanly, since only `username` is
	// unique. The insert itself is what enforces "exactly once".
	admin, err := s.Store.CreateFirstAdmin(r.Context(), req.Username, hash)
	if errors.Is(err, store.ErrAdminExists) {
		writeErr(w, http.StatusConflict, "an administrator account already exists")
		return
	}
	if err != nil {
		writeErr(w, http.StatusConflict, "could not create the account")
		return
	}
	token, sess, err := s.sessions.create(admin.ID, admin.Username, admin.Role,
		clientAddr(r), s.now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not start a session")
		return
	}
	s.setSessionCookies(w, r, token, sess.csrf)
	writeJSON(w, http.StatusCreated, s.sessionPayload(sess))
}

func directSetupHost(hostport string) bool {
	host := hostport
	if splitHost, port, err := net.SplitHostPort(hostport); err == nil {
		if port == "" {
			return false
		}
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return false
		}
		host = splitHost
	} else if len(hostport) >= 2 && hostport[0] == '[' && hostport[len(hostport)-1] == ']' {
		host = hostport[1 : len(hostport)-1]
	}
	return strings.EqualFold(host, "localhost") || net.ParseIP(host) != nil
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
		writeErr(w, http.StatusInternalServerError, "could not read the account")
		return
	}
	// The current password is required even though the caller is already signed
	// in: it is what stops a borrowed session from becoming permanent ownership
	// of the account.
	var ok bool
	if !s.withHashSlot(func() { ok = s.verifyPassword(admin, req.Current) }) {
		w.Header().Set("Retry-After", "2")
		writeCodedErr(w, http.StatusServiceUnavailable, "password_hash_capacity",
			"busy; try again shortly")
		return
	}
	if !ok {
		s.sessions.failCredentialAttempt(sess, now)
		s.logAuth(r.Context(), "auth.password_change_failed", "warning",
			admin.Username, clientAddr(r))
		writeCodedErr(w, http.StatusUnauthorized, "incorrect_password",
			"current password is incorrect")
		return
	}
	if _, live := s.sessions.succeedCredentialAttempt(sess, now, false); !live {
		s.clearSessionCookies(w, r)
		writeCodedErr(w, http.StatusUnauthorized, "not_signed_in", "session expired")
		return
	}
	if err := validateCredential(admin.Username, req.New); err != nil {
		writeCodedErr(w, http.StatusBadRequest, "weak_password", err.Error())
		return
	}
	var hash string
	var hashErr error
	if !s.withHashSlot(func() {
		hash, hashErr = secrets.HashPassword([]byte(req.New), secrets.DefaultParams())
	}) {
		w.Header().Set("Retry-After", "2")
		writeCodedErr(w, http.StatusServiceUnavailable, "password_hash_capacity",
			"busy; try again shortly")
		return
	}
	if hashErr != nil {
		writeErr(w, http.StatusInternalServerError, "could not hash the password")
		return
	}
	if err := s.Store.SetAdminPassword(r.Context(), admin.ID, hash); err != nil {
		s.writeAccountStoreError(w, r, err)
		return
	}
	// Every session, including this one. A password change that leaves the old
	// sessions alive has not actually changed anything for whoever had one.
	dropped := s.sessions.dropAdmin(admin.ID)
	s.clearSessionCookies(w, r)
	s.auditSessionRevocation(r, "auth.sessions_revoked", sess, admin.ID,
		admin.Username, "self", dropped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "message": "password changed; sign in again", "signed_out": true,
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
	return validateNewPassword(password)
}

func validateNewPassword(password string) error {
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
