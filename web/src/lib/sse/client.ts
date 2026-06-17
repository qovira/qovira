// SSE client — one persistent connection per session.
//
// Opens a fetch-based ReadableStream connection to /events (root path, same-origin
// session cookie). Fans events to the typed router. Reconnects with exponential
// backoff on connection drops. On every (re)connect, reconciles the visible live
// collections by refetching from the REST API.
//
// Lifecycle:
//   openSseConnection()  — called after session is confirmed (onSessionReady hook)
//   closeSseConnection() — called on logout / 401 (onTearDown hook)
//
// The SSE endpoint is /events (root path, NOT /api/v1/events). Auth is ambient
// cookie (HttpOnly session cookie). No Authorization header is needed or sent.
//
// Backoff: 500ms initial, 2× per attempt, max 30s, jittered ±20% (clamped after jitter).

import { Api } from "$lib/api/index.js";
import { setReminders, upsertReminder, removeReminder } from "$lib/stores/reminders.svelte.js";
import {
  applyStreamingDelta,
  ensureStreamingSlot,
  finalizeStreamingMessage,
  getActiveConversationId,
  setConversationHistory,
  setTurnFailed,
  STREAMING_SENTINEL_ID,
} from "$lib/stores/conversation.svelte.js";
import {
  toolCallStarted,
  toolCallCompleted,
  toolCallFailed,
  finalizeToolCallsForMessage,
} from "$lib/stores/tool-calls.svelte.js";
import {
  confirmationRequired,
  confirmationExpired as storeConfirmationExpired,
  finalizeConfirmationsForMessage,
} from "$lib/stores/confirmations.svelte.js";
import { parseFrames } from "./parser.js";
import { routeEvent, type RouterHandlers, type ToolStartedPayload } from "./router.js";
import { nextBackoff, BACKOFF_INITIAL_MS } from "./backoff.js";
import { notifyReminderFired } from "$lib/notifications/reminder-fired.svelte.js";
import type { components, operations } from "$lib/api/schema.d.ts";

type ReminderItem = components["schemas"]["Reminder"];
/** Shape of a successful GET /reminders 200 response body. */
type RemindersPage = operations["listReminders"]["responses"]["200"]["content"]["application/json"];

// ---------------------------------------------------------------------------
// Connection state
// ---------------------------------------------------------------------------

/** Set when a connection is active — used to abort it. */
let _controller: AbortController | null = null;
/** Whether the SSE loop should keep running (cleared on close). */
let _active = false;

// ---------------------------------------------------------------------------
// Handler bag wired to stores
// The bag is constructed once at open time; all mutation flows through it.
// Stub handlers for events owned by future surface slices are intentionally
// empty; eslint-disable comments suppress the no-empty-function rule on them.
// ---------------------------------------------------------------------------

