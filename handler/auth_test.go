package handler

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/KevinZhao/SmartRenew/auth"
	"github.com/KevinZhao/SmartRenew/config"
	"github.com/KevinZhao/SmartRenew/model"
)

// --- test doubles ---

type fakeStore struct {
	pingErr error
}

func (f *fakeStore) List(typeFilter, accountFilter string) ([]model.Reservation, error) {
	return []model.Reservation{{ResourceID: "sp-123", Type: "sp"}}, nil
}
func (f *fakeStore) GetAlerts(maxDays int, remindDays ...int) ([]model.Alert, error) {
	return nil, nil
}
func (f *fakeStore) Upsert(r model.Reservation) error                      { return nil }
func (f *fakeStore) Ping() error                                           { return f.pingErr }
func (f *fakeStore) ListGPUCoverage(a string) ([]model.GPUCoverage, error) { return nil, nil }

type fakeSyncer struct{ called bool }

func (f *fakeSyncer) SyncAll(ctx context.Context) (int, []error) {
	f.called = true
	return 7, nil
}

const testPassword = "correct-horse-battery"

// testPasswordHash is a pre-generated PBKDF2 hash of testPassword using the
// minimum allowed iteration count, so the suite is not dominated by KDF work.
// Production hashes use auth.DefaultIterations (600k).
const testPasswordHash = "pbkdf2-sha256$10000$zz-dCHKSnvcnFw6jdWm7jg$VKwtipsCbN7_WAh5btKtrwGdFHL2E27RF-QToqXLOls"

func testFrontend(t *testing.T) fs.FS {
	t.Helper()
	return fstest.MapFS{
		"index.html":                   {Data: []byte("<html>APP</html>")},
		"login.html":                   {Data: []byte("<html>LOGIN</html>")},
		"static/css/style.css":         {Data: []byte("body{}")},
		"static/js/login.js":           {Data: []byte("// login")},
		"static/js/app.js":             {Data: []byte("// app")},
		"static/js/vue.global.prod.js": {Data: []byte("// vue")},
	}
}

func mustAuthenticator(t *testing.T) *auth.Authenticator {
	t.Helper()
	a, err := auth.NewAuthenticator([]auth.User{{Username: "alice", PasswordHash: testPasswordHash}})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	return a
}

func newHandlerWithAuth(t *testing.T, maxAttempts int) (*Handler, *auth.SessionStore) {
	t.Helper()
	// The per-username limiter is deliberately generous here so that IP-scoped
	// tests are not perturbed by it; TestLoginRateLimitPerUsername covers it.
	return newHandlerWithLimits(t, maxAttempts, 10_000)
}

func newHandlerWithLimits(t *testing.T, maxAttempts, maxUserAttempts int) (*Handler, *auth.SessionStore) {
	t.Helper()
	sessions := auth.NewSessionStore(time.Hour)
	limiter := auth.NewLoginLimiter(maxAttempts, 15*time.Minute)
	userLimiter := auth.NewLoginLimiter(maxUserAttempts, 15*time.Minute)
	h := New(&fakeStore{}, &fakeSyncer{}, config.DefaultConfig(), testFrontend(t),
		WithAuth(mustAuthenticator(t), sessions, limiter, userLimiter, false))
	return h, sessions
}

func newHandlerNoAuth(t *testing.T) *Handler {
	t.Helper()
	return New(&fakeStore{}, &fakeSyncer{}, config.DefaultConfig(), testFrontend(t))
}

// loginOK performs a successful login and returns the session cookie.
func loginOK(t *testing.T, h *Handler) *http.Cookie {
	t.Helper()
	rec := doLogin(t, h, "alice", testPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			return c
		}
	}
	t.Fatal("no session cookie in login response")
	return nil
}

