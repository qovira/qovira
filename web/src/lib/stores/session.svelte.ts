// Session store — module-level singleton, per-user data, RESET on logout.
//
// This is the client's sole source of auth state. The token is HttpOnly and never visible to JS; we only hold the user
// object and expiry from the server's response. Module-level $state is valid here because ssr=false is set in
// +layout.ts — this module only ever runs in the browser.

import type { components } from "$lib/api/schema.d.ts";

export type User = components["schemas"]["User"];

export interface Session {
  user: User;
  // RFC 3339 UTC, or null when the expiry is unknown (the /me boot probe does not return it — only POST /auth/login
  // does). Never fabricate a value: a made-up expiry would arm the soft pre-expiry timer against a time unrelated to
  // the real cookie lifetime.
  expiresAt: string | null;
}

// Module-level $state — safe: ssr=false, browser-only singleton.
let _session = $state<Session | null>(null);

// ---------------------------------------------------------------------------
// Read API
// ---------------------------------------------------------------------------

/** Returns the full session object, or null when unauthenticated. */
export function getSession(): Session | null {
  return _session;
}

/** Returns the authenticated user, or null when unauthenticated. */
export function getUser(): User | null {
  return _session?.user ?? null;
}

/**
 * Returns true when there is an active session. Derived from _session so it updates reactively in components that read
 * it.
 */
export function isAuthenticated(): boolean {
  return _session !== null;
}

// ---------------------------------------------------------------------------
// Write API
// ---------------------------------------------------------------------------

/**
 * Seed the session after a successful /me probe or POST /auth/login response. Replaces any existing session (e.g.
 * re-authentication after soft expiry).
 */
export function seedSession(data: Session): void {
  _session = data;
}

/**
 * Reset the session store to unauthenticated state. Call on logout or when a 401 is received via the onUnauthorized
 * hook. Safe to call when already unauthenticated.
 */
export function resetSession(): void {
  _session = null;
  // Cancel any armed soft-pre-expiry timer — after teardown there is no session to warn about, so a stale re-login
  // prompt must not fire.
  _cancelPreExpiry();
}

// ---------------------------------------------------------------------------
// Soft pre-expiry seam
//
// Consumers can register a callback to be notified shortly before the session expires (e.g. to prompt the user to
// re-authenticate). No-op until wired.
// ---------------------------------------------------------------------------

type PreExpiryCallback = () => void;
let _preExpiryCallback: PreExpiryCallback | null = null;
let _preExpiryTimerId: ReturnType<typeof setTimeout> | null = null;

/**
 * Register a callback to be invoked a given number of milliseconds before the session expires. Call with null to
 * cancel. Called at most once per session seed. Clears the previous timer on re-seed or reset.
 */
export function onPreExpiry(cb: PreExpiryCallback | null, warningMs = 60_000): void {
  _preExpiryCallback = cb;
  _schedulePreExpiry(warningMs);
}

/** Clear any pending pre-expiry timer. Safe to call when none is armed. */
function _cancelPreExpiry(): void {
  if (_preExpiryTimerId !== null) {
    clearTimeout(_preExpiryTimerId);
    _preExpiryTimerId = null;
  }
}

function _schedulePreExpiry(warningMs: number): void {
  _cancelPreExpiry();
  if (_preExpiryCallback === null || _session === null) {
    return;
  }
  // No known expiry (e.g. seeded from the /me boot probe) — nothing to schedule.
  if (_session.expiresAt === null) {
    return;
  }
  const expiresMs = new Date(_session.expiresAt).getTime();
  const triggerMs = expiresMs - warningMs - Date.now();
  if (triggerMs <= 0) {
    return;
  } // already within warning window or expired
  const cb = _preExpiryCallback;
  _preExpiryTimerId = setTimeout(() => {
    _preExpiryTimerId = null;
    cb();
  }, triggerMs);
}

// ---------------------------------------------------------------------------
// SSE lifecycle seams (no-op stubs; SSE slice wires these later)
// ---------------------------------------------------------------------------

type VoidFn = () => void;

/** Called after a successful login to signal that SSE should be opened. */
// eslint-disable-next-line @typescript-eslint/no-empty-function
let _onSessionReadyCb: VoidFn = () => {};

/** Called on logout/401 to signal that SSE should be torn down. */
// eslint-disable-next-line @typescript-eslint/no-empty-function
let _onTearDownCb: VoidFn = () => {};

/** Register the "session ready" hook (SSE open). No-op until wired by SSE slice. */
export function onSessionReady(cb: VoidFn): void {
  _onSessionReadyCb = cb;
}

/** Register the "tear down" hook (SSE close). No-op until wired by SSE slice. */
export function onTearDown(cb: VoidFn): void {
  _onTearDownCb = cb;
}

/** Invoke the session-ready hook. Called by the login flow after probe succeeds. */
export function notifySessionReady(): void {
  _onSessionReadyCb();
}

/** Invoke the tear-down hook. Called by logout and the 401 handler. */
export function notifyTearDown(): void {
  _onTearDownCb();
}
