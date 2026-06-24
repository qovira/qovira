// Tests for the session store rune logic.
// Rune environment: node + Svelte compiler (vitest project "runes"). Uses flushSync to drain $derived updates
// synchronously.
import { flushSync } from "svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  getSession,
  getUser,
  isAuthenticated,
  onPreExpiry,
  resetSession,
  seedSession,
  type User,
} from "./session.svelte.js";

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function makeUser(overrides: Partial<User> = {}): User {
  return {
    id: "u1",
    email: "user@example.com",
    displayName: "Test User",
    role: "user",
    timezone: "UTC",
    locale: "en-US",
    language: "en",
    ...overrides,
  };
}

// ---------------------------------------------------------------------------
// Reset state between tests (module-level singleton, SSR note does not apply since ssr=false; reset manually to keep
// tests isolated).
// ---------------------------------------------------------------------------

afterEach(() => {
  resetSession();
  flushSync();
});

// ---------------------------------------------------------------------------
// Default / unauthenticated state
// ---------------------------------------------------------------------------

describe("session store — initial state", () => {
  it("starts unauthenticated", () => {
    expect(isAuthenticated()).toBe(false);
  });

  it("starts with no user", () => {
    expect(getUser()).toBeNull();
  });

  it("starts with no session data", () => {
    expect(getSession()).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// seedSession — populates the store
// ---------------------------------------------------------------------------

describe("seedSession()", () => {
  it("marks the session as authenticated after seed", () => {
    const user = makeUser();
    const expiresAt = "2030-01-01T00:00:00Z";
    seedSession({ user, expiresAt });
    flushSync();
    expect(isAuthenticated()).toBe(true);
  });

  it("exposes the user after seed", () => {
    const user = makeUser({ id: "u99", email: "alice@example.com" });
    seedSession({ user, expiresAt: "2030-01-01T00:00:00Z" });
    flushSync();
    expect(getUser()).toEqual(user);
  });

  it("stores the full session object", () => {
    const user = makeUser();
    const expiresAt = "2030-06-01T12:00:00Z";
    seedSession({ user, expiresAt });
    flushSync();
    const session = getSession();
    expect(session).not.toBeNull();
    expect(session?.user).toEqual(user);
    expect(session?.expiresAt).toBe(expiresAt);
  });

  it("replaces an existing session on re-seed", () => {
    seedSession({ user: makeUser({ id: "u1" }), expiresAt: "2030-01-01T00:00:00Z" });
    seedSession({ user: makeUser({ id: "u2", email: "bob@example.com" }), expiresAt: "2031-01-01T00:00:00Z" });
    flushSync();
    expect(getUser()?.id).toBe("u2");
    expect(getSession()?.expiresAt).toBe("2031-01-01T00:00:00Z");
  });
});

// ---------------------------------------------------------------------------
// resetSession — clears the store
// ---------------------------------------------------------------------------

describe("resetSession()", () => {
  it("clears authentication state", () => {
    seedSession({ user: makeUser(), expiresAt: "2030-01-01T00:00:00Z" });
    flushSync();
    expect(isAuthenticated()).toBe(true);

    resetSession();
    flushSync();
    expect(isAuthenticated()).toBe(false);
  });

  it("clears the user", () => {
    seedSession({ user: makeUser(), expiresAt: "2030-01-01T00:00:00Z" });
    flushSync();
    resetSession();
    flushSync();
    expect(getUser()).toBeNull();
  });

  it("clears the session object", () => {
    seedSession({ user: makeUser(), expiresAt: "2030-01-01T00:00:00Z" });
    flushSync();
    resetSession();
    flushSync();
    expect(getSession()).toBeNull();
  });

  it("is a no-op when already unauthenticated", () => {
    resetSession();
    flushSync();
    expect(isAuthenticated()).toBe(false);
    expect(getUser()).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// Derived: isAuthenticated reflects seed/reset synchronously
// ---------------------------------------------------------------------------

describe("isAuthenticated() — derived from session state", () => {
  it("toggles correctly across multiple seed/reset cycles", () => {
    expect(isAuthenticated()).toBe(false);

    seedSession({ user: makeUser(), expiresAt: "2030-01-01T00:00:00Z" });
    flushSync();
    expect(isAuthenticated()).toBe(true);

    resetSession();
    flushSync();
    expect(isAuthenticated()).toBe(false);

    seedSession({ user: makeUser({ id: "u3" }), expiresAt: "2032-01-01T00:00:00Z" });
    flushSync();
    expect(isAuthenticated()).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// onPreExpiry — soft pre-expiry scheduler. A null expiry (the /me boot probe path) must NOT arm a timer; a known
// expiry fires the callback before it.
// ---------------------------------------------------------------------------

describe("onPreExpiry() — soft pre-expiry scheduler", () => {
  afterEach(() => {
    onPreExpiry(null);
    vi.useRealTimers();
  });

  it("does not arm a timer when expiresAt is null (seeded from the /me probe)", () => {
    vi.useFakeTimers();
    const cb = vi.fn();
    seedSession({ user: makeUser(), expiresAt: null });
    flushSync();

    onPreExpiry(cb, 60_000);
    // Advance well past any plausible expiry — the callback must never fire.
    vi.advanceTimersByTime(48 * 60 * 60 * 1000);
    expect(cb).not.toHaveBeenCalled();
  });

  it("fires the callback warningMs before a known expiry", () => {
    vi.useFakeTimers();
    const cb = vi.fn();
    const expiresAt = new Date(Date.now() + 5 * 60_000).toISOString();
    seedSession({ user: makeUser(), expiresAt });
    flushSync();

    onPreExpiry(cb, 60_000); // warn 60s before → fires at +4min
    vi.advanceTimersByTime(4 * 60_000 - 1);
    expect(cb).not.toHaveBeenCalled();
    vi.advanceTimersByTime(2);
    expect(cb).toHaveBeenCalledTimes(1);
  });

  it("cancels the armed timer when the session is reset (logout/401 teardown)", () => {
    vi.useFakeTimers();
    const cb = vi.fn();
    seedSession({ user: makeUser(), expiresAt: new Date(Date.now() + 5 * 60_000).toISOString() });
    flushSync();
    onPreExpiry(cb, 60_000);

    // Teardown cancels the pending prompt — no re-login warning after logout.
    resetSession();
    flushSync();
    vi.advanceTimersByTime(10 * 60_000);
    expect(cb).not.toHaveBeenCalled();
  });
});