func doLogin(t *testing.T, h *Handler, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "10.0.0.1:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// --- protected endpoints ---

// protectedEndpoints are every route that must reject anonymous callers.
var protectedEndpoints = []struct {
	method string
	path   string
}{
	{http.MethodGet, "/api/reservations"},
	{http.MethodGet, "/api/alerts"},
	{http.MethodPost, "/api/sync"},
	{http.MethodGet, "/api/sync/status"},
	{http.MethodGet, "/api/export"},
	{http.MethodPost, "/api/import"},
	{http.MethodGet, "/api/gpu-coverage"},
}

func TestProtectedEndpointsRejectAnonymous(t *testing.T) {
	h, _ := newHandlerWithAuth(t, 100)
	for _, ep := range protectedEndpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			req := httptest.NewRequest(ep.method, ep.path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body: %s)", rec.Code, rec.Body.String())
			}
			// Must not leak payload data in the rejection.
			if strings.Contains(rec.Body.String(), "sp-123") {
				t.Fatalf("unauthenticated response leaked data: %s", rec.Body.String())
			}
		})
	}
}

func TestProtectedEndpointsRejectForgedCookie(t *testing.T) {
	h, _ := newHandlerWithAuth(t, 100)
	for _, ep := range protectedEndpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			req := httptest.NewRequest(ep.method, ep.path, nil)
			req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "forged-token-value"})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestProtectedEndpointsAllowValidSession(t *testing.T) {
	h, _ := newHandlerWithAuth(t, 100)
	cookie := loginOK(t, h)

	for _, ep := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/reservations"},
		{http.MethodGet, "/api/alerts"},
		{http.MethodGet, "/api/sync/status"},
		{http.MethodGet, "/api/gpu-coverage"},
		{http.MethodGet, "/api/export"},
	} {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			req := httptest.NewRequest(ep.method, ep.path, nil)
			req.AddCookie(cookie)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHealthEndpointIsPublic(t *testing.T) {
	h, _ := newHandlerWithAuth(t, 100)
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — k8s probes must not need credentials", rec.Code)
	}
}

// --- login flow ---

func TestLoginSuccessSetsHardenedCookie(t *testing.T) {
	h, sessions := newHandlerWithAuth(t, 100)
	rec := doLogin(t, h, "alice", testPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie set")
	}
	if !cookie.HttpOnly {
		t.Error("session cookie is not HttpOnly — readable by JS")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Path != "/" {
		t.Errorf("Path = %q, want /", cookie.Path)
	}
	if cookie.Value == "" {
		t.Error("empty cookie value")
	}
	if strings.Contains(cookie.Value, testPassword) || strings.Contains(cookie.Value, "alice") {
		t.Errorf("cookie value leaks credentials: %q", cookie.Value)
	}
	if sessions.Count() != 1 {
		t.Errorf("session count = %d, want 1", sessions.Count())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["username"] != "alice" {
		t.Errorf("username = %v, want alice", body["username"])
	}
	if _, ok := body["password"]; ok {
		t.Error("response echoes the password")
	}
}

func TestLoginCookieSecureFlag(t *testing.T) {
	for _, secure := range []bool{true, false} {
		t.Run(map[bool]string{true: "secure", false: "insecure"}[secure], func(t *testing.T) {
			h := New(&fakeStore{}, &fakeSyncer{}, config.DefaultConfig(), testFrontend(t),
				WithAuth(mustAuthenticator(t), auth.NewSessionStore(time.Hour), auth.NewLoginLimiter(100, time.Minute), auth.NewLoginLimiter(100, time.Minute), secure))
			rec := doLogin(t, h, "alice", testPassword)
			for _, c := range rec.Result().Cookies() {
				if c.Name == auth.SessionCookieName && c.Secure != secure {
					t.Fatalf("cookie Secure = %v, want %v", c.Secure, secure)
				}
			}
		})
	}
}

func TestLoginFailures(t *testing.T) {
	tests := []struct {
		name     string
		username string
		password string
		want     int
	}{
		{"wrong password", "alice", "nope", http.StatusUnauthorized},
		{"unknown user", "eve", testPassword, http.StatusUnauthorized},
		{"empty username", "", testPassword, http.StatusBadRequest},
		{"empty password", "alice", "", http.StatusBadRequest},
		{"password case mismatch", "alice", strings.ToUpper(testPassword), http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, sessions := newHandlerWithAuth(t, 100)
			rec := doLogin(t, h, tc.username, tc.password)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tc.want, rec.Body.String())
			}
			for _, c := range rec.Result().Cookies() {
				if c.Name == auth.SessionCookieName && c.Value != "" {
					t.Fatal("failed login issued a session cookie")
				}
			}
			if sessions.Count() != 0 {
				t.Fatalf("session count = %d after failed login, want 0", sessions.Count())
			}
		})
	}
}

