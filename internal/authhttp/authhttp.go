// Package authhttp implements the HTTP transport layer for authentication and self-service account management.  Domain
// logic lives in [auth.Service] and [auth.Sessions]; this package only handles JSON encoding, cookie management, and
// route registration.
//
// The auth endpoints live under /api/v1/auth:
//
//	POST   /api/v1/auth/login    — public; mints a session + sets cookies
//	DELETE /api/v1/auth/session  — protected; deletes the current session
//	DELETE /api/v1/auth/sessions — protected; deletes all user sessions
//
// The self-service "me" endpoints live under /api/v1/me (all protected):
//
//	GET    /api/v1/me          — returns the current user record
//	PATCH  /api/v1/me          — updates display_name, timezone, locale, language (merge semantics)
//	POST   /api/v1/me/password — verifies current password, sets new hash, revokes other sessions
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

// Module is the auth HTTP feature slice.  It satisfies [app.Module] and mounts the three auth endpoints onto the shared
// [httpx.Router].
//
// Construct via [New]; the zero value is not valid.
type Module struct {
	svc      *auth.Service
	sessions *auth.Sessions
	cfg      auth.SessionConfig
	now      func() time.Time
	logger   *slog.Logger
}

// New constructs a [Module] backed by the provided service, sessions, config, clock function, and logger.  Pass
// [time.Now] as now in production; inject a synthetic clock in tests for deterministic expiry assertions.  If logger is
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

// Routes implements [app.Module] and registers the auth and me endpoints.
func (m *Module) Routes(r *httpx.Router) {
	r.HandleFunc("POST /api/v1/auth/login", m.LoginHandler())
	r.HandleFunc("DELETE /api/v1/auth/session", m.LogoutOneHandler())
	r.HandleFunc("DELETE /api/v1/auth/sessions", m.LogoutAllHandler())
	r.HandleFunc("GET /api/v1/me", m.MeHandler())
	r.HandleFunc("PATCH /api/v1/me", m.UpdateMeHandler())
	r.HandleFunc("POST /api/v1/me/password", m.ChangePasswordHandler())
}

// ── Handlers (exported for direct testing) ────────────────────────────────────

// LoginHandler returns the handler for POST /api/v1/auth/login. It is exported so package-level tests can call it
// without starting a real server.
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
			httpx.WriteProblem(w, r, httpx.InternalProblem(m.logger, "login_failed", err.Error()))
			return
		}

		token, _, expiresAt, err := m.sessions.Mint(r.Context(), user.ID, m.now().UTC())
		if err != nil {
			httpx.WriteProblem(w, r, httpx.InternalProblem(m.logger, "session_mint_failed", err.Error()))
			return
		}

		// Generate a fresh CSRF token: 32 bytes from crypto/rand encoded as base64url.
		csrfToken, err := generateCSRFToken()
		if err != nil {
			httpx.WriteProblem(w, r, httpx.InternalProblem(m.logger, "csrf_token_failed", err.Error()))
			return
		}

		maxAge := int(m.cfg.AbsoluteTTL.Seconds())

		// __Host- prefix mandates: Secure=true, Path="/", no Domain attribute. HttpOnly prevents JavaScript from
		// reading the session token. authCookie bakes in Secure/SameSite=Strict/Path so both set and clear paths use
		// identical attributes and the browser finds the same jar entry.
		http.SetCookie(w, authCookie(httpx.SessionCookieName, token, true, maxAge))

		// The CSRF cookie must be readable by JavaScript (no HttpOnly) so the SPA can echo it as the CSRF-Token
		// request header on unsafe methods. G124: HttpOnly=false is intentional for the CSRF double-submit pattern.
		http.SetCookie(w, authCookie(httpx.CSRFCookieName, csrfToken, false, maxAge)) //nolint:gosec // G124: HttpOnly=false intentional for CSRF double-submit

		resp := loginResponseBody{
			ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
			User:      userBody(user),
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// LogoutOneHandler returns the handler for DELETE /api/v1/auth/session. It deletes the current session (identified via
// cookie or Bearer header) and clears both cookies.
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

// LogoutAllHandler returns the handler for DELETE /api/v1/auth/sessions. It deletes all sessions for the authenticated
// user (from context) and clears both cookies.
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

// MeHandler returns the handler for GET /api/v1/me. It returns the current authenticated user's record as
// {"user": {...}}. The user ID is always taken from the Principal in context — never from the request body or path.
func (m *Module) MeHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := httpx.PrincipalFromContext(r.Context())
		if !ok {
			// This route is never reached unauthenticated (middleware guarantees it), so a missing principal is an
			// internal wiring error.
			httpx.WriteProblem(w, r, httpx.InternalProblem(m.logger, "get_me_principal_missing", "principal missing from context on protected route"))
			return
		}

		u, err := m.svc.GetUserByID(r.Context(), principal.UserID)
		if err != nil {
			// ErrUserNotFound here means the row vanished mid-session — treat as 500.
			httpx.WriteProblem(w, r, httpx.InternalProblem(m.logger, "get_me_user_failed", err.Error()))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(meResponseBody{User: userBody(u)})
	}
}

