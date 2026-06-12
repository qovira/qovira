package authhttp_test

// Handler-level tests for the authhttp module: login, logout-one, logout-all.
// Tests seed a real SQLCipher store (same harness as internal/auth/*_test.go).
// Protected logout handlers receive their Principal via httpx.ContextWithPrincipal.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/app"
	"github.com/qovira/qovira/internal/auth"
	"github.com/qovira/qovira/internal/authhttp"
	"github.com/qovira/qovira/internal/config"
	"github.com/qovira/qovira/internal/httpx"
	"github.com/qovira/qovira/internal/store"
)

// ── test harness ──────────────────────────────────────────────────────────────

const testKey = "a-sufficiently-long-passphrase-for-sqlcipher"

// fastParams are low-cost argon2id params for unit test speed.
var fastParams = auth.Params{Memory: 64, Time: 1, Threads: 1, KeyLen: 32, SaltLen: 16}

// fastPolicy is a minimal password policy for tests.
var fastPolicy = auth.Policy{MinLen: 8, MaxLen: 64}

// testNow is a fixed synthetic clock for deterministic expiry assertions.
var testNow = time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)

// openStore opens a migrated SQLCipher store in a temp dir and registers cleanup.
func openStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(store.Config{
		Path:         filepath.Join(dir, "test.db"),
		Key:          testKey,
		ReadPoolSize: 4,
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	runner := store.NewRunner()
	if err := runner.Up(context.Background(), s.Writer()); err != nil {
		t.Fatalf("runner.Up: %v", err)
	}
	return s
}

// buildModule builds the authhttp.Module backed by the given store, with the
// provided now-clock (defaults to testNow when nil).
func buildModule(t *testing.T, s *store.Store, nowFn func() time.Time) *authhttp.Module {
	t.Helper()
	hasher := auth.NewHasher(fastParams)
	svc := auth.NewService(s, hasher, fastPolicy)
	sessions := auth.NewSessions(s, auth.DefaultSessionConfig)
	if nowFn == nil {
		nowFn = func() time.Time { return testNow }
	}
	return authhttp.New(svc, sessions, auth.DefaultSessionConfig, nowFn, slog.Default())
}

// createUser creates a test user and returns it. Fatally fails on error.
func createUser(t *testing.T, s *store.Store, email, password string) auth.User {
	t.Helper()
	hasher := auth.NewHasher(fastParams)
	svc := auth.NewService(s, hasher, fastPolicy)
	u, err := svc.CreateUser(context.Background(), auth.NewUser{
		Email:       email,
		DisplayName: "Test User",
		Password:    password,
		Role:        auth.RoleMember,
		Timezone:    "UTC",
		Locale:      "en-US",
		Language:    "en",
	})
	if err != nil {
		t.Fatalf("createUser(%q): %v", email, err)
	}
	return u
}

// loginBody is the JSON request body for POST /api/v1/auth/login.
// G117: the Password field is a test plaintext credential, not a secret read
// from the environment; gosec's secret-pattern match is a false positive here.
type loginBody struct { //nolint:gosec // G117: test fixture struct, not production secret handling
	Email    string `json:"email"`
	Password string `json:"password"`
}

// loginResponse is the expected JSON body for a successful login.
type loginResponse struct {
	ExpiresAt string   `json:"expiresAt"`
	User      userJSON `json:"user"`
}

type userJSON struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`
	Timezone    string `json:"timezone"`
	Locale      string `json:"locale"`
	Language    string `json:"language"`
}

// problemBody mirrors the RFC 9457 shape for assertions.
type problemBody struct {
	Status    int    `json:"status"`
	Code      string `json:"code"`
	RequestID string `json:"requestId"`
}

// serveLogin fires a login request through the module's HTTP handler directly.
func serveLogin(t *testing.T, mod *authhttp.Module, body loginBody) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body) //nolint:gosec // G117: test fixture only — no real secret being marshaled
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	// Inject a request ID so WriteProblem can fill it.
	r = r.WithContext(httpx.ContextWithRequestID(r.Context(), "test-req-id"))
	rr := httptest.NewRecorder()
	mod.LoginHandler()(rr, r)
	return rr
}

// serveDelete fires a DELETE request (session or sessions) with the given
// token injected as both cookie and context principal.
func serveDelete(
	t *testing.T,
	path string,
	handler http.HandlerFunc,
	token string,
	principal store.Principal,
) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodDelete, path, nil)
	r.AddCookie(&http.Cookie{Name: httpx.SessionCookieName, Value: token}) //nolint:gosec // G124: test request, not a real browser response
	ctx := httpx.ContextWithPrincipal(r.Context(), principal)
	ctx = httpx.ContextWithRequestID(ctx, "test-req-id")
	r = r.WithContext(ctx)
	rr := httptest.NewRecorder()
	handler(rr, r)
	return rr
}

// ── Login success ─────────────────────────────────────────────────────────────

// TestLogin_Success_200WithCookiesAndBody verifies the happy path:
//   - 200 status
//   - TWO Set-Cookie headers: __Host-qovira_session (HttpOnly) and qovira_csrf (not HttpOnly)
//   - Session token absent from the response body
//   - Body is {expiresAt, user} in camelCase
func TestLogin_Success_200WithCookiesAndBody(t *testing.T) {
	t.Parallel()

	s := openStore(t)
	const pw = "correct-horse"
	u := createUser(t, s, "login-ok@example.com", pw)
	mod := buildModule(t, s, nil)

	rr := serveLogin(t, mod, loginBody{Email: u.Email, Password: pw})

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body)
	}

	// Content-Type must be application/json.
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	// Assert both cookies are present.
	cookies := rr.Result().Cookies()
	var sessionCookie, csrfCookie *http.Cookie
	for _, c := range cookies {
		switch c.Name {
		case httpx.SessionCookieName:
			sessionCookie = c
		case httpx.CSRFCookieName:
			csrfCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("Set-Cookie for __Host-qovira_session missing")
	}
	if csrfCookie == nil {
		t.Fatal("Set-Cookie for qovira_csrf missing")
	}

	// Session cookie must be HttpOnly.
	if !sessionCookie.HttpOnly {
		t.Error("__Host-qovira_session cookie: HttpOnly = false, want true")
	}
	// Session cookie must have Path=/.
	if sessionCookie.Path != "/" {
		t.Errorf("__Host-qovira_session cookie: Path = %q, want /", sessionCookie.Path)
	}
	// Session cookie must have a non-empty value starting with "qov_".
	if !strings.HasPrefix(sessionCookie.Value, "qov_") {
		t.Errorf("__Host-qovira_session value = %q, must start with qov_", sessionCookie.Value)
	}

	// CSRF cookie must NOT be HttpOnly (SPA must read it).
	if csrfCookie.HttpOnly {
		t.Error("qovira_csrf cookie: HttpOnly = true, want false")
	}
	// CSRF cookie value must be non-empty.
	if csrfCookie.Value == "" {
		t.Error("qovira_csrf cookie value is empty")
	}

	// Token must NOT appear anywhere in the response body.
	bodyStr := rr.Body.String()
	if strings.Contains(bodyStr, sessionCookie.Value) {
		t.Error("session token leaked into response body")
	}

	// Body must decode as {expiresAt, user}.
	var resp loginResponse
	if err := json.Unmarshal([]byte(bodyStr), &resp); err != nil {
		t.Fatalf("unmarshal login response: %v; body=%s", err, bodyStr)
	}
	if resp.ExpiresAt == "" {
		t.Error("response expiresAt is empty")
	}
	if resp.User.ID != u.ID {
		t.Errorf("user.id = %q, want %q", resp.User.ID, u.ID)
	}
	if resp.User.Email != u.Email {
		t.Errorf("user.email = %q, want %q", resp.User.Email, u.Email)
	}
	if resp.User.Role != string(u.Role) {
		t.Errorf("user.role = %q, want %q", resp.User.Role, u.Role)
	}
}

// ── No enumeration: unknown email and wrong password are byte-identical ────────

// TestLogin_InvalidCredentials_UniformBody verifies that unknown-email and
// wrong-password return byte-identical 401 bodies with code "invalid_credentials".
func TestLogin_InvalidCredentials_UniformBody(t *testing.T) {
	t.Parallel()

	s := openStore(t)
	const pw = "correct-horse"
	u := createUser(t, s, "enumeration@example.com", pw)
	mod := buildModule(t, s, nil)

	rr1 := serveLogin(t, mod, loginBody{Email: "nobody@example.com", Password: pw})
	rr2 := serveLogin(t, mod, loginBody{Email: u.Email, Password: "wrong-password"})

	if rr1.Code != http.StatusUnauthorized {
		t.Errorf("unknown email: status = %d, want 401", rr1.Code)
	}
	if rr2.Code != http.StatusUnauthorized {
		t.Errorf("wrong password: status = %d, want 401", rr2.Code)
	}

	body1 := rr1.Body.String()
	body2 := rr2.Body.String()
	if body1 != body2 {
		t.Errorf("bodies differ (enumeration risk):\nunknown email: %s\nwrong password: %s", body1, body2)
	}

	var p problemBody
	if err := json.Unmarshal([]byte(body1), &p); err != nil {
		t.Fatalf("unmarshal problem: %v", err)
	}
	if p.Code != "invalid_credentials" {
		t.Errorf("code = %q, want invalid_credentials", p.Code)
	}
	if p.Status != http.StatusUnauthorized {
		t.Errorf("status in body = %d, want 401", p.Status)
	}

	// Content-Type must be application/problem+json.
	if ct := rr1.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

// ── Malformed JSON → 400 ──────────────────────────────────────────────────────

// TestLogin_MalformedJSON_Returns400 verifies that an unparseable request body
// returns 400 application/problem+json with code "malformed_body".
func TestLogin_MalformedJSON_Returns400(t *testing.T) {
	t.Parallel()

	s := openStore(t)
	mod := buildModule(t, s, nil)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader("{not json"))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(httpx.ContextWithRequestID(r.Context(), "req-malformed"))
	rr := httptest.NewRecorder()
	mod.LoginHandler()(rr, r)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}

	var p problemBody
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rr.Body)
	}
	if p.Code != "malformed_body" {
		t.Errorf("code = %q, want malformed_body", p.Code)
	}
	if p.RequestID == "" {
		t.Error("requestId missing in 400 body")
	}
}

// ── Logout one: DELETE /api/v1/auth/session ────────────────────────────────────

// TestLogoutOne_DeletesSessionAndClearsCookies verifies that:
//   - DELETE /api/v1/auth/session returns 204
//   - The session token can no longer be looked up
//   - Both cookies are cleared (Max-Age=0)
func TestLogoutOne_DeletesSessionAndClearsCookies(t *testing.T) {
	t.Parallel()

	s := openStore(t)
	const pw = "correct-horse"
	u := createUser(t, s, "logout-one@example.com", pw)
	mod := buildModule(t, s, nil)

	// Login to obtain a real session token.
	lrr := serveLogin(t, mod, loginBody{Email: u.Email, Password: pw})
	if lrr.Code != http.StatusOK {
		t.Fatalf("login: status = %d; body = %s", lrr.Code, lrr.Body)
	}
	var sessionToken string
	for _, c := range lrr.Result().Cookies() {
		if c.Name == httpx.SessionCookieName {
			sessionToken = c.Value
		}
	}
	if sessionToken == "" {
		t.Fatal("no session cookie after login")
	}

	// Logout.
	principal := store.Principal{UserID: u.ID, Role: string(u.Role)}
	rr := serveDelete(t, "/api/v1/auth/session", mod.LogoutOneHandler(), sessionToken, principal)

	if rr.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204; body = %s", rr.Code, rr.Body)
	}

	// Both cookies must be cleared.
	var sessionCleared, csrfCleared bool
	for _, c := range rr.Result().Cookies() {
		if c.Name == httpx.SessionCookieName && c.MaxAge < 0 {
			sessionCleared = true
		}
		if c.Name == httpx.CSRFCookieName && c.MaxAge < 0 {
			csrfCleared = true
		}
	}
	if !sessionCleared {
		t.Error("__Host-qovira_session cookie not cleared (MaxAge >= 0 or absent)")
	}
	if !csrfCleared {
		t.Error("qovira_csrf cookie not cleared (MaxAge >= 0 or absent)")
	}

	// Session must no longer exist.
	sessions := auth.NewSessions(s, auth.DefaultSessionConfig)
	_, lookupErr := sessions.Lookup(context.Background(), sessionToken)
	if lookupErr == nil {
		t.Error("session still exists after logout-one; want ErrSessionNotFound")
	}
}

// ── Logout all: DELETE /api/v1/auth/sessions ──────────────────────────────────

// TestLogoutAll_DeletesAllSessionsAndClearsCookies verifies that:
//   - DELETE /api/v1/auth/sessions returns 204
//   - All sessions for the user are removed
//   - Both cookies are cleared
func TestLogoutAll_DeletesAllSessionsAndClearsCookies(t *testing.T) {
	t.Parallel()

	s := openStore(t)
	const pw = "correct-horse"
	u := createUser(t, s, "logout-all@example.com", pw)

	sessions := auth.NewSessions(s, auth.DefaultSessionConfig)
	now := time.Now().UTC()

	// Mint several sessions for the same user.
	token1, _, _, _ := sessions.Mint(context.Background(), u.ID, now)
	token2, _, _, _ := sessions.Mint(context.Background(), u.ID, now)

	mod := buildModule(t, s, nil)
	principal := store.Principal{UserID: u.ID, Role: string(u.Role)}

	rr := serveDelete(t, "/api/v1/auth/sessions", mod.LogoutAllHandler(), token1, principal)

	if rr.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204; body = %s", rr.Code, rr.Body)
	}

	// Both cookies cleared.
	var sessionCleared, csrfCleared bool
	for _, c := range rr.Result().Cookies() {
		if c.Name == httpx.SessionCookieName && c.MaxAge < 0 {
			sessionCleared = true
		}
		if c.Name == httpx.CSRFCookieName && c.MaxAge < 0 {
			csrfCleared = true
		}
	}
	if !sessionCleared {
		t.Error("__Host-qovira_session cookie not cleared after logout-all")
	}
	if !csrfCleared {
		t.Error("qovira_csrf cookie not cleared after logout-all")
	}

	// Both sessions gone.
	for _, tok := range []string{token1, token2} {
		_, err := sessions.Lookup(context.Background(), tok)
		if err == nil {
			t.Errorf("session %q still exists after logout-all", tok[:10])
		}
	}
}

// ── Module interface conformance ──────────────────────────────────────────────

// TestModule_NameIsAuth verifies the module name.
func TestModule_NameIsAuth(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	mod := buildModule(t, s, nil)
	if mod.Name() != "auth" {
		t.Errorf("Name() = %q, want auth", mod.Name())
	}
}

// TestModule_ToolsIsNil verifies Tools returns nil (no capability tools yet).
func TestModule_ToolsIsNil(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	mod := buildModule(t, s, nil)
	if tools := mod.Tools(); tools != nil {
		t.Errorf("Tools() = %v, want nil", tools)
	}
}

// ── End-to-end wiring through app.New ─────────────────────────────────────────

// TestEndToEnd_LoginThroughAppNew verifies the full wiring: POST /api/v1/auth/login
// goes through the app's HTTP server (public exemption in isPublicRoute), returns 200,
// and sets the two expected cookies.
func TestEndToEnd_LoginThroughAppNew(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &config.Config{
		MasterKey:   testKey,
		DataDir:     dir,
		HTTPAddr:    "127.0.0.1:0",
		LogLevel:    "error",
		LogFormat:   "json",
		AutoMigrate: true,
	}

	// denyAllValidatorCtor is a minimal newValidator for the e2e test.
	// The auth middleware path (token validation) is exercised by the
	// Authenticator wired via AuthModuleCtor; login is public so this
	// deny-all validator is never invoked for the login endpoint.
	denyAllValidatorCtor := func(s *store.Store) httpx.TokenValidator {
		sessions := auth.NewSessions(s, auth.DefaultSessionConfig)
		return auth.NewAuthenticator(sessions)
	}

	// Build the app with the real auth module ctor.
	a, err := app.New(
		context.Background(),
		cfg,
		discardLogger(),
		denyAllValidatorCtor,
		"test",
		app.AuthModuleCtor(fastParams, fastPolicy, auth.DefaultSessionConfig, discardLogger()),
	)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = a.Server().Shutdown(ctx)
		_ = a.Store().Close()
	})

	// Seed a user directly against the store that app.New opened.
	hasher := auth.NewHasher(fastParams)
	svc := auth.NewService(a.Store(), hasher, fastPolicy)
	const pw = "correct-horse"
	u, err := svc.CreateUser(context.Background(), auth.NewUser{
		Email:       "e2e@example.com",
		DisplayName: "E2E User",
		Password:    pw,
		Role:        auth.RoleMember,
		Timezone:    "UTC",
		Locale:      "en-US",
		Language:    "en",
	})
	if err != nil {
		t.Fatalf("CreateUser (e2e): %v", err)
	}

	b, _ := json.Marshal(loginBody{Email: u.Email, Password: pw}) //nolint:gosec // G117: test fixture only
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	a.Server().Handler.ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("e2e login: status = %d, want 200; body = %s", rr.Code, rr.Body)
	}

	var foundSession, foundCSRF bool
	for _, c := range rr.Result().Cookies() {
		if c.Name == httpx.SessionCookieName {
			foundSession = true
		}
		if c.Name == httpx.CSRFCookieName {
			foundCSRF = true
		}
	}
	if !foundSession {
		t.Error("e2e: __Host-qovira_session cookie missing after login")
	}
	if !foundCSRF {
		t.Error("e2e: qovira_csrf cookie missing after login")
	}

	// The login route is public: no token required.
	// Also confirm a second POST with wrong password returns 401.
	b2, _ := json.Marshal(loginBody{Email: u.Email, Password: "wrongpass"}) //nolint:gosec // G117: test fixture only
	r2 := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(b2))
	r2.Header.Set("Content-Type", "application/json")
	rr2 := httptest.NewRecorder()
	a.Server().Handler.ServeHTTP(rr2, r2)
	if rr2.Code != http.StatusUnauthorized {
		t.Errorf("e2e wrong password: status = %d, want 401", rr2.Code)
	}
}

// discardLogger returns a logger that only emits Error level to stderr.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(
		&discardWriter{},
		&slog.HandlerOptions{Level: slog.LevelError},
	))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
