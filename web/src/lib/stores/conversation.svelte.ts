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
// Active-conversation persistence (reload-resilience, AC #4).
//
// The active conversation id is mirrored into sessionStorage so a full page
// reload — which discards this module's $state — can restore the same
// conversation instead of minting a fresh empty one, letting the user's
// in-flight turn survive the reload. Only the id is persisted, never message
// content: history is refetched from the server (the authoritative encrypted
// store) by the page load. sessionStorage — not localStorage — scopes this to
// the tab and clears it when the tab closes, so conversation ids are not left
// on disk across browser sessions.
// ---------------------------------------------------------------------------

const ACTIVE_CONVERSATION_STORAGE_KEY = "qovira:active-conversation";

/** Best-effort read of the persisted active conversation id; null when absent or unavailable. */
function readPersistedConversationId(): string | null {
  if (typeof sessionStorage === "undefined") {
    return null;
  }
  try {
    return sessionStorage.getItem(ACTIVE_CONVERSATION_STORAGE_KEY);
  } catch {
    // sessionStorage access can throw (e.g. disabled in some privacy modes).
    return null;
  }
}

/** Best-effort mirror of the active conversation id into sessionStorage (null clears it). */
function persistConversationId(id: string | null): void {
  if (typeof sessionStorage === "undefined") {
    return;
  }
  try {
    if (id === null) {
      sessionStorage.removeItem(ACTIVE_CONVERSATION_STORAGE_KEY);
    } else {
      sessionStorage.setItem(ACTIVE_CONVERSATION_STORAGE_KEY, id);
    }
  } catch {
    // Persistence is best-effort: a failure costs reload-resilience, not correctness.
  }
}

// ---------------------------------------------------------------------------
// Module-level $state — safe: ssr=false, browser-only singleton.
// ---------------------------------------------------------------------------

// Seeded from sessionStorage so a reload restores the active conversation id;
// the page load then refetches its history from the server.
let _conversationId = $state<string | null>(readPersistedConversationId());
let _history = $state<(HistoryMessage | StreamingHistoryMessage)[]>([]);
/**
 * Non-null when the most recent AI turn failed for the active conversation.
 * Holds the error `code` string from the turn.failed SSE event.
 * Cleared by clearTurnError(), setActiveConversation(), and resetConversation().
 */
let _turnError = $state<string | null>(null);

/**
 * Sentinel id for the in-flight streaming slot. Never matches a real server id.
 *
 * Exported because it is the single anchor every in-flight turn adornment hangs
 * off: streaming text fills this slot, and tool-call chips and confirmation cards
 * are tagged with this id so they render under the slot until message.completed
 * retags them to the real message id.
 */
export const STREAMING_SENTINEL_ID = "__streaming__";

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
  persistConversationId(id);
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
 * Ensure an in-flight streaming slot exists, opening an empty one if none is open.
 *
 * A destructive tool call can suspend a turn that emitted no preceding text delta
 * (the model went straight to the tool), so applyStreamingDelta never ran and
 * there is no assistant slot to anchor the confirmation card to. The SSE client
 * calls this on confirmation.required so the suspended turn has a rendered
 * assistant message under which the card (tagged STREAMING_SENTINEL_ID) shows.
 *
 * Idempotent: a no-op when a slot is already open (the common case where the model
 * narrated before calling the tool), so it never disturbs accumulated delta text.
 */
export function ensureStreamingSlot(): void {
  if (_history.some(isStreamingSlot)) {
    return;
  }

  _history.push({
    id: STREAMING_SENTINEL_ID,
    role: "assistant",
    content: "",
    createdAt: new Date().toISOString(),
    abandoned: false,
    streaming: true,
  });
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
  if (_conversationId === null || conversationId !== _conversationId) {
    return;
  }
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
  persistConversationId(null);
}

// ---------------------------------------------------------------------------
// sendChatMessage — chat-send orchestration (Fix #1 / M4)
//
// Extracted from +page.svelte's sendMessage() so the error-handling paths are
// testable without rendering a route component.
//
// Contract:
//   - POSTs text via postFn.
//   - On success (data defined): appends the persisted message, returns null.
//   - On returned problem+json error (result.error defined — Api wrapper does
//     NOT throw): returns the original text so the caller can restore the
//     composer, and records the turn failure via setTurnFailed.
//   - On thrown network error (postFn throws): same as the returned-error path.
//
// The caller (page component) is responsible for clearing the composer before
// calling this and for restoring it when the return value is non-null.
// ---------------------------------------------------------------------------

/** Minimal shape of the server's 202 MessageResponse body. */
interface MessageResponseData {
  id: string;
  conversationId: string;
  role: string;
  content: string;
  createdAt: string;
}

/**
 * The POST function injected by callers. Matches the shape of
 * `Api.POST("/conversations/{id}/messages", …)` — a function that takes the
 * conversationId and text and resolves to the Api FetchResponse discriminated
 * union. Injected (not imported) so it is testable without mocking the module.
 */
export type PostMessageFn = (
  conversationId: string,
  text: string,
) => Promise<
  | { data: MessageResponseData; error?: never; response: Response }
  | { data?: never; error: unknown; response?: Response }
>;

/**
 * Orchestrate a chat-message send.
 *
 * @param postFn         - Injected API call (see PostMessageFn).
 * @param conversationId - The conversation to post into.
 * @param text           - The trimmed message text (non-empty, pre-validated by caller).
 * @returns `null` on success (nothing to restore); the original `text` on any
 *          error (caller should put it back in the composer).
 */
export async function sendChatMessage(
  postFn: PostMessageFn,
  conversationId: string,
  text: string,
): Promise<string | null> {
  try {
    const result = await postFn(conversationId, text);

    if (result.data !== undefined) {
      // Success: append the persisted user message.
      // appendMessage() splices before any open streaming slot so the user
      // bubble always precedes the assistant reply even when a message.delta
      // arrived over SSE while the POST was still in-flight.
      appendMessage({
        id: result.data.id,
        role: result.data.role,
        content: result.data.content,
        createdAt: result.data.createdAt,
        abandoned: false,
      });
      return null;
    }

    // result.data is undefined → server returned a problem+json error (4xx/5xx).
    // The Api wrapper resolves with { error, data: undefined } instead of
    // throwing. Without this branch the text is silently dropped — Fix #1 (M4).
    setTurnFailed(conversationId, "send_error");
    return text;
  } catch {
    // Network error (no connectivity, CORS, etc.) — same treatment as a
    // returned error: restore text + signal the turn failure.
    setTurnFailed(conversationId, "send_error");
    return text;
  }
}
