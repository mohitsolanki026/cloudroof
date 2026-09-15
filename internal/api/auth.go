// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Mohit Solanki

package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"github.com/mohitsolanki026/cloudroof/internal/store"
)

// Authentication (v2.0) is a single admin account guarding the whole HTTP and
// WebSocket surface. Sessions are server-side: the browser holds an opaque
// random token in an HttpOnly, SameSite=Strict cookie and only the token's
// SHA-256 is stored, so the cookie is XSS-safe and revocable at once. The
// existing same-origin guard already covers CSRF, so no separate token is
// needed here.
//
// This protects the network surface only — it does NOT gate the keyring, so a
// stolen master.key still exposes sealed credentials regardless of login.
const (
	sessionCookie  = "cloudroof_session"
	sessionTTL     = 30 * 24 * time.Hour // absolute lifetime of a session
	sessionSlide   = 24 * time.Hour      // rewrite expiry/last_seen at most this often
	bcryptCost     = 12
	minPasswordLen = 10
	maxPasswordLen = 72 // bcrypt ignores bytes past 72; reject rather than silently truncate
	maxUsernameLen = 64

	loginMaxFails   = 8               // consecutive failures from one IP before a lockout
	loginLockWindow = 5 * time.Minute // how long that IP is then refused
)

// errNoSession is the internal marker for "no valid session on this request".
var errNoSession = errors.New("auth: no session")

// --- password + token primitives --------------------------------------------

func hashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	return string(b), err
}

func verifyPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// dummyHash is compared against when the username is unknown, so a login attempt
// costs the same whether or not the user exists (no timing-based enumeration).
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("cloudroof-timing-equalizer"), bcryptCost)

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func validatePassword(pw string) error {
	if n := len(pw); n < minPasswordLen || n > maxPasswordLen {
		return errBadPassword
	}
	return nil
}

var (
	errBadPassword = errors.New("password must be 10–72 bytes")
	errBadUsername = errors.New("username must be 1–64 characters, no spaces")
)

func validateUsername(u string) error {
	if u == "" || utf8.RuneCountInString(u) > maxUsernameLen || strings.ContainsAny(u, " \t\r\n") {
		return errBadUsername
	}
	return nil
}

// --- login rate limiter ------------------------------------------------------

type attemptRec struct {
	fails       int
	lockedUntil time.Time
}

type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string]*attemptRec
}

func newLoginLimiter() *loginLimiter { return &loginLimiter{attempts: map[string]*attemptRec{}} }

// allowed reports whether an attempt from ip may proceed, and if not, how long
// until it may.
func (l *loginLimiter) allowed(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.attempts[ip]
	if r == nil {
		return true, 0
	}
	if now := time.Now(); now.Before(r.lockedUntil) {
		return false, time.Until(r.lockedUntil)
	}
	return true, 0
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.attempts[ip]
	if r == nil {
		r = &attemptRec{}
		l.attempts[ip] = r
	}
	r.fails++
	if r.fails >= loginMaxFails {
		r.lockedUntil = time.Now().Add(loginLockWindow)
		r.fails = 0
	}
}

func (l *loginLimiter) success(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, ip)
}

// --- current-user plumbing ---------------------------------------------------

type ctxKey int

const userCtxKey ctxKey = 0

func withUser(ctx context.Context, u store.User) context.Context {
	return context.WithValue(ctx, userCtxKey, u)
}

func userFromContext(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(userCtxKey).(store.User)
	return u, ok
}

// currentUser resolves the session cookie to a user, sliding the session's
// expiry when it has been idle a while. It is the single gate the middleware
// and the WebSocket handlers share.
func (s *Server) currentUser(r *http.Request) (store.User, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return store.User{}, errNoSession
	}
	th := hashToken(c.Value)
	sess, err := s.store.GetSession(th)
	if err != nil {
		return store.User{}, errNoSession
	}
	u, err := s.store.GetUser(sess.UserID)
	if err != nil {
		return store.User{}, errNoSession
	}
	if time.Since(sess.LastSeen) > sessionSlide {
		now := time.Now().UTC()
		_ = s.store.TouchSession(th, now, now.Add(sessionTTL))
	}
	return u, nil
}

// actorFor names who is performing an action, for the audit log. With auth off
// (or as a fallback) it is the legacy "admin".
func (s *Server) actorFor(r *http.Request) string {
	if u, ok := userFromContext(r.Context()); ok && u.Username != "" {
		return u.Username
	}
	return actor
}

// requireAuth is the middleware that gates every /api route once auth is on.
// A handful of endpoints are reachable without a session so the client can
// discover state, log in, or perform first-run setup.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	exempt := map[string]bool{
		"/api/health":      true,
		"/api/auth/me":     true,
		"/api/auth/login":  true,
		"/api/auth/setup":  true,
		"/api/auth/logout": true, // clearing a cookie needs no valid session
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authOff || !strings.HasPrefix(r.URL.Path, "/api/") || exempt[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		u, err := s.currentUser(r)
		if err != nil {
			writeJSON(w, 401, apiError{Error: "authentication required", Code: "unauthorized"})
			return
		}
		next.ServeHTTP(w, r.WithContext(withUser(r.Context(), u)))
	})
}

// --- cookie helpers ----------------------------------------------------------

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   isHTTPS(r),
		MaxAge:   int(ttl.Seconds()),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   isHTTPS(r),
		MaxAge:   -1,
	})
}

