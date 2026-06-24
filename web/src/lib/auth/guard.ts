/**
 * Route guard — pure decision function for redirect-to-login logic.
 *
 * Exemptions (never redirected, with or without a session):
 *   /login
 *   /onboarding     (exact match)
 *   /onboarding/*   (any sub-path — boundary-safe: /onboarding-x is NOT exempt)
 *
 * All other routes require an active session; unauthenticated loads are
 * redirected to /login.
 */

const EXEMPT_EXACT = new Set(["/login", "/onboarding"]);

/**
 * Returns true when the route is reachable without a session (/login,
 * /onboarding, /onboarding/*). Boundary-safe: /onboarding-x is NOT exempt.
 *
 * Exported so the root layout can render these routes immediately, without
 * waiting for the boot probe — they never depend on auth state either way.
 *
 * @param pathname - The current URL pathname (e.g. page.url.pathname)
 */
export function isExemptRoute(pathname: string): boolean {
  if (EXEMPT_EXACT.has(pathname)) {
    return true;
  }
  // Boundary-safe sub-path check: /onboarding/ prefix only, not /onboarding-x.
  if (pathname.startsWith("/onboarding/")) {
    return true;
  }
  return false;
}

/**
 * Returns true when the navigation should be redirected to /login.
 *
 * @param pathname       - The current URL pathname (e.g. page.url.pathname)
 * @param authenticated  - Whether the session store has an active session
 */
export function shouldRedirectToLogin(pathname: string, authenticated: boolean): boolean {
  if (authenticated) {
    return false;
  }
  return !isExemptRoute(pathname);
}
