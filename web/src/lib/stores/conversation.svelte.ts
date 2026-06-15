// Conversation store — module-level singleton, reset on logout.
//
// Safe as a module-level singleton: ssr=false in +layout.ts, browser-only.
// Holds the currently-active conversation id and its message history, updated
// incrementally by SSE events (applyStreamingDelta, finalizeStreamingMessage,
// etc.) and replaced wholesale by the REST reconcile on (re)connect.
// The layout resets on logout / 401.

import type { components } from "$lib/api/schema.d.ts";

export type HistoryMessage = components["schemas"]["HistoryMessage"];

// ---------------------------------------------------------------------------
// StreamingHistoryMessage — extends HistoryMessage with a streaming marker.
//
// The server does NOT send a messageId on message.delta events. The store
// manages a single in-flight streaming slot: the first delta opens it, all
// subsequent deltas append to it, and message.completed finalizes it with the
// real messageId. The `streaming: true` marker distinguishes this in-flight
// entry from persisted messages.
// ---------------------------------------------------------------------------

export interface StreamingHistoryMessage extends HistoryMessage {
  /** True while the assistant reply is still streaming. Set to false on message.completed. */
  streaming: boolean;
}

// ---------------------------------------------------------------------------
// Module-level $state — safe: ssr=false, browser-only singleton.
// ---------------------------------------------------------------------------

let _conversationId = $state<string | null>(null);
let _history = $state<(HistoryMessage | StreamingHistoryMessage)[]>([]);
/**
 * Non-null when the most recent AI turn failed for the active conversation.
 * Holds the error `code` string from the turn.failed SSE event.
 * Cleared by clearTurnError(), setActiveConversation(), and resetConversation().
 */
let _turnError = $state<string | null>(null);

/** Sentinel id for the in-flight streaming slot. Never matches a real server id. */
const STREAMING_SENTINEL_ID = "__streaming__";

// ---------------------------------------------------------------------------
// Internal type predicate
// ---------------------------------------------------------------------------

function isStreamingSlot(m: HistoryMessage | StreamingHistoryMessage): m is StreamingHistoryMessage {
  // After the `"streaming" in m` check, TypeScript narrows m to StreamingHistoryMessage.
  return "streaming" in m && m.streaming;
}

// ---------------------------------------------------------------------------
// Read API
// ---------------------------------------------------------------------------

/** Returns the active conversation id, or null when none is open. */
export function getActiveConversationId(): string | null {
  return _conversationId;
}

/** Returns the message history for the active conversation (reactive). */
export function getConversationHistory(): (HistoryMessage | StreamingHistoryMessage)[] {
  return _history;
}

/**
 * Returns the current turn error code, or null when no error is set.
 * Non-null when the most recent AI turn failed for the active conversation.
 */
export function getTurnError(): string | null {
  return _turnError;
}

// ---------------------------------------------------------------------------
// Write API
// ---------------------------------------------------------------------------

/**
 * Set the active conversation and seed its initial message history.
 * Replaces any previous conversation + history. Clears any turn error.
 * Called when the user opens a conversation and its history is fetched.
 */
export function setActiveConversation(id: string, messages: HistoryMessage[]): void {
  _conversationId = id;
  _history = messages;
  _turnError = null;
}

/**
 * Replace the entire history for the current conversation.
 * Called by the REST reconcile on every (re)connect to resync from server truth.
 * Does not change the active conversation id.
 */
export function setConversationHistory(messages: HistoryMessage[]): void {
  _history = messages;
}

/**
 * Append a persisted message to the history without clobbering in-flight streaming text.
 *
 * Inserts `message` immediately before the first open streaming slot (streaming: true), or
 * pushes it to the end when no slot is open. This prevents the user-message append that
 * follows a 202 response from reordering history when a message.delta has already arrived
 * over the concurrently-open SSE connection while the POST was still in-flight.
 *
 * Use this instead of setActiveConversation(..., [...history, msg]) when appending a single
 * persisted message — a full-replace snapshot taken after an awaited POST may be stale and
 * will clobber any streaming text accumulated between the send and the 202 response.
 *
 * @param message - The persisted HistoryMessage to insert.
 */
export function appendMessage(message: HistoryMessage): void {
  const streamingIdx = _history.findIndex(isStreamingSlot);
  if (streamingIdx !== -1) {
    // Splice before the first open streaming slot so the user bubble precedes the assistant reply.
    _history.splice(streamingIdx, 0, message);
  } else {
    _history.push(message);
  }
}

/**
 * Append a streaming text delta to the single in-flight assistant message slot.
 *
 * The server sends NO messageId on message.delta — only `{conversationId, text}`.
 * The caller (SSE client) guards on conversationId before calling this.
 *
 * On the first call the streaming slot is opened (role assistant, streaming true,
 * placeholder id). All subsequent calls append to that same slot.
 */
export function applyStreamingDelta(text: string): void {
  const streamingIdx = _history.findIndex(isStreamingSlot);

  if (streamingIdx !== -1) {
    // Append to the existing streaming slot.
    const existing = _history[streamingIdx] as StreamingHistoryMessage;
    _history[streamingIdx] = { ...existing, content: existing.content + text };
  } else {
    // Open a new streaming slot.
    const slot: StreamingHistoryMessage = {
      id: STREAMING_SENTINEL_ID,
      role: "assistant",
      content: text,
      createdAt: new Date().toISOString(),
      abandoned: false,
      streaming: true,
    };
    _history.push(slot);
  }
}

/**
 * Finalize the in-flight streaming message when message.completed arrives.
 *
 * Sets the real `messageId` from the server, clears the `streaming` flag, and
 * optionally reconciles content to the server's authoritative value.
 *
 * @param messageId    - The real server-assigned message id from CompletedPayload.
 * @param content      - Optional authoritative content. When provided, replaces
 *                       accumulated delta text. When absent, delta text is kept.
 * @param finishReason - The finish reason from CompletedPayload.
 *
 * No-op when no streaming slot is open (e.g. reconcile already replaced history).
 */
export function finalizeStreamingMessage(messageId: string, content: string | undefined, finishReason: string): void {
  const streamingIdx = _history.findIndex(isStreamingSlot);

  if (streamingIdx === -1) {
    // No in-flight streaming slot — no-op.
    return;
  }

  const slot = _history[streamingIdx] as StreamingHistoryMessage;
  _history[streamingIdx] = {
    ...slot,
    id: messageId,
    content: content ?? slot.content,
    finishReason,
    streaming: false,
  };
}

/**
 * Record a turn.failed event for the active conversation.
 *
 * Guards on conversationId: if the event belongs to a different conversation,
 * it is a no-op. Also a no-op when no conversation is active.
 *
 * @param conversationId - The conversationId from the turn.failed SSE payload.
 * @param code           - The error code from the SSE payload (e.g. "turn_error").
 */
export function setTurnFailed(conversationId: string, code: string): void {
  if (_conversationId === null || conversationId !== _conversationId) return;
  _turnError = code;
}

/**
 * Clear the current turn error.
 * Call when the user dismisses the error or sends a new message.
 */
export function clearTurnError(): void {
  _turnError = null;
}

/**
 * Reset the conversation store to empty state.
 * Call on logout or 401 teardown. Safe when already empty.
 */
export function resetConversation(): void {
  _conversationId = null;
  _history = [];
  _turnError = null;
}