func TestLoginErrorMessageDoesNotDistinguishUserFromPassword(t *testing.T) {
	h, _ := newHandlerWithAuth(t, 100)
	wrongPass := doLogin(t, h, "alice", "wrong")
	unknownUser := doLogin(t, h, "nobody", "wrong")

	if wrongPass.Code != unknownUser.Code {
		t.Fatalf("status differs: wrong password %d vs unknown user %d", wrongPass.Code, unknownUser.Code)
	}
	if wrongPass.Body.String() != unknownUser.Body.String() {
		t.Fatalf("error body differs, enables user enumeration:\n wrong password: %s\n unknown user:  %s",
			wrongPass.Body.String(), unknownUser.Body.String())
	}
}

func TestLoginRejectsMalformedBody(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"not json", "hello"},
		{"empty body", ""},
		{"json array", `["alice","pw"]`},
		{"unknown fields", `{"username":"alice","password":"pw","is_admin":true}`},
		{"null", `null`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newHandlerWithAuth(t, 100)
			req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestLoginRejectsOversizedBody(t *testing.T) {
	h, _ := newHandlerWithAuth(t, 100)
	huge := strings.Repeat("a", 64<<10)
	body := `{"username":"alice","password":"` + huge + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for oversized body", rec.Code)
	}
}

func TestLoginRejectsOverlongCredentials(t *testing.T) {
	h, _ := newHandlerWithAuth(t, 100)
	long := strings.Repeat("b", maxCredentialLen+1)
	rec := doLogin(t, h, "alice", long)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for overlong password", rec.Code)
	}
}

func TestLoginRejectsGET(t *testing.T) {
	h, _ := newHandlerWithAuth(t, 100)
	req := httptest.NewRequest(http.MethodGet, "/api/login", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// ServeMux method routing serves the SPA fallback / rejects; either way it
	// must not authenticate.
	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "username") {
		t.Fatalf("GET /api/login authenticated: %s", rec.Body.String())
	}
}

// --- rate limiting ---

func TestLoginRateLimitLocksOut(t *testing.T) {
	h, _ := newHandlerWithAuth(t, 3)
	for i := 0; i < 3; i++ {
		if rec := doLogin(t, h, "alice", "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401", i+1, rec.Code)
		}
	}
	rec := doLogin(t, h, "alice", "wrong")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 after 3 failures", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 response missing Retry-After header")
	}

	// Correct credentials must also be refused while locked out.
	rec = doLogin(t, h, "alice", testPassword)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d during lockout, want 429 even for correct password", rec.Code)
	}
}

func TestLoginRateLimitClearedBySuccess(t *testing.T) {
	h, _ := newHandlerWithAuth(t, 3)
	doLogin(t, h, "alice", "wrong")
	doLogin(t, h, "alice", "wrong")
	if rec := doLogin(t, h, "alice", testPassword); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// Counter reset: two more failures must not lock out.
	doLogin(t, h, "alice", "wrong")
	doLogin(t, h, "alice", "wrong")
	if rec := doLogin(t, h, "alice", testPassword); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — success did not reset the counter", rec.Code)
	}
}

func TestLastAttemptBeforeLockoutStillAcceptsCorrectPassword(t *testing.T) {
	// With max=3, two failures must leave one usable attempt: the user typing
	// their password correctly on the third try must get in, not a 429.
	h, _ := newHandlerWithAuth(t, 3)
	doLogin(t, h, "alice", "wrong")
	doLogin(t, h, "alice", "wrong")
	rec := doLogin(t, h, "alice", testPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 on the last attempt of the window (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestMalformedRequestDoesNotConsumeAttempts(t *testing.T) {
	// Garbage bodies are rejected before the limiter, so they cannot be used to
	// burn a legitimate user's budget.
	h, _ := newHandlerWithAuth(t, 3)
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader("garbage"))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("iteration %d status = %d, want 400", i, rec.Code)
		}
	}
	if rec := doLogin(t, h, "alice", testPassword); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — malformed requests consumed the attempt budget", rec.Code)
	}
}

