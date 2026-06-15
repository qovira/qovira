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

// ---------------------------------------------------------------------------
// Write API
// ---------------------------------------------------------------------------

/**
 * Set the active conversation and seed its initial message history.
 * Replaces any previous conversation + history.
 * Called when the user opens a conversation and its history is fetched.
 */
export function setActiveConversation(id: string, messages: HistoryMessage[]): void {
  _conversationId = id;
  _history = messages;
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
 * Reset the conversation store to empty state.
 * Call on logout or 401 teardown. Safe when already empty.
 */
export function resetConversation(): void {
  _conversationId = null;
  _history = [];
}
