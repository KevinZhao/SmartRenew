package handler

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/KevinZhao/SmartRenew/auth"
)

// loginRequest is the JSON body of POST /api/login.
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

const (
	maxLoginBodyBytes = 4 << 10 // 4 KiB is ample for {username,password}
	maxCredentialLen  = 256
)

// requireAuth wraps next so that only requests with a valid session pass.
// When auth is disabled the wrapper is a no-op.
func (h *Handler) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	if h.sessions == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := h.currentSession(r); !ok {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if !isSafeMethod(r.Method) && !sameOriginRequest(r) {
			// SameSite=Lax already blocks cross-site form POSTs, but "site"
			// ignores the port and covers sibling subdomains — so another app
			// on the same registrable domain could still forge one. Checking
			// Origin/Referer closes that gap for state-changing calls.
			writeError(w, http.StatusForbidden, "cross-origin request rejected")
			return
		}
		next(w, r)
	}
}

func isSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// sameOriginRequest reports whether a state-changing request originated from
// this app's own pages. It compares the Origin header (or Referer as a
// fallback) against the request's own host.
func sameOriginRequest(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}
	if origin == "" {
		// Non-browser clients (curl, scripts) send neither header. They also
		// cannot be tricked into a CSRF, so allow them through — the session
		// cookie remains required.
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	// Compare against the Host the client addressed, which behind a proxy is
	// the public hostname the browser also used to build Origin.
	return strings.EqualFold(u.Host, r.Host)
}

// requireAuthPage wraps a page handler, redirecting unauthenticated browsers to
// the login page instead of returning a JSON 401.
func (h *Handler) requireAuthPage(next http.HandlerFunc) http.HandlerFunc {
	if h.sessions == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := h.currentSession(r); ok {
			next(w, r)
			return
		}
		if isPublicAsset(r.URL.Path) {
			next(w, r)
			return
		}
		http.Redirect(w, r, "/login.html", http.StatusFound)
	}
}

// isPublicAsset reports whether a static path may be served without a session.
// The login page needs its own CSS/JS, and Vue is loaded by it.
func isPublicAsset(p string) bool {
	switch p {
	case "/login.html", "/static/css/style.css", "/static/js/login.js", "/static/js/vue.global.prod.js":
		return true
	}
	return false
}

func (h *Handler) currentSession(r *http.Request) (auth.Session, bool) {
	if h.sessions == nil {
		return auth.Session{}, false
	}
	c, err := r.Cookie(auth.SessionCookieName)
	if err != nil {
		return auth.Session{}, false
	}
	return h.sessions.Get(c.Value)
}

// login validates credentials and issues a session cookie.
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil || h.authenticator == nil {
		writeError(w, http.StatusNotFound, "authentication is disabled")
		return
	}
	if !sameOriginRequest(r) {
		writeError(w, http.StatusForbidden, "cross-origin request rejected")
		return
	}

	// Parse and validate before touching the limiters, so a malformed request
	// does not consume a legitimate user's attempt budget.
	var req loginRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLoginBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "username and password are required")
		return
	}
	if len(req.Username) > maxCredentialLen || len(req.Password) > maxCredentialLen {
		writeError(w, http.StatusBadRequest, "username or password too long")
		return
	}

	// Reserve an attempt on both limiters up front. Reserve counts the attempt
	// as it checks, so concurrent requests cannot all slip past the threshold
	// during the ~0.3s the password check takes.
	clientKey := h.clientIP(r)
	if allowed, retryAfter := h.loginLimiter.Reserve(clientKey); !allowed {
		h.writeLockout(w, retryAfter)
		return
	}
	// Second limiter keyed by username: X-Forwarded-For is client-controlled, so
	// an attacker can rotate it to dodge the per-IP lockout. Usernames are fixed
	// in config and therefore guessable, so throttling them too closes that hole.
	// The threshold is looser than the per-IP one to limit how easily a third
	// party can lock a legitimate user out.
	userKey := "user:" + strings.ToLower(strings.TrimSpace(req.Username))
	if allowed, retryAfter := h.userLimiter.Reserve(userKey); !allowed {
		h.writeLockout(w, retryAfter)
		return
	}

	username, ok := h.authenticator.Verify(req.Username, req.Password)
	if !ok {
		slog.Warn("login failed", "username", req.Username, "client", clientKey)
		// Same message for unknown user and wrong password — no enumeration.
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}

	token, sess, err := h.sessions.Create(username)
	if err != nil {
		slog.Error("create session failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not start session")
		return
	}
	h.loginLimiter.Succeed(clientKey)
	h.userLimiter.Succeed(userKey)
	h.setSessionCookie(w, token, sess.ExpiresAt)
	slog.Info("login ok", "username", username, "client", clientKey)
	writeJSON(w, map[string]any{
		"username":   username,
		"expires_at": sess.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// writeLockout renders a 429 with a Retry-After hint.
func (h *Handler) writeLockout(w http.ResponseWriter, retryAfter time.Duration) {
	secs := strconv.Itoa(int(retryAfter.Seconds()) + 1)
	w.Header().Set("Retry-After", secs)
	writeError(w, http.StatusTooManyRequests, "too many failed login attempts, retry in "+secs+"s")
}

// logout revokes the current session and clears the cookie.
func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, http.StatusNotFound, "authentication is disabled")
		return
	}
	// A forced cross-site logout is only a nuisance, but the check is free.
	if !sameOriginRequest(r) {
		writeError(w, http.StatusForbidden, "cross-origin request rejected")
		return
	}
	if c, err := r.Cookie(auth.SessionCookieName); err == nil {
		h.sessions.Delete(c.Value)
	}
	h.clearSessionCookie(w)
	writeJSON(w, map[string]string{"status": "ok"})
}

// me reports the current authentication state, letting the SPA decide whether
// to render the app or redirect to the login page.
func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeJSON(w, map[string]any{"auth_enabled": false, "authenticated": true})
		return
	}
	sess, ok := h.currentSession(r)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"auth_enabled": true, "authenticated": false})
		return
	}
	writeJSON(w, map[string]any{
		"auth_enabled":  true,
		"authenticated": true,
		"username":      sess.Username,
		"expires_at":    sess.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

func (h *Handler) setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   h.cookieSecure,
		SameSite: http.SameSiteLaxMode,
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
	})
}

func (h *Handler) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   h.cookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// clientIP extracts the caller address used as the rate-limit key. When
// trustProxyHeaders is set (the app usually sits behind CloudFront/ALB, where
// RemoteAddr is the proxy and would lock out every user at once) the
// left-most X-Forwarded-For entry is used. That header is client-controlled, so
// a determined attacker can rotate it to evade the per-IP lockout; the PBKDF2
// work factor is the second line of defense there.
func (h *Handler) clientIP(r *http.Request) string {
	if h.trustProxyHeaders {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first := strings.TrimSpace(strings.Split(xff, ",")[0]); first != "" {
				return first
			}
		}
		if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
			return xr
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
