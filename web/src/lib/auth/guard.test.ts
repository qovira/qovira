/**
 * Tests for the route-guard decision function.
 *
 * shouldRedirectToLogin(pathname, authenticated) → boolean
 *
 * Rules:
 *   - /login is always exempt (never redirect)
 *   - /onboarding and /onboarding/* are always exempt
 *   - All other routes require a session; unauthenticated → true (redirect)
 *   - Authenticated users on any guarded route → false (pass through)
 *
 * Environment: browser (happy-dom) — lives under src/lib/.
 */
import { describe, expect, it } from "vitest";

import { isExemptRoute, shouldRedirectToLogin } from "./guard.js";

// ---------------------------------------------------------------------------
// isExemptRoute — routes reachable without a session (used by the layout to render immediately, without waiting on the
// boot probe)
// ---------------------------------------------------------------------------

describe("isExemptRoute", () => {
  it("treats /login as exempt", () => {
    expect(isExemptRoute("/login")).toBe(true);
  });

  it("treats /onboarding (exact) as exempt", () => {
    expect(isExemptRoute("/onboarding")).toBe(true);
  });

  it("treats /onboarding/* sub-paths as exempt", () => {
    expect(isExemptRoute("/onboarding/step-1")).toBe(true);
    expect(isExemptRoute("/onboarding/deep/nested")).toBe(true);
  });

  it("does not treat guarded routes as exempt", () => {
    expect(isExemptRoute("/")).toBe(false);
    expect(isExemptRoute("/reminders")).toBe(false);
    expect(isExemptRoute("/settings")).toBe(false);
  });

  it("is boundary-safe: /onboarding-archive is NOT exempt", () => {
    expect(isExemptRoute("/onboarding-archive")).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// Exempt routes — always accessible without a session
// ---------------------------------------------------------------------------

describe("shouldRedirectToLogin — exempt routes", () => {
  it("does not redirect /login when unauthenticated", () => {
    expect(shouldRedirectToLogin("/login", false)).toBe(false);
  });

  it("does not redirect /login when authenticated", () => {
    expect(shouldRedirectToLogin("/login", true)).toBe(false);
  });

  it("does not redirect /onboarding when unauthenticated", () => {
    expect(shouldRedirectToLogin("/onboarding", false)).toBe(false);
  });

  it("does not redirect /onboarding/step-1 when unauthenticated", () => {
    expect(shouldRedirectToLogin("/onboarding/step-1", false)).toBe(false);
  });

  it("does not redirect /onboarding/deep/nested when unauthenticated", () => {
    expect(shouldRedirectToLogin("/onboarding/deep/nested", false)).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// Guarded routes — unauthenticated → redirect
// ---------------------------------------------------------------------------

describe("shouldRedirectToLogin — guarded routes, unauthenticated", () => {
  it("redirects / when unauthenticated", () => {
    expect(shouldRedirectToLogin("/", false)).toBe(true);
  });

  it("redirects /reminders when unauthenticated", () => {
    expect(shouldRedirectToLogin("/reminders", false)).toBe(true);
  });

  it("redirects /settings when unauthenticated", () => {
    expect(shouldRedirectToLogin("/settings", false)).toBe(true);
  });

  it("redirects /settings/account when unauthenticated", () => {
    expect(shouldRedirectToLogin("/settings/account", false)).toBe(true);
  });

  it("does not confuse /onboarding-archive with /onboarding/* exemption", () => {
    // /onboarding-archive is NOT under /onboarding/ — must be guarded.
    expect(shouldRedirectToLogin("/onboarding-archive", false)).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// Guarded routes — authenticated → pass through
// ---------------------------------------------------------------------------

describe("shouldRedirectToLogin — guarded routes, authenticated", () => {
  it("does not redirect / when authenticated", () => {
    expect(shouldRedirectToLogin("/", true)).toBe(false);
  });

  it("does not redirect /reminders when authenticated", () => {
    expect(shouldRedirectToLogin("/reminders", true)).toBe(false);
  });

  it("does not redirect /settings when authenticated", () => {
    expect(shouldRedirectToLogin("/settings", true)).toBe(false);
  });
});
