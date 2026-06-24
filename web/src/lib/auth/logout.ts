/**
 * Logout — calls DELETE /auth/session, resets the session store, and
 * navigates to /login. Also invokes the tear-down seam so the SSE slice
 * can close its connection when it is wired in.
 *
 * This is the single logout authority; do NOT call resetSession + goto
 * independently from other callsites.
 */

import { goto } from "$app/navigation";

import { Api } from "$lib/api/index.js";
import { notifyTearDown, resetSession } from "$lib/stores/session.svelte.js";

/**
 * Log out the current user.
 *
 * The DELETE call clears the HttpOnly session cookie server-side.
 * We fire-and-forget the 401 result (if the session was already invalid,
 * we still complete the client-side logout).
 */
export async function logout(): Promise<void> {
  // Best-effort network call — ignore all errors (expired/already-revoked
  // sessions return 401 as a non-throwing ProblemError; genuine network
  // failures throw a TypeError). The finally block guarantees local teardown
  // runs unconditionally, even if the DELETE never reaches the server.
  try {
    await Api.DELETE("/auth/session", {});
  } catch {
    // Network failures (fetch rejected) — swallow and proceed with local teardown.
  } finally {
    // Tear down SSE connection (no-op until SSE slice wires the seam).
    notifyTearDown();

    // Clear client-side session state.
    resetSession();

    // Return to login page.
    await goto("/login");
  }
}
