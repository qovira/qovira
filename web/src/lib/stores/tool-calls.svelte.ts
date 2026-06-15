// Tool-call state store — module-level singleton, reset on logout.
//
// Holds in-progress and resolved tool-call chips for the active conversation.
// Keyed by callId. Events may arrive in any order, but tool.started always
// carries the name; tool.completed / tool.failed correlate back to it via callId.
//
// State machine per callId:
//   toolCallStarted  → state "started"   (in-progress chip)
//   toolCallCompleted → state "completed" (entity card)
//   toolCallFailed   → state "failed"    (soft error chip)
//
// Turn association:
//   Each entry carries an assistantMessageId so chips/cards stay attached to
//   the assistant turn that produced them. While the turn is streaming the id
//   is the sentinel "__streaming__". When message.completed fires,
//   finalizeToolCallsForMessage("__streaming__", realMessageId) retags all
//   in-flight entries so they persist under the real id after streaming ends.
//
// Safe as a module-level singleton: ssr=false in +layout.ts, browser-only.
// The layout resets on logout / 401.

import type { ToolStartedPayload } from "$lib/sse/router.js";

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

/** The sentinel assistant-message id used while a turn is streaming. */
export const STREAMING_SENTINEL_ID = "__streaming__";

/** A tool call in the "started" state — chip shows argsSummary. */
export interface StartedToolCall {
  readonly state: "started";
  readonly callId: string;
  readonly conversationId: string;
  readonly assistantMessageId: string;
  readonly name: string;
  readonly risk: string;
  readonly argsSummary: string;
}

/** A tool call that completed successfully. result is the raw JSON value from the server. */
export interface CompletedToolCall {
  readonly state: "completed";
  readonly callId: string;
  readonly conversationId: string;
  readonly assistantMessageId: string;
  readonly name: string;
  readonly risk: string;
  readonly argsSummary: string;
  readonly result: unknown;
}

/** A tool call that failed — error is the model-safe message only, no stack/internal detail. */
export interface FailedToolCall {
  readonly state: "failed";
  readonly callId: string;
  readonly conversationId: string;
  readonly assistantMessageId: string;
  readonly name: string;
  readonly risk: string;
  readonly argsSummary: string;
  readonly error: string;
}

/** Discriminated union of all tool-call states. */
export type ToolCallEntry = StartedToolCall | CompletedToolCall | FailedToolCall;

// ---------------------------------------------------------------------------
// Module-level $state — safe: ssr=false, browser-only singleton.
// Ordered list; insertion order is the arrival order of tool.started events.
// ---------------------------------------------------------------------------

let _toolCalls = $state<ToolCallEntry[]>([]);

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

function findIndex(callId: string): number {
  return _toolCalls.findIndex((tc) => tc.callId === callId);
}

// ---------------------------------------------------------------------------
// Read API
// ---------------------------------------------------------------------------

/**
 * Returns the ordered list of tool-call entries for the active conversation.
 * Reactive — reads derive from this automatically.
 */
export function getToolCalls(): ToolCallEntry[] {
  return _toolCalls;
}

/**
 * Returns tool-call entries for a specific assistant message turn.
 *
 * During streaming, pass STREAMING_SENTINEL_ID to get in-flight entries.
 * After finalization, pass the real messageId to get the persisted cards.
 * Reactive — reads derive from this automatically.
 *
 * @param assistantMessageId - The message id (or sentinel) of the owning turn.
 */
export function getToolCallsForMessage(assistantMessageId: string): ToolCallEntry[] {
  return _toolCalls.filter((tc) => tc.assistantMessageId === assistantMessageId);
}

// ---------------------------------------------------------------------------
// Write API
// ---------------------------------------------------------------------------

/**
 * Record a tool.started event.
 *
 * Adds a new entry in state "started" tagged with the streaming sentinel id.
 * If a duplicate callId arrives (e.g. a reconnect race), the second start is
 * ignored — the existing entry is preserved.
 *
 * @param payload - The ToolStartedPayload from the SSE event.
 */
export function toolCallStarted(payload: ToolStartedPayload): void {
  if (findIndex(payload.callId) !== -1) return; // guard: no duplicate
  const entry: StartedToolCall = {
    state: "started",
    callId: payload.callId,
    conversationId: payload.conversationId,
    assistantMessageId: STREAMING_SENTINEL_ID,
    name: payload.name,
    risk: payload.risk,
    argsSummary: payload.argsSummary,
  };
  _toolCalls.push(entry);
}

/**
 * Record a tool.completed event.
 *
 * Transitions the matching entry from "started" to "completed" and attaches
 * the raw result value. If the callId is unknown (completed without started),
 * this is a no-op — the server guarantees ordering, but defensive.
 *
 * @param conversationId - Conversation the event belongs to (for future per-conv filtering).
 * @param callId         - Correlates to the originating tool.started.
 * @param result         - Opaque JSON result from the server (may be any shape).
 */
export function toolCallCompleted(conversationId: string, callId: string, result: unknown): void {
  const idx = findIndex(callId);
  if (idx === -1) return; // no matching start — defensive no-op

  const existing = _toolCalls[idx];
  if (existing === undefined) return; // idx is valid — satisfies noUncheckedIndexedAccess
  const updated: CompletedToolCall = {
    state: "completed",
    callId: existing.callId,
    conversationId,
    assistantMessageId: existing.assistantMessageId,
    name: existing.name,
    risk: existing.risk,
    argsSummary: existing.argsSummary,
    result,
  };
  _toolCalls[idx] = updated;
}

/**
 * Record a tool.failed event.
 *
 * Transitions the matching entry from "started" to "failed" and attaches the
 * model-safe error text. No-op when the callId is unknown.
 *
 * @param conversationId - Conversation the event belongs to.
 * @param callId         - Correlates to the originating tool.started.
 * @param error          - Model-safe error message (no stack, no internal detail).
 */
export function toolCallFailed(conversationId: string, callId: string, error: string): void {
  const idx = findIndex(callId);
  if (idx === -1) return;

  const existing = _toolCalls[idx];
  if (existing === undefined) return; // idx is valid — satisfies noUncheckedIndexedAccess
  const updated: FailedToolCall = {
    state: "failed",
    callId: existing.callId,
    conversationId,
    assistantMessageId: existing.assistantMessageId,
    name: existing.name,
    risk: existing.risk,
    argsSummary: existing.argsSummary,
    error,
  };
  _toolCalls[idx] = updated;
}

/**
 * Retag all tool-call entries from the streaming sentinel to the real messageId.
 *
 * Called when message.completed fires: the streaming slot is finalized with its
 * real server-assigned messageId, and all entries tagged with `oldId` (normally
 * STREAMING_SENTINEL_ID) are retagged to `newId`. This preserves the association
 * between the tool-call chips/cards and the now-finalized assistant message so
 * cards remain visible and clickable after streaming ends.
 *
 * Only entries with `assistantMessageId === oldId` are affected — previously
 * finalized turns are untouched.
 *
 * @param oldId - The id to retag from (typically STREAMING_SENTINEL_ID).
 * @param newId - The real server-assigned messageId.
 */
export function finalizeToolCallsForMessage(oldId: string, newId: string): void {
  for (let i = 0; i < _toolCalls.length; i++) {
    const tc = _toolCalls[i];
    if (tc?.assistantMessageId === oldId) {
      _toolCalls[i] = { ...tc, assistantMessageId: newId };
    }
  }
}

/**
 * Reset the tool-call store to empty state.
 * Call on logout, 401 teardown, or when starting a new conversation.
 */
export function resetToolCalls(): void {
  _toolCalls = [];
}
