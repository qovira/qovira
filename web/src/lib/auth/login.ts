/**
 * Login flow — the testable core of the /login page.
 *
 * Posts credentials to POST /auth/login (the server sets the HttpOnly session
 * cookie + readable CSRF cookie; the token is never in the body), then probes
 * GET /me for the full user, seeds the session store, and signals the
 * "session ready" seam. Returns a discriminated result the component maps to
 * UI state — navigation stays in the component, so this module has no
 * dependency on $app and is unit-testable by stubbing fetch.
 */

import { Api, ProblemError } from "$lib/api/index.js";
import { notifySessionReady, seedSession } from "$lib/stores/session.svelte.js";
import {
  login_error_invalid_credentials,
  login_error_unexpected,
  login_error_session_verify,
} from "$lib/paraglide/messages.js";

export interface LoginFieldErrors {
  email?: string;
  password?: string;
}

export type LoginResult = { ok: true } | { ok: false; fieldErrors?: LoginFieldErrors; message?: string };

/**
 * Attempt to log in. On success the session store is seeded and the
 * session-ready seam fired; the caller navigates home. On failure a
 * user-safe result is returned (field-level for 422, a message otherwise).
 *
 * A raw network/parse failure (Api rejects rather than returning a
 * ProblemError) propagates — the caller wraps the call to surface a generic
 * error and always reset its loading state.
 */
export async function performLogin(email: string, password: string): Promise<LoginResult> {
  const { data: loginData, error: loginError } = await Api.POST("/auth/login", {
    body: { email, password },
  });

  if (loginError instanceof ProblemError) {
    if (loginError.status === 422 && loginError.errors) {
      // Map field-level validation errors from JSON Pointer paths to field names.
      const fieldErrors: LoginFieldErrors = {};
      for (const fe of loginError.errors) {
        if (fe.pointer === "/email") fieldErrors.email = fe.detail;
        else if (fe.pointer === "/password") fieldErrors.password = fe.detail;
      }
      if (fieldErrors.email !== undefined || fieldErrors.password !== undefined) {
        return { ok: false, fieldErrors };
      }
      return { ok: false, message: loginError.detail };
    }
    // 401 = invalid credentials (uniform, user-safe); other = generic detail.
    return {
      ok: false,
      message: loginError.status === 401 ? login_error_invalid_credentials() : loginError.detail,
    };
  }

  if (!loginData) {
    return { ok: false, message: login_error_unexpected() };
  }

  // Probe /me to get the full user object and confirm the cookie is set.
  const { data: meData } = await Api.GET("/me", {});

  if (!meData) {
    return {
      ok: false,
      message: login_error_session_verify(),
    };
  }

  // Seed the session store with the user and the real expiry from the login body.
  seedSession({ user: meData.user, expiresAt: loginData.expiresAt });

  // Signal the "session ready" seam so the SSE slice can open its connection
  // when wired. No-op until the SSE slice registers its callback.
  notifySessionReady();

  return { ok: true };
}