func TestLoginRateLimitIsPerClient(t *testing.T) {
	h, _ := newHandlerWithAuth(t, 2)
	// Lock out one forwarded client.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"alice","password":"wrong"}`))
		req.Header.Set("X-Forwarded-For", "203.0.113.9")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"alice","password":"wrong"}`))
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked client status = %d, want 429", rec.Code)
	}

	// A different forwarded client is unaffected.
	body, _ := json.Marshal(map[string]string{"username": "alice", "password": testPassword})
	req2 := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(string(body)))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Forwarded-For", "198.51.100.7")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("other client status = %d, want 200 — lockout leaked across clients", rec2.Code)
	}
}

// loginFromIP attempts a login while presenting a spoofed forwarded client IP.
func loginFromIP(t *testing.T, h *Handler, ip, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", ip)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestLoginRateLimitPerUsernameSurvivesIPRotation(t *testing.T) {
	// X-Forwarded-For is attacker-controlled, so the per-IP limiter alone can be
	// dodged by rotating it. The per-username limiter must still bite.
	h, _ := newHandlerWithLimits(t, 3, 5)

	for i := 0; i < 5; i++ {
		ip := "203.0.113." + strconv.Itoa(i+1) // fresh IP every attempt
		if rec := loginFromIP(t, h, ip, "alice", "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401", i+1, rec.Code)
		}
	}

	rec := loginFromIP(t, h, "203.0.113.99", "alice", "wrong")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d after 5 failures from 5 distinct IPs, want 429 — username limiter did not engage", rec.Code)
	}
	// Even the correct password is refused while the username is locked.
	rec = loginFromIP(t, h, "203.0.113.100", "alice", testPassword)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 during username lockout", rec.Code)
	}
}

func TestUsernameLockoutIsCaseInsensitive(t *testing.T) {
	// Otherwise "Alice"/"ALICE"/"alice" would each get their own budget.
	h, _ := newHandlerWithLimits(t, 100, 3)
	for i, name := range []string{"alice", "ALICE", "Alice"} {
		if rec := loginFromIP(t, h, "203.0.113."+strconv.Itoa(i+1), name, "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d (%q) status = %d, want 401", i+1, name, rec.Code)
		}
	}
	if rec := loginFromIP(t, h, "203.0.113.50", "aLiCe", "wrong"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 — username lockout is case-sensitive", rec.Code)
	}
}

func TestUsernameLockoutDoesNotAffectOtherUsernames(t *testing.T) {
	h, _ := newHandlerWithLimits(t, 100, 2)
	loginFromIP(t, h, "203.0.113.1", "alice", "wrong")
	loginFromIP(t, h, "203.0.113.2", "alice", "wrong")
	if rec := loginFromIP(t, h, "203.0.113.3", "alice", "wrong"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("alice status = %d, want 429", rec.Code)
	}
	// A different username still gets its own budget (401, not 429).
	if rec := loginFromIP(t, h, "203.0.113.4", "bob", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bob status = %d, want 401 — lockout leaked across usernames", rec.Code)
	}
}

func TestUsernameLockoutClearedBySuccess(t *testing.T) {
	h, _ := newHandlerWithLimits(t, 100, 3)
	loginFromIP(t, h, "203.0.113.1", "alice", "wrong")
	loginFromIP(t, h, "203.0.113.2", "alice", "wrong")
	if rec := loginFromIP(t, h, "203.0.113.3", "alice", testPassword); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// Budget reset: two more failures must not lock out.
	loginFromIP(t, h, "203.0.113.4", "alice", "wrong")
	loginFromIP(t, h, "203.0.113.5", "alice", "wrong")
	if rec := loginFromIP(t, h, "203.0.113.6", "alice", testPassword); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — success did not clear the username counter", rec.Code)
	}
}

// --- cross-origin (CSRF) ---

func TestStateChangingRequestsRejectCrossOrigin(t *testing.T) {
	h, _ := newHandlerWithAuth(t, 100)
	cookie := loginOK(t, h)

	for _, ep := range []struct{ method, path string }{
		{http.MethodPost, "/api/sync"},
		{http.MethodPost, "/api/import"},
	} {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			req := httptest.NewRequest(ep.method, ep.path, nil)
			req.AddCookie(cookie)
			req.Header.Set("Origin", "http://evil.example.com")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 for cross-origin state change", rec.Code)
			}
		})
	}
}

func TestStateChangingRequestsAllowSameOrigin(t *testing.T) {
	h, _ := newHandlerWithAuth(t, 100)
	cookie := loginOK(t, h)

	req := httptest.NewRequest(http.MethodPost, "/api/sync", nil)
	req.AddCookie(cookie)
	// httptest.NewRequest sets Host to example.com.
	req.Header.Set("Origin", "http://"+req.Host)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 for same-origin sync (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestCrossOriginSyncDoesNotRun(t *testing.T) {
	syncer := &fakeSyncer{}
	h := New(&fakeStore{}, syncer, config.DefaultConfig(), testFrontend(t),
		WithAuth(mustAuthenticator(t), auth.NewSessionStore(time.Hour),
			auth.NewLoginLimiter(100, time.Minute), auth.NewLoginLimiter(100, time.Minute), false))
	cookie := loginOK(t, h)

	req := httptest.NewRequest(http.MethodPost, "/api/sync", nil)
	req.AddCookie(cookie)
	req.Header.Set("Origin", "http://evil.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	time.Sleep(20 * time.Millisecond)
	if syncer.called {
		t.Fatal("cross-origin request triggered an AWS sync")
	}
}

func TestCrossOriginRefererIsRejected(t *testing.T) {
	// Some browsers omit Origin on same-origin-ish navigations but always send
	// Referer, so it must be honoured as a fallback.
	h, _ := newHandlerWithAuth(t, 100)
	cookie := loginOK(t, h)

	req := httptest.NewRequest(http.MethodPost, "/api/sync", nil)
	req.AddCookie(cookie)
	req.Header.Set("Referer", "http://evil.example.com/attack.html")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for cross-origin Referer", rec.Code)
	}
}

func TestSafeMethodsAreNotOriginChecked(t *testing.T) {
	// GETs are side-effect free and are how the SPA loads data; blocking them on
	// Origin would break legitimate use without adding safety.
	h, _ := newHandlerWithAuth(t, 100)
	cookie := loginOK(t, h)

	req := httptest.NewRequest(http.MethodGet, "/api/reservations", nil)
	req.AddCookie(cookie)
	req.Header.Set("Origin", "http://evil.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestNonBrowserClientWithoutOriginIsAllowed(t *testing.T) {
	// curl/scripts send no Origin or Referer and cannot be CSRF'd; they still
	// need a valid session cookie.
	h, _ := newHandlerWithAuth(t, 100)
	cookie := loginOK(t, h)

	req := httptest.NewRequest(http.MethodPost, "/api/sync", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestLoginRejectsCrossOrigin(t *testing.T) {
	h, sessions := newHandlerWithAuth(t, 100)
	body, err := json.Marshal(map[string]string{"username": "alice", "password": testPassword})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://evil.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for cross-origin login", rec.Code)
	}
	if sessions.Count() != 0 {
		t.Fatalf("session count = %d, want 0 — cross-origin login created a session", sessions.Count())
	}
}

func TestLogoutRejectsCrossOrigin(t *testing.T) {
	h, sessions := newHandlerWithAuth(t, 100)
	cookie := loginOK(t, h)

	req := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	req.AddCookie(cookie)
	req.Header.Set("Origin", "http://evil.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if sessions.Count() != 1 {
		t.Fatalf("session count = %d, want 1 — cross-origin request forced a logout", sessions.Count())
	}
}

func TestSameOriginRequest(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		origin  string
		referer string
		want    bool
	}{
		{"no headers", "app.example.com", "", "", true},
		{"matching origin", "app.example.com", "https://app.example.com", "", true},
		{"matching origin case-insensitive", "app.example.com", "https://APP.EXAMPLE.COM", "", true},
		{"different host", "app.example.com", "https://evil.com", "", false},
		{"sibling subdomain", "app.example.com", "https://other.example.com", "", false},
		{"different port same host", "app.example.com:5000", "https://app.example.com:8080", "", false},
		{"referer fallback matches", "app.example.com", "", "https://app.example.com/page", true},
		{"referer fallback differs", "app.example.com", "", "https://evil.com/page", false},
		{"origin wins over referer", "app.example.com", "https://evil.com", "https://app.example.com/p", false},
		{"opaque origin null", "app.example.com", "null", "", false},
		{"malformed origin", "app.example.com", "::::not a url", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/sync", nil)
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.referer != "" {
				req.Header.Set("Referer", tc.referer)
			}
			if got := sameOriginRequest(req); got != tc.want {
				t.Fatalf("sameOriginRequest() = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- logout ---

func TestLogoutRevokesSession(t *testing.T) {
	h, sessions := newHandlerWithAuth(t, 100)
	cookie := loginOK(t, h)

	req := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout status = %d, want 200", rec.Code)
	}
	if sessions.Count() != 0 {
		t.Fatalf("session count = %d after logout, want 0", sessions.Count())
	}

	// Cookie must be cleared.
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookieName && c.Value == "" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("logout did not clear the session cookie")
	}

	// The old cookie must no longer grant access.
	req2 := httptest.NewRequest(http.MethodGet, "/api/reservations", nil)
	req2.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d after logout, want 401 — session was not revoked", rec2.Code)
	}
}

func TestLogoutWithoutSessionIsIdempotent(t *testing.T) {
	h, _ := newHandlerWithAuth(t, 100)
	req := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// --- /api/me ---

func TestMeUnauthenticated(t *testing.T) {
	h, _ := newHandlerWithAuth(t, 100)
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["authenticated"] != false {
		t.Errorf("authenticated = %v, want false", body["authenticated"])
	}
	if body["auth_enabled"] != true {
		t.Errorf("auth_enabled = %v, want true", body["auth_enabled"])
	}
}

func TestMeAuthenticated(t *testing.T) {
	h, _ := newHandlerWithAuth(t, 100)
	cookie := loginOK(t, h)
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["username"] != "alice" {
		t.Errorf("username = %v, want alice", body["username"])
	}
	if body["authenticated"] != true {
		t.Errorf("authenticated = %v, want true", body["authenticated"])
	}
}

// --- session expiry ---

func TestExpiredSessionIsRejected(t *testing.T) {
	sessions := auth.NewSessionStore(time.Millisecond)
	h := New(&fakeStore{}, &fakeSyncer{}, config.DefaultConfig(), testFrontend(t),
		WithAuth(mustAuthenticator(t), sessions, auth.NewLoginLimiter(100, time.Minute), auth.NewLoginLimiter(100, time.Minute), false))
	cookie := loginOK(t, h)

	time.Sleep(5 * time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/api/reservations", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for expired session", rec.Code)
	}
}

// --- static pages ---

func TestSPARedirectsAnonymousToLogin(t *testing.T) {
	h, _ := newHandlerWithAuth(t, 100)
	for _, path := range []string{"/", "/index.html"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusFound {
				t.Fatalf("status = %d, want 302 (body: %s)", rec.Code, rec.Body.String())
			}
			if loc := rec.Header().Get("Location"); loc != "/login.html" {
				t.Fatalf("Location = %q, want /login.html", loc)
			}
			if strings.Contains(rec.Body.String(), "APP") {
				t.Fatal("SPA HTML served to anonymous caller")
			}
		})
	}
}

func TestLoginPageAssetsArePublic(t *testing.T) {
	h, _ := newHandlerWithAuth(t, 100)
	for _, path := range []string{"/login.html", "/static/css/style.css", "/static/js/login.js", "/static/js/vue.global.prod.js"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 — login page would not render", rec.Code)
			}
		})
	}
}

func TestSPAServedToAuthenticatedUser(t *testing.T) {
	h, _ := newHandlerWithAuth(t, 100)
	cookie := loginOK(t, h)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "APP") {
		t.Fatalf("SPA not served to authenticated user: %s", rec.Body.String())
	}
}

func TestAppJSRequiresAuth(t *testing.T) {
	h, _ := newHandlerWithAuth(t, 100)
	req := httptest.NewRequest(http.MethodGet, "/static/js/app.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("app.js served to anonymous caller — should redirect to login")
	}
}

// --- auth disabled ---

func TestAuthDisabledLeavesEndpointsOpen(t *testing.T) {
	h := newHandlerNoAuth(t)
	for _, ep := range protectedEndpoints {
		if ep.path == "/api/import" {
			continue // needs a multipart body; covered elsewhere
		}
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			req := httptest.NewRequest(ep.method, ep.path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code == http.StatusUnauthorized {
				t.Fatalf("status = 401 with auth disabled, want the endpoint to work")
			}
		})
	}
}

func TestAuthDisabledServesSPADirectly(t *testing.T) {
	h := newHandlerNoAuth(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestAuthDisabledMeReportsDisabled(t *testing.T) {
	h := newHandlerNoAuth(t)
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["auth_enabled"] != false {
		t.Errorf("auth_enabled = %v, want false", body["auth_enabled"])
	}
}

func TestAuthDisabledLoginReturns404(t *testing.T) {
	h := newHandlerNoAuth(t)
	rec := doLogin(t, h, "alice", testPassword)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when auth is disabled", rec.Code)
	}
}

// --- clientIP ---

func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		trustProxy bool
		remoteAddr string
		headers    map[string]string
		want       string
	}{
		{"remote addr only", true, "192.0.2.5:4321", nil, "192.0.2.5"},
		{"xff single", true, "10.0.0.1:1", map[string]string{"X-Forwarded-For": "203.0.113.9"}, "203.0.113.9"},
		{"xff chain uses leftmost", true, "10.0.0.1:1", map[string]string{"X-Forwarded-For": "203.0.113.9, 70.41.3.18, 150.172.238.178"}, "203.0.113.9"},
		{"xff with spaces", true, "10.0.0.1:1", map[string]string{"X-Forwarded-For": "  203.0.113.9  "}, "203.0.113.9"},
		{"empty xff falls through to real ip", true, "10.0.0.1:1", map[string]string{"X-Forwarded-For": "", "X-Real-IP": "198.51.100.4"}, "198.51.100.4"},
		{"proxy headers ignored when untrusted", false, "10.0.0.1:1", map[string]string{"X-Forwarded-For": "203.0.113.9"}, "10.0.0.1"},
		{"malformed remote addr passed through", true, "not-an-addr", nil, "not-an-addr"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{trustProxyHeaders: tc.trustProxy}
			req := httptest.NewRequest(http.MethodPost, "/api/login", nil)
			req.RemoteAddr = tc.remoteAddr
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			if got := h.clientIP(req); got != tc.want {
				t.Fatalf("clientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIsPublicAsset(t *testing.T) {
	public := []string{"/login.html", "/static/css/style.css", "/static/js/login.js", "/static/js/vue.global.prod.js"}
	private := []string{"/", "/index.html", "/static/js/app.js", "/login.html/../index.html", "/LOGIN.HTML"}
	for _, p := range public {
		if !isPublicAsset(p) {
			t.Errorf("isPublicAsset(%q) = false, want true", p)
		}
	}
	for _, p := range private {
		if isPublicAsset(p) {
			t.Errorf("isPublicAsset(%q) = true, want false", p)
		}
	}
}

// --- sync is gated ---

func TestSyncRequiresAuthAndDoesNotRun(t *testing.T) {
	syncer := &fakeSyncer{}
	h := New(&fakeStore{}, syncer, config.DefaultConfig(), testFrontend(t),
		WithAuth(mustAuthenticator(t), auth.NewSessionStore(time.Hour), auth.NewLoginLimiter(100, time.Minute), auth.NewLoginLimiter(100, time.Minute), false))

	req := httptest.NewRequest(http.MethodPost, "/api/sync", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	// Give any (incorrectly) spawned goroutine a moment.
	time.Sleep(20 * time.Millisecond)
	if syncer.called {
		t.Fatal("anonymous request triggered an AWS sync")
	}
}