// UpdateMeHandler returns the handler for PATCH /api/v1/me. It applies merge semantics: only fields present in the body
// are changed; omitted fields retain their current values.  The role and email fields cannot be changed via this
// endpoint — unknown keys (including "role" and "email") are silently ignored by encoding/json.
func (m *Module) UpdateMeHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := httpx.PrincipalFromContext(r.Context())
		if !ok {
			httpx.WriteProblem(w, r, httpx.InternalProblem(m.logger, "update_me_principal_missing", "principal missing from context on protected route"))
			return
		}

		// Pointer fields: present=set, omitted=unchanged (merge semantics).
		var body struct {
			DisplayName *string `json:"displayName"`
			Timezone    *string `json:"timezone"`
			Locale      *string `json:"locale"`
			Language    *string `json:"language"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpx.WriteProblem(w, r, httpx.MalformedBodyProblem())
			return
		}

		// Load current values so omitted fields keep their stored content.
		current, err := m.svc.GetUserByID(r.Context(), principal.UserID)
		if err != nil {
			httpx.WriteProblem(w, r, httpx.InternalProblem(m.logger, "update_me_user_readback_failed", err.Error()))
			return
		}

		// Overlay non-nil fields.
		displayName := current.DisplayName
		timezone := current.Timezone
		locale := current.Locale
		language := current.Language
		if body.DisplayName != nil {
			displayName = *body.DisplayName
		}
		if body.Timezone != nil {
			timezone = *body.Timezone
		}
		if body.Locale != nil {
			locale = *body.Locale
		}
		if body.Language != nil {
			language = *body.Language
		}

		if err := m.svc.UpdateProfile(r.Context(), principal.UserID, displayName, timezone, locale, language); err != nil {
			httpx.WriteProblem(w, r, httpx.InternalProblem(m.logger, "profile_update_failed", err.Error()))
			return
		}

		// Re-fetch so updatedAt and any other server-side fields are accurate.
		updated, err := m.svc.GetUserByID(r.Context(), principal.UserID)
		if err != nil {
			httpx.WriteProblem(w, r, httpx.InternalProblem(m.logger, "update_me_result_readback_failed", err.Error()))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(meResponseBody{User: userBody(updated)})
	}
}

// ChangePasswordHandler returns the handler for POST /api/v1/me/password. It verifies the current password, validates
// and stores the new one, and then revokes every OTHER session for the user while keeping the caller's current session
// alive.  Returns 204 on success.
func (m *Module) ChangePasswordHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := httpx.PrincipalFromContext(r.Context())
		if !ok {
			httpx.WriteProblem(w, r, httpx.InternalProblem(m.logger, "change_password_principal_missing", "principal missing from context on protected route"))
			return
		}

		var body struct {
			CurrentPassword string `json:"currentPassword"`
			NewPassword     string `json:"newPassword"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpx.WriteProblem(w, r, httpx.MalformedBodyProblem())
			return
		}

		err := m.svc.ChangePassword(r.Context(), principal.UserID, body.CurrentPassword, body.NewPassword)
		if err != nil {
			if errors.Is(err, auth.ErrInvalidCredentials) {
				// Finding 2: use "validation_error" for cross-endpoint consistency.
				// Finding 4: problem-level detail is a general message; the specific "incorrect" detail lives
				// only on the field error (mirrors the weak-new-password branch shape).
				httpx.WriteProblem(w, r, httpx.ValidationProblem(
					"validation_error",
					"Request validation failed.",
					httpx.FieldError{Pointer: "/currentPassword", Detail: "The current password is incorrect."},
				))
				return
			}
			if errors.Is(err, auth.ErrPasswordTooShort) || errors.Is(err, auth.ErrPasswordTooLong) {
				httpx.WriteProblem(w, r, httpx.ValidationProblem(
					"validation_error",
					"The new password does not meet the password policy requirements.",
					httpx.FieldError{Pointer: "/newPassword", Detail: err.Error()},
				))
				return
			}
			httpx.WriteProblem(w, r, httpx.InternalProblem(m.logger, "change_password_failed", err.Error()))
			return
		}

		// Revoke every OTHER session, keeping the caller's current session alive.
		token, _ := httpx.SessionTokenFromRequest(r)
		if token == "" {
			httpx.WriteProblem(w, r, httpx.InternalProblem(m.logger, "change_password_token_missing", "session token missing from request after successful password change"))
			return
		}
		sess, err := m.sessions.Lookup(r.Context(), token)
		if err != nil {
			// The password is already changed here; without the current session ID we cannot scope the revocation,
			// so other sessions may survive. Log it as a security-relevant event so an operator can react.
			httpx.WriteProblem(w, r, httpx.InternalProblem(m.logger, "password_changed_session_lookup_failed", "password changed but current session lookup failed; other sessions were not revoked and may remain valid: "+err.Error()))
			return
		}
		if err := m.sessions.DeleteAllOtherForUser(r.Context(), principal.UserID, sess.ID); err != nil {
			// Same security-relevant state: the new hash is persisted but stale sessions may still resolve. Surface
			// enough detail server-side to alert an operator.
			httpx.WriteProblem(w, r, httpx.InternalProblem(m.logger, "password_changed_revocation_failed", "password changed but revoking other sessions failed; stale sessions may remain valid: "+err.Error()))
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// ── Response types ────────────────────────────────────────────────────────────

// loginResponseBody is the JSON shape returned on a successful login. Fields are camelCase per the HTTP house guide.
type loginResponseBody struct {
	ExpiresAt string   `json:"expiresAt"`
	User      userJSON `json:"user"`
}

// meResponseBody is the JSON shape returned by GET /api/v1/me and PATCH /api/v1/me. A single resource is returned bare
// (no collection wrapper) per the HTTP house guide.
type meResponseBody struct {
	User userJSON `json:"user"`
}

// userJSON is the safe user sub-object in the login response.  It deliberately omits PasswordHash; it mirrors the
// public fields of [auth.User].
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

// authCookie builds a cookie for the auth layer, baking in the shared security attributes (Secure, SameSite, Path)
// that must be identical on both the set and clear paths. Using a single factory prevents the browser from treating a
// mismatched clear cookie as a different jar entry, which would silently leave the original cookie intact after logout.
//
// SameSite=Strict: Qovira is a single-user self-hosted SPA with no legitimate cross-site top-level entry flow; Strict
// is a tighter default than Lax and adds defense-in-depth on top of the existing double-submit CSRF check.
func authCookie(name, value string, httpOnly bool, maxAge int) *http.Cookie {
	return &http.Cookie{ //nolint:gosec // G124: HttpOnly is parameterised; callers that pass false (CSRF cookie) carry their own nolint at the call site
		Name:     name,
		Value:    value,
		HttpOnly: httpOnly,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		Path:     "/",
		MaxAge:   maxAge,
	}
}

// clearAuthCookies emits expired Set-Cookie headers for both the session and CSRF cookies.  MaxAge=-1 causes Go's http
// package to render Max-Age=0 on the wire, which instructs browsers to delete the cookie immediately.
//
// The attributes (Secure, SameSite, Path, HttpOnly) must mirror the login set path exactly; authCookie enforces that
// single source of truth.
func clearAuthCookies(w http.ResponseWriter) {
	http.SetCookie(w, authCookie(httpx.SessionCookieName, "", true, -1))
	// G124: HttpOnly=false intentional — mirrors the login cookie so the browser deletes the correct jar entry
	// (HttpOnly and non-HttpOnly are separate entries).
	http.SetCookie(w, authCookie(httpx.CSRFCookieName, "", false, -1)) //nolint:gosec // G124: HttpOnly=false intentional for CSRF double-submit pattern
}

// generateCSRFToken returns a 32-byte random value encoded as base64url (no padding).  It uses [crypto/rand]
// exclusively.
func generateCSRFToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
