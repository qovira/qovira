// Confirmation store — module-level singleton, reset on logout.
//
// Holds pending, resolved, and expired tool-confirmation cards for the active
// conversation. Keyed by callId. Mirrors the tool-calls persistence pattern:
// each entry carries an assistantMessageId so the card stays attached to the
// assistant turn that produced the confirmation.required event.
//
// While the turn is suspended (waiting on the user's decision) the
// assistantMessageId is the in-flight streaming-slot sentinel
// (STREAMING_SENTINEL_ID), the SAME anchor tool chips and streaming text use, so
// the card renders under the in-flight assistant slot. The SSE client opens that
// slot (ensureStreamingSlot) when the suspending turn emitted no preceding text.
// When message.completed eventually fires (after the turn re-enters and finishes),
// finalizeConfirmationsForMessage retags all in-flight entries to the real
// messageId so the cards persist inline under the finalized turn.
//
// State machine per callId:
//   confirmation.required  → state "pending"  (approve/deny card)
//   confirmationResolved() → state "resolved" (decision recorded)
//   confirmation.expired   → state "expired"  (greyed, buttons disabled)
//
// Safe as a module-level singleton: ssr=false in +layout.ts, browser-only.
// The layout resets on logout / 401.

import { Api } from "$lib/api/index.js";
import { STREAMING_SENTINEL_ID } from "$lib/stores/conversation.svelte.js";
import type { ConfirmationRequiredPayload } from "$lib/sse/router.js";

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

/** A pending confirmation — waiting for an approve or deny decision. */
export interface PendingConfirmation {
  readonly state: "pending";
  readonly callId: string;
  readonly conversationId: string;
  readonly assistantMessageId: string;
  readonly name: string;
  readonly risk: string;
  readonly args: unknown;
  readonly expiresAt: string;
}

/** A confirmation that was resolved by the user (approved or denied). */
export interface ResolvedConfirmation {
  readonly state: "resolved";
  readonly callId: string;
  readonly conversationId: string;
  readonly assistantMessageId: string;
  readonly name: string;
  readonly risk: string;
  readonly args: unknown;
  readonly expiresAt: string;
  readonly decision: "approve" | "deny";
}

/** A confirmation that expired — either via server SSE event or local clock. */
export interface ExpiredConfirmation {
  readonly state: "expired";
  readonly callId: string;
  readonly conversationId: string;
  readonly assistantMessageId: string;
  readonly name: string;
  readonly risk: string;
  readonly args: unknown;
  readonly expiresAt: string;
}

/** Discriminated union of all confirmation states. */
export type ConfirmationEntry = PendingConfirmation | ResolvedConfirmation | ExpiredConfirmation;

// ---------------------------------------------------------------------------
// Module-level $state — safe: ssr=false, browser-only singleton.
// Ordered list; insertion order is the arrival order of confirmation.required events.
// ---------------------------------------------------------------------------

let _confirmations = $state<ConfirmationEntry[]>([]);

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

function findIndex(callId: string): number {
  return _confirmations.findIndex((c) => c.callId === callId);
}

// ---------------------------------------------------------------------------
// Read API
// ---------------------------------------------------------------------------

/**
 * Returns the ordered list of all confirmation entries.
 * Reactive — reads derive from this automatically.
 */
export function getConfirmations(): ConfirmationEntry[] {
  return _confirmations;
}

/**
 * Returns confirmation entries for a specific assistant message turn.
 *
 * During the suspension window, pass STREAMING_SENTINEL_ID to get in-flight
 * entries. After finalization, pass the real messageId.
 * Reactive — reads derive from this automatically.
 *
 * @param assistantMessageId - The message id (or sentinel) of the owning turn.
 */
export function getConfirmationsForMessage(assistantMessageId: string): ConfirmationEntry[] {
  return _confirmations.filter((c) => c.assistantMessageId === assistantMessageId);
}

// ---------------------------------------------------------------------------
// Write API
// ---------------------------------------------------------------------------

/**
 * Record a confirmation.required event.
 *
 * Adds a new entry in state "pending" tagged with the confirmation sentinel id.
 * Ignores duplicate callIds (reconnect race guard).
 *
 * @param payload - The ConfirmationRequiredPayload from the SSE event.
 */
export function confirmationRequired(payload: ConfirmationRequiredPayload): void {
  if (findIndex(payload.callId) !== -1) {
    return;
  } // guard: no duplicate
  const entry: PendingConfirmation = {
    state: "pending",
    callId: payload.callId,
    conversationId: payload.conversationId,
    assistantMessageId: STREAMING_SENTINEL_ID,
    name: payload.name,
    risk: payload.risk,
    args: payload.args,
    expiresAt: payload.expiresAt,
  };
  _confirmations.push(entry);
}