/** Exported for unit-testing the reminder.fired dispatch seam. */
export function makeHandlers(): RouterHandlers {
  return {
    onReminderEvent(eventName: string, payload: unknown): void {
      // reminder.deleted carries only an id; all others carry a full Reminder object.
      if (eventName === "reminder.deleted") {
        const p = payload as Record<string, unknown>;
        if (typeof p.id === "string") removeReminder(p.id);
        return;
      }
      // reminder.fired carries FiredEventPayload (no full Reminder body).
      // Raise an in-app toast and (when permitted) an OS notification.
      // Store patching for the reminders list is handled by the reminders-live-list
      // issue; this branch is additive — do not remove the created/updated/completed/deleted
      // handling below.
      if (eventName === "reminder.fired") {
        const fp = payload as { reminderId: string; title: string; dueAt: string; firedAt: string };
        if (
          typeof fp.reminderId === "string" &&
          typeof fp.title === "string" &&
          typeof fp.dueAt === "string" &&
          typeof fp.firedAt === "string"
        ) {
          notifyReminderFired(fp);
        }
        return;
      }
      // reminder.created / reminder.updated / reminder.completed carry a full Reminder.
      const r = payload as ReminderItem;
      if (typeof r.id === "string") upsertReminder(r);
    },

    onMessageDelta(conversationId: string, text: string): void {
      // Only apply if this event belongs to the currently-open conversation.
      // The server sends no messageId on deltas — only conversationId + text.
      if (conversationId !== getActiveConversationId()) return;
      applyStreamingDelta(text);
    },

    onMessageCompleted(conversationId: string, messageId: string, finishReason: string): void {
      // Only finalize for the currently-open conversation.
      if (conversationId !== getActiveConversationId()) return;
      // CompletedPayload carries messageId and finishReason; no authoritative content
      // field — the server does not re-send full content on completed. Keep delta text.
      finalizeStreamingMessage(messageId, undefined, finishReason);
      // Retag all tool-call entries from the streaming sentinel to the real messageId so
      // the cards remain visible and clickable after the streaming slot finalizes.
      finalizeToolCallsForMessage(STREAMING_SENTINEL_ID, messageId);
      // Retag confirmation cards from the streaming sentinel to the real messageId
      // so the cards persist inline under the finalized assistant turn.
      finalizeConfirmationsForMessage(STREAMING_SENTINEL_ID, messageId);
      // Heal any deltas the client missed. The completed assistant message is now
      // persisted server-side, so refetch the conversation and replace history with
      // server truth. This is what makes a mid-turn reload lose nothing: after a
      // reload the new SSE connection misses the deltas already streamed to the old
      // connection, leaving only a partial slot — the reconcile restores the full
      // text (reload-resilience, journey 5). Fire-and-forget: a failure here must
      // not disrupt the live stream; reconcileConversationHistory swallows its own errors.
      void reconcileConversationHistory();
    },

    onToolStarted(payload: ToolStartedPayload): void {
      // Guard: only apply to the currently-open conversation.
      if (payload.conversationId !== getActiveConversationId()) return;
      toolCallStarted(payload);
    },

    onToolCompleted(conversationId: string, callId: string, result: unknown): void {
      if (conversationId !== getActiveConversationId()) return;
      toolCallCompleted(conversationId, callId, result);
    },

    onToolFailed(conversationId: string, callId: string, error: string): void {
      if (conversationId !== getActiveConversationId()) return;
      toolCallFailed(conversationId, callId, error);
    },

    onConfirmationRequired(payload): void {
      // Guard on active conversation so off-screen events are ignored.
      if (payload.conversationId !== getActiveConversationId()) return;
      // A destructive tool can suspend a turn that streamed no preceding text, so
      // open an in-flight assistant slot if none exists — the card (tagged with
      // STREAMING_SENTINEL_ID) needs a rendered message to hang under.
      ensureStreamingSlot();
      confirmationRequired(payload);
    },

    onConfirmationExpired(conversationId, callId): void {
      if (conversationId !== getActiveConversationId()) return;
      storeConfirmationExpired(callId);
    },

    onTurnFailed(conversationId: string, code: string): void {
      // Guard on conversationId so only the active conversation's error is shown.
      setTurnFailed(conversationId, code);
    },
  };
}

// ---------------------------------------------------------------------------
// Reconcile — refetch live collections to heal a connection gap
//
// Runs on every successful (re)connect. A transient error here must NOT abort
// the live SSE stream — log/swallow so the stream continues.
// ---------------------------------------------------------------------------

async function reconcile(): Promise<void> {
  // 1. Refetch the full reminders list, paging through all pages.
  //    cursor-paginated (default page size 25); collect all pages then replace store.
  try {
    const allReminders: ReminderItem[] = [];
    let cursor: string | null = null;

    for (;;) {
      // exactOptionalPropertyTypes: pass cursor via spread so absent key is omitted entirely.
      const query = cursor !== null ? { cursor } : {};
      const result = await Api.GET("/reminders", { params: { query } });
      const page = result.data as RemindersPage | undefined;
      if (page?.data !== undefined) {
        allReminders.push(...page.data);
      }
      // Advance to next page or stop.
      const nextCursor: string | null = page?.pagination.nextCursor ?? null;
      if (nextCursor !== null && nextCursor !== "") {
        cursor = nextCursor;
      } else {
        break;
      }
    }

    setReminders(allReminders);
  } catch (err) {
    // Reconcile failure must not tear down the live stream — log and continue.
    console.warn("[sse] reconcile: reminders fetch failed", err);
  }

  // 2. Refetch the open conversation history, if any.
  await reconcileConversationHistory();
}

// ---------------------------------------------------------------------------
// reconcileConversationHistory — refetch the open conversation from the server
// and replace the local history with server truth.
//
// Used on (re)connect (gap healing) and on message.completed (to recover any
// deltas missed when a mid-turn reload reconnected after early deltas had
// already streamed — reload-resilience). A no-op when no conversation is open.
// Swallows its own errors so a transient failure never disrupts the live stream.
// ---------------------------------------------------------------------------

