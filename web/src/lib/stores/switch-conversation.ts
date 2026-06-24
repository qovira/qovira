// switchConversation / startNewConversation — the single clean path for
// switching the active conversation.
//
// Both paths share the same pre-switch teardown: reset the tool-calls and
// confirmations stores so cards from the previous conversation do not leak
// into the newly-opened thread. This is the critical invariant the issue
// requires (previously the two stores only reset on logout).
//
// switchConversation(id) — new ordering closes the SSE race window:
//   1. setActiveConversation(id, []) — pivot active id + clear history/turnError
//      BEFORE the async fetch, so SSE guards (conversationId !== getActiveConversationId())
//      already reject in-flight old-conversation events during the fetch window.
//   2. resetToolCalls() + resetConfirmations() — purge previous thread's cards.
//   3. const { data, error } = await Api.GET("/conversations/{id}", …).
//   4. On success:   setConversationHistory(data.messages) — seed history without re-pivoting.
//   5. On 404/empty: leave history empty — legitimately blank thread.
//   6. On real error (non-404): setTurnFailed(id, code) — calm error signal,
//      not a silent blank indistinguishable from an empty new conversation.
//
// startNewConversation():
//   1. Reset tool-calls and confirmations.
//   2. Mint a fresh ULID (same pattern as +page.ts on first load).
//   3. Call setActiveConversation(newId, []) — the server creates the
//      conversation implicitly on the first POST /conversations/{id}/messages.

import { ulid } from "ulid";
import { Api, ProblemError } from "$lib/api/index.js";
import { setActiveConversation, setConversationHistory, setTurnFailed } from "$lib/stores/conversation.svelte.js";
import { resetToolCalls } from "$lib/stores/tool-calls.svelte.js";
import { resetConfirmations } from "$lib/stores/confirmations.svelte.js";

// ---------------------------------------------------------------------------
// Internal teardown helper — run before every switch.
// ---------------------------------------------------------------------------

function tearDownCurrentConversation(): void {
  resetToolCalls();
  resetConfirmations();
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

/**
 * Switch the active conversation to `id`.
 *
 * Pivots the active id BEFORE the async fetch so in-flight SSE events for the
 * previous conversation are rejected by the client guards for the entire fetch
 * window. Resets tool-calls and confirmations to prevent card leakage, fetches
 * the history, and seeds the conversation store.
 *
 * Error handling:
 *   - 404 / missing body: leaves history empty — correct for a freshly-minted
 *     conversation that hasn't had a message sent yet.
 *   - Real error (non-404 status present): sets a calm turn-error signal via
 *     setTurnFailed so the UI surfaces "something went wrong" rather than
 *     silently presenting an empty thread indistinguishable from a new one.
 *
 * @param id - The conversation id to switch to.
 */
export async function switchConversation(id: string): Promise<void> {
  // Step 1: Pivot the active id FIRST. This narrows the SSE race window:
  // getActiveConversationId() already returns `id` during the fetch below, so
  // the SSE guards reject any residual events from the previous conversation.
  // setActiveConversation also clears turnError and history in one call.
  setActiveConversation(id, []);

  // Step 2: Purge the previous thread's tool/confirmation cards.
  tearDownCurrentConversation();

  // Step 3: Fetch the conversation history.
  const { data, error } = await Api.GET("/conversations/{id}", {
    params: { path: { id } },
  });

  // Step 4: Seed history or surface an error.
  if (data !== undefined) {
    // Success — seed history without re-pivoting (active id is already correct).
    setConversationHistory(data.messages);
  } else {
    const errStatus = error instanceof ProblemError ? error.status : undefined;
    if (errStatus !== undefined && errStatus !== 404) {
      // Real server error on an existing conversation — surface a calm error state
      // rather than silently leaving history empty (which looks like a new thread).
      setTurnFailed(id, "fetch_error");
    }
    // No error or 404: leave history calmly empty — a new / absent thread.
  }
}

/**
 * Start a new (empty) conversation.
 *
 * Resets the tool-calls and confirmations stores, mints a fresh ULID, and
 * seeds the conversation store with an empty history. The server creates the
 * conversation record implicitly when the first POST /conversations/{id}/messages
 * is sent — no API call is needed here.
 */
export function startNewConversation(): void {
  tearDownCurrentConversation();
  const newId = ulid();
  setActiveConversation(newId, []);
}