/**
 * Record that the user resolved a pending confirmation (approve or deny).
 *
 * Transitions the matching entry from "pending" to "resolved". No-op when the
 * callId is unknown or the entry is already in a terminal state.
 *
 * @param callId   - The callId from the originating confirmation.required event.
 * @param decision - The user's decision: "approve" or "deny".
 */
export function confirmationResolved(callId: string, decision: "approve" | "deny"): void {
  const idx = findIndex(callId);
  if (idx === -1) {
    return;
  }

  const existing = _confirmations[idx];
  if (existing === undefined) {
    return;
  } // satisfies noUncheckedIndexedAccess
  if (existing.state !== "pending") {
    return;
  } // already in terminal state

  const updated: ResolvedConfirmation = {
    state: "resolved",
    callId: existing.callId,
    conversationId: existing.conversationId,
    assistantMessageId: existing.assistantMessageId,
    name: existing.name,
    risk: existing.risk,
    args: existing.args,
    expiresAt: existing.expiresAt,
    decision,
  };
  _confirmations[idx] = updated;
}

/**
 * Record a confirmation.expired event from the server.
 *
 * Transitions the matching entry to "expired" and disables its buttons.
 * No-op when the callId is unknown.
 *
 * @param callId - The callId from the confirmation.expired SSE payload.
 */
export function confirmationExpired(callId: string): void {
  const idx = findIndex(callId);
  if (idx === -1) {
    return;
  }

  const existing = _confirmations[idx];
  if (existing === undefined) {
    return;
  } // satisfies noUncheckedIndexedAccess

  const updated: ExpiredConfirmation = {
    state: "expired",
    callId: existing.callId,
    conversationId: existing.conversationId,
    assistantMessageId: existing.assistantMessageId,
    name: existing.name,
    risk: existing.risk,
    args: existing.args,
    expiresAt: existing.expiresAt,
  };
  _confirmations[idx] = updated;
}

/**
 * Retag all confirmation entries from oldId to newId.
 *
 * Called alongside finalizeToolCallsForMessage when message.completed fires.
 * Confirmation entries are tagged with STREAMING_SENTINEL_ID while the turn is
 * suspended; this retag makes them persist under the real messageId after the
 * turn re-enters and the message.completed event arrives.
 *
 * @param oldId - The id to retag from (typically STREAMING_SENTINEL_ID).
 * @param newId - The real server-assigned messageId.
 */
export function finalizeConfirmationsForMessage(oldId: string, newId: string): void {
  for (let i = 0; i < _confirmations.length; i++) {
    const c = _confirmations[i];
    if (c?.assistantMessageId === oldId) {
      _confirmations[i] = { ...c, assistantMessageId: newId };
    }
  }
}

/**
 * Reset the confirmation store to empty state.
 * Call on logout, 401 teardown, or when starting a new conversation.
 */
export function resetConfirmations(): void {
  _confirmations = [];
}

// ---------------------------------------------------------------------------
// resolveConfirmation — POST to the confirmations endpoint, guarded
// ---------------------------------------------------------------------------

/**
 * POST the user's decision to the server and, only on success, record it in
 * the store.
 *
 * The Api wrapper does NOT throw on problem+json errors — it returns
 * `{ error: ProblemError }`. This function checks the returned error before
 * calling confirmationResolved, so a 409/404/422 response never falsely
 * flips the card to "Approved"/"Denied". A genuine network throw (the only
 * path the old catch handled) is also swallowed here so the caller can
 * unconditionally clear the `resolving` flag in a finally block.
 *
 * On any error the card is left in its current state (pending or, if a
 * concurrent SSE event arrived, expired) — the user can retry or the expiry
 * path greys the card via the existing mechanism.
 *
 * @param entry    - The confirmation entry being resolved.
 * @param decision - The user's decision: "approve" or "deny".
 */
export async function resolveConfirmation(entry: PendingConfirmation, decision: "approve" | "deny"): Promise<void> {
  try {
    const result = await Api.POST("/conversations/{id}/confirmations/{callId}", {
      params: { path: { id: entry.conversationId, callId: entry.callId } },
      body: { decision },
    });
    if (result.error !== undefined) {
      // Server returned a problem+json error (409 already-resolved/expired,
      // 404 unknown callId, 422 validation). Leave the card pending so the
      // user can retry or the expiry path handles it naturally.
      return;
    }
    // 202 success — record the decision optimistically.
    // The harness re-enters via SSE after this.
    confirmationResolved(entry.callId, decision);
  } catch {
    // Genuine network throw (fetch failed, no response).
    // Leave the card pending so the user can retry.
  }
}
