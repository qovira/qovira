// Package authhttp implements the HTTP transport layer for authentication:
// login, logout-one, and logout-all.  Domain logic lives in [auth.Service] and
// [auth.Sessions]; this package only handles JSON encoding, cookie management,
// and route registration.
//
// The three endpoints live under /api/v1/auth:
//
//	POST   /api/v1/auth/login    — public; mints a session + sets cookies
//	DELETE /api/v1/auth/session  — protected; deletes the current session
//	DELETE /api/v1/auth/sessions — protected; deletes all user sessions
package authhttp

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/qovira/qovira/internal/auth"
	"github.com/qovira/qovira/internal/capability"
	"github.com/qovira/qovira/internal/httpx"
)

// ── Module ────────────────────────────────────────────────────────────────────

// Module is the auth HTTP feature slice.  It satisfies [app.Module] and mounts
// the three auth endpoints onto the shared [httpx.Router].
//
// Construct via [New]; the zero value is not valid.
type Module struct {
	svc      *auth.Service
	sessions *auth.Sessions
	cfg      auth.SessionConfig
	now      func() time.Time
	logger   *slog.Logger
}

// New constructs a [Module] backed by the provided service, sessions, config,
// clock function, and logger.  Pass [time.Now] as now in production; inject a
// synthetic clock in tests for deterministic expiry assertions.  If logger is
// nil, [slog.Default] is used.
func New(
	svc *auth.Service,
	sessions *auth.Sessions,
	cfg auth.SessionConfig,
	now func() time.Time,
	logger *slog.Logger,
) *Module {
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Module{svc: svc, sessions: sessions, cfg: cfg, now: now, logger: logger}
}

// Name implements [app.Module].
func (m *Module) Name() string { return "auth" }

// Tools implements [app.Module].  The auth slice has no capability tools.
func (m *Module) Tools() []capability.Tool { return nil }

// Routes implements [app.Module] and registers the three auth endpoints.
func (m *Module) Routes(r *httpx.Router) {
	r.HandleFunc("POST /api/v1/auth/login", m.LoginHandler())
	r.HandleFunc("DELETE /api/v1/auth/session", m.LogoutOneHandler())
	r.HandleFunc("DELETE /api/v1/auth/sessions", m.LogoutAllHandler())
}

// ── Handlers (exported for direct testing) ────────────────────────────────────

// LoginHandler returns the handler for POST /api/v1/auth/login.
// It is exported so package-level tests can call it without starting a real server.
func (m *Module) LoginHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpx.WriteProblem(w, r, httpx.MalformedBodyProblem())
			return
		}

		user, err := m.svc.Authenticate(r.Context(), body.Email, body.Password)
		if err != nil {
			if errors.Is(err, auth.ErrInvalidCredentials) {
				httpx.WriteProblem(w, r, httpx.Problem{
					Title:  "Invalid credentials",
					Status: http.StatusUnauthorized,
					Detail: "The email or password is incorrect.",
					Code:   "invalid_credentials",
				})
				return
			}
			// Infrastructure failure — do not leak detail to client; log server-side.
			httpx.WriteProblem(w, r, httpx.InternalProblem(m.logger, "internal_error", err.Error()))
			return
		}

		token, _, expiresAt, err := m.sessions.Mint(r.Context(), user.ID, m.now().UTC())
		if err != nil {
			httpx.WriteProblem(w, r, httpx.InternalProblem(m.logger, "internal_error", err.Error()))
			return
		}

		// Generate a fresh CSRF token: 32 bytes from crypto/rand encoded as base64url.
		csrfToken, err := generateCSRFToken()
		if err != nil {
			httpx.WriteProblem(w, r, httpx.InternalProblem(m.logger, "internal_error", err.Error()))
			return
		}

		maxAge := int(m.cfg.AbsoluteTTL.Seconds())

		// __Host- prefix mandates: Secure=true, Path="/", no Domain attribute.
		// HttpOnly prevents JavaScript from reading the session token.
		http.SetCookie(w, &http.Cookie{
			Name:     httpx.SessionCookieName,
			Value:    token,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
			Path:     "/",
			MaxAge:   maxAge,
		})

		// The CSRF cookie must be readable by JavaScript (no HttpOnly) so the
		// SPA can echo it as the CSRF-Token request header on unsafe methods.
		// G124: HttpOnly=false is intentional for the CSRF double-submit pattern.
		http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: HttpOnly=false intentional for CSRF double-submit
			Name:     httpx.CSRFCookieName,
			Value:    csrfToken,
			HttpOnly: false,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
			Path:     "/",
			MaxAge:   maxAge,
		})

		resp := loginResponseBody{
			ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
			User:      userBody(user),
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// LogoutOneHandler returns the handler for DELETE /api/v1/auth/session.
// It deletes the current session (identified via cookie or Bearer header) and
// clears both cookies.
func (m *Module) LogoutOneHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, _ := httpx.SessionTokenFromRequest(r)
		if token != "" {
			// Ignore not-found — idempotent.
			_ = m.sessions.DeleteByToken(r.Context(), token)
		}
		clearAuthCookies(w)
		w.WriteHeader(http.StatusNoContent)
	}
}

// LogoutAllHandler returns the handler for DELETE /api/v1/auth/sessions.
// It deletes all sessions for the authenticated user (from context) and clears
// both cookies.
func (m *Module) LogoutAllHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := httpx.PrincipalFromContext(r.Context())
		if ok {
			_ = m.sessions.DeleteAllForUser(r.Context(), principal.UserID)
		}
		clearAuthCookies(w)
		w.WriteHeader(http.StatusNoContent)
	}
}

// ── Response types ────────────────────────────────────────────────────────────

// loginResponseBody is the JSON shape returned on a successful login.
// Fields are camelCase per the HTTP house guide.
type loginResponseBody struct {
	ExpiresAt string   `json:"expiresAt"`
	User      userJSON `json:"user"`
}

// userJSON is the safe user sub-object in the login response.  It deliberately
// omits PasswordHash; it mirrors the public fields of [auth.User].
type userJSON struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`
	Timezone    string `json:"timezone"`
	Locale      string `json:"locale"`
	Language    string `json:"language"`
}

// userBody converts an [auth.User] to the wire-format [userJSON].
func userBody(u auth.User) userJSON {
	return userJSON{
		ID:          u.ID,
		Email:       u.Email,
		DisplayName: u.DisplayName,
		Role:        string(u.Role),
		Timezone:    u.Timezone,
		Locale:      u.Locale,
		Language:    u.Language,
	}
}

// ── Cookie helpers ────────────────────────────────────────────────────────────

// clearAuthCookies emits expired Set-Cookie headers for both the session and
// CSRF cookies.  MaxAge=-1 causes Go's http package to render Max-Age=0 on the
// wire, which instructs browsers to delete the cookie immediately.
func clearAuthCookies(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     httpx.SessionCookieName,
		Value:    "",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		Path:     "/",
		MaxAge:   -1,
	})
	// G124: HttpOnly=false intentional — mirrors the login cookie so the browser
	// deletes the correct jar entry (HttpOnly and non-HttpOnly are separate entries).
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: HttpOnly=false intentional for CSRF double-submit pattern
		Name:     httpx.CSRFCookieName,
		Value:    "",
		HttpOnly: false,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		Path:     "/",
		MaxAge:   -1,
	})
}

// generateCSRFToken returns a 32-byte random value encoded as base64url (no
// padding).  It uses [crypto/rand] exclusively.
func generateCSRFToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