// isHTTPS reports whether the request reached us over TLS, directly or via a
// terminating reverse proxy, so the cookie's Secure flag is set when it should
// be (and left off for a plain-HTTP loopback bind, where it would break login).
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// startNewSession mints a session for a user and sets the cookie. Returned so
// login, setup, and password-change can all reuse it.
func (s *Server) startNewSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	token, err := newToken()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	ua := r.Header.Get("User-Agent")
	if len(ua) > 256 {
		ua = ua[:256]
	}
	if err := s.store.CreateSession(store.Session{
		TokenHash: hashToken(token),
		UserID:    userID,
		CreatedAt: now,
		ExpiresAt: now.Add(sessionTTL),
		LastSeen:  now,
		IP:        clientIP(r),
		UserAgent: ua,
	}); err != nil {
		return err
	}
	s.setSessionCookie(w, r, token, sessionTTL)
	return nil
}

// --- handlers ----------------------------------------------------------------

type authMeDTO struct {
	AuthDisabled  bool        `json:"authDisabled"`
	SetupRequired bool        `json:"setupRequired"`
	Authenticated bool        `json:"authenticated"`
	User          *store.User `json:"user"`
}

func (s *Server) authMe(w http.ResponseWriter, r *http.Request) {
	if s.authOff {
		writeJSON(w, 200, authMeDTO{AuthDisabled: true, Authenticated: true})
		return
	}
	n, err := s.store.CountUsers()
	if err != nil {
		fail(w, err)
		return
	}
	if n == 0 {
		writeJSON(w, 200, authMeDTO{SetupRequired: true})
		return
	}
	u, err := s.currentUser(r)
	if err != nil {
		writeJSON(w, 200, authMeDTO{})
		return
	}
	writeJSON(w, 200, authMeDTO{Authenticated: true, User: &u})
}

type credsInput struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// authSetup creates the first (admin) account. It works exactly once: once any
// user exists it is closed.
func (s *Server) authSetup(w http.ResponseWriter, r *http.Request) {
	if s.authOff {
		writeJSON(w, 409, apiError{Error: "authentication is disabled (-no-auth)", Code: "auth_disabled"})
		return
	}
	n, err := s.store.CountUsers()
	if err != nil {
		fail(w, err)
		return
	}
	if n > 0 {
		writeJSON(w, 409, apiError{Error: "already set up", Code: "already_setup"})
		return
	}
	var in credsInput
	if err := readJSON(w, r, &in); err != nil {
		badRequest(w, err.Error())
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	if err := validateUsername(in.Username); err != nil {
		badRequest(w, err.Error())
		return
	}
	if err := validatePassword(in.Password); err != nil {
		badRequest(w, err.Error())
		return
	}
	pwHash, err := hashPassword(in.Password)
	if err != nil {
		fail(w, err)
		return
	}
	u, err := s.store.CreateUser(in.Username, pwHash, "admin")
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.startNewSession(w, r, u.ID); err != nil {
		fail(w, err)
		return
	}
	s.log.Info("auth: admin account created", "user", u.Username)
	writeJSON(w, 201, u)
}

func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	if s.authOff {
		writeJSON(w, 409, apiError{Error: "authentication is disabled (-no-auth)", Code: "auth_disabled"})
		return
	}
	ip := clientIP(r)
	if ok, retry := s.login.allowed(ip); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		writeJSON(w, 429, apiError{Error: "too many attempts; try again later", Code: "rate_limited"})
		return
	}
	var in credsInput
	if err := readJSON(w, r, &in); err != nil {
		badRequest(w, err.Error())
		return
	}

	u, err := s.store.GetUserByUsername(strings.TrimSpace(in.Username))
	// Always run a bcrypt compare so timing does not reveal whether the user
	// exists. The generic error below never says which half was wrong.
	ok := false
	if err == nil {
		ok = verifyPassword(u.PWHash, in.Password)
	} else {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(in.Password))
	}
	if !ok {
		s.login.fail(ip)
		s.log.Warn("auth: failed login", "user", in.Username, "ip", ip)
		writeJSON(w, 401, apiError{Error: "invalid username or password", Code: "bad_credentials"})
		return
	}
	s.login.success(ip)
	if err := s.startNewSession(w, r, u.ID); err != nil {
		fail(w, err)
		return
	}
	s.log.Info("auth: login", "user", u.Username, "ip", ip)
	writeJSON(w, 200, u)
}

func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		_ = s.store.DeleteSession(hashToken(c.Value))
	}
	s.clearSessionCookie(w, r)
	w.WriteHeader(204)
}

type passwordInput struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

// authPassword changes the current user's password. It revokes all existing
// sessions (via the store) and then mints a fresh one for this request, so the
// device that made the change stays logged in while every other is booted.
func (s *Server) authPassword(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		writeJSON(w, 401, apiError{Error: "authentication required", Code: "unauthorized"})
		return
	}
	var in passwordInput
	if err := readJSON(w, r, &in); err != nil {
		badRequest(w, err.Error())
		return
	}
	if !verifyPassword(u.PWHash, in.CurrentPassword) {
		writeJSON(w, 401, apiError{Error: "current password is incorrect", Code: "bad_credentials"})
		return
	}
	if err := validatePassword(in.NewPassword); err != nil {
		badRequest(w, err.Error())
		return
	}
	pwHash, err := hashPassword(in.NewPassword)
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.store.SetUserPassword(u.ID, pwHash); err != nil {
		fail(w, err)
		return
	}
	if err := s.startNewSession(w, r, u.ID); err != nil {
		fail(w, err)
		return
	}
	s.log.Info("auth: password changed", "user", u.Username)
	w.WriteHeader(204)
}