async function reconcileConversationHistory(): Promise<void> {
  const convId = getActiveConversationId();
  if (convId === null) return;
  try {
    const { data: convData } = await Api.GET("/conversations/{id}", { params: { path: { id: convId } } });
    if (convData?.messages !== undefined) {
      setConversationHistory(convData.messages);
    }
  } catch (err) {
    // Non-fatal — stream continues.
    console.warn("[sse] reconcile: conversation fetch failed", err);
  }
}

// ---------------------------------------------------------------------------
// Stream reader — reads chunks from a fetch ReadableStream
// ---------------------------------------------------------------------------

async function readStream(
  body: ReadableStream<Uint8Array>,
  signal: AbortSignal,
  handlers: RouterHandlers,
): Promise<void> {
  const reader = body.getReader();
  const decoder = new TextDecoder("utf-8");
  let buffer = "";

  try {
    while (!signal.aborted) {
      const { done, value } = await reader.read();
      if (done) break;

      buffer += decoder.decode(value, { stream: true });

      // Extract complete frames from the buffer. After parsing, retain only
      // the portion after the last "\n\n" — that is the incomplete frame tail.
      const lastDoubleLF = buffer.lastIndexOf("\n\n");
      if (lastDoubleLF !== -1) {
        const completeChunk = buffer.slice(0, lastDoubleLF + 2);
        buffer = buffer.slice(lastDoubleLF + 2);

        for (const frame of parseFrames(completeChunk)) {
          if (frame.event !== undefined && frame.data !== undefined) {
            routeEvent(frame.event, frame.data, handlers);
          }
        }
      }
    }
  } finally {
    reader.cancel().catch(() => {
      // Ignore cancel errors — the stream may already be closed.
    });
  }
}

// ---------------------------------------------------------------------------
// SSE connection loop with exponential backoff
// ---------------------------------------------------------------------------

async function connectionLoop(): Promise<void> {
  const handlers = makeHandlers();
  let backoffMs = BACKOFF_INITIAL_MS;

  while (_active) {
    _controller = new AbortController();
    const { signal } = _controller;

    try {
      const response = await fetch("/events", {
        credentials: "include",
        signal,
        headers: { Accept: "text/event-stream" },
      });

      if (!response.ok) {
        if (response.status === 401) {
          // 401 means the session has been revoked on the server. Deactivate the
          // loop now; the onUnauthorized hook (wired in the layout) will call
          // closeSseConnection() → notifyTearDown() → resetSession() → redirect.
          _active = false;
          return;
        }
        // Other non-2xx: fall through to the backoff retry path.
        throw new Error(`SSE connection failed: ${String(response.status)}`);
      }

      if (response.body === null) {
        throw new Error("SSE response has no body");
      }

      // Connection established — reset backoff immediately on a successful connection,
      // BEFORE reconcile so a transient reconcile failure never inflates the backoff.
      backoffMs = BACKOFF_INITIAL_MS;

      // Reconcile live collections. A failure here must NOT abort the live stream —
      // reconcile() swallows its own errors.
      await reconcile();

      // Consume the stream until it closes or the signal is aborted.
      await readStream(response.body, signal, handlers);
    } catch (err) {
      // Ignore abort errors (clean close / intentional disconnect).
      if ((err instanceof DOMException || err instanceof Error) && err.name === "AbortError") {
        return;
      }
      // All other errors fall through to the backoff sleep below.
    }

    // Stream closed or threw — back off and retry if still active.
    if (!_active) return;

    await sleep(backoffMs, signal);
    // closeSseConnection() may have cleared _active during the await above.
    // eslint-disable-next-line @typescript-eslint/no-unnecessary-condition
    if (!_active) return;

    backoffMs = nextBackoff(backoffMs);
  }
}

// ---------------------------------------------------------------------------
// sleep helper — resolves after ms, or immediately if signal is aborted
// ---------------------------------------------------------------------------

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise<void>((resolve) => {
    if (signal.aborted) {
      resolve();
      return;
    }
    const id = setTimeout(() => {
      resolve();
    }, ms);
    signal.addEventListener("abort", () => {
      clearTimeout(id);
      resolve();
    });
  });
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

/**
 * Open the SSE connection and begin the reconnect loop.
 * Called once after the session is confirmed (onSessionReady hook).
 * No-op if already open.
 */
export function openSseConnection(): void {
  if (_active) return;
  _active = true;
  // Run the loop in the background — do not await here; the caller is synchronous.
  void connectionLoop();
}

/**
 * Close the SSE connection and stop the reconnect loop.
 * Called on logout or when a 401 is received (onTearDown hook).
 * Safe to call when already closed.
 */
export function closeSseConnection(): void {
  _active = false;
  _controller?.abort();
  _controller = null;
}
