// Tests for the SSE client — backoff utility and client-level teardown / reconnect behaviour.
// Environment: browser (happy-dom) — included via src/lib/**/*.test.ts pattern.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { nextBackoff, BACKOFF_INITIAL_MS } from "./backoff.js";
import { openSseConnection, closeSseConnection, makeHandlers, readStream } from "./client.js";
import { onUnauthorized, callUnauthorizedHandler } from "$lib/api/index.js";

// ---------------------------------------------------------------------------
// Module mocks (hoisted — must be at the top level of the file)
// These stub the store imports so the client can be loaded in happy-dom without
// Svelte $state rune support.
// ---------------------------------------------------------------------------

vi.mock("$lib/stores/reminders.svelte.js", () => ({
  setReminders: vi.fn(),
  upsertReminder: vi.fn(),
  removeReminder: vi.fn(),
}));

vi.mock("$lib/stores/conversation.svelte.js", () => ({
  applyStreamingDelta: vi.fn(),
  ensureStreamingSlot: vi.fn(),
  finalizeStreamingMessage: vi.fn(),
  getActiveConversationId: vi.fn(() => null),
  setConversationHistory: vi.fn(),
  setTurnFailed: vi.fn(),
  STREAMING_SENTINEL_ID: "__streaming__",
}));

vi.mock("$lib/stores/tool-calls.svelte.js", () => ({
  toolCallStarted: vi.fn(),
  toolCallCompleted: vi.fn(),
  toolCallFailed: vi.fn(),
  finalizeToolCallsForMessage: vi.fn(),
}));

vi.mock("$lib/stores/confirmations.svelte.js", () => ({
  confirmationRequired: vi.fn(),
  confirmationExpired: vi.fn(),
  finalizeConfirmationsForMessage: vi.fn(),
}));

vi.mock("$lib/notifications/reminder-fired.svelte.js", () => ({
  notifyReminderFired: vi.fn(),
}));

// ---------------------------------------------------------------------------
// nextBackoff — exponential backoff with jitter
// ---------------------------------------------------------------------------

describe("nextBackoff()", () => {
  it("doubles the initial backoff", () => {
    // With jitter ±20%, the result must land in [500*2*0.8, 500*2*1.2] = [800, 1200].
    const result = nextBackoff(500);
    expect(result).toBeGreaterThanOrEqual(800);
    expect(result).toBeLessThanOrEqual(1200);
  });

  it("caps at BACKOFF_MAX_MS (30 000ms) — jitter is applied AFTER clamping", () => {
    // After fix: clamp THEN jitter → result ≤ 30 000ms, NOT 36 000ms.
    for (let i = 0; i < 50; i++) {
      const result = nextBackoff(30_000);
      expect(result).toBeLessThanOrEqual(30_000);
      expect(result).toBeGreaterThanOrEqual(24_000);
    }
  });

  it("caps at 30 000ms even when input is far above max", () => {
    for (let i = 0; i < 50; i++) {
      const result = nextBackoff(1_000_000);
      expect(result).toBeLessThanOrEqual(30_000);
      expect(result).toBeGreaterThanOrEqual(24_000);
    }
  });

  it("always returns a positive integer", () => {
    for (let i = 0; i < 20; i++) {
      const result = nextBackoff(500);
      expect(Number.isInteger(result)).toBe(true);
      expect(result).toBeGreaterThan(0);
    }
  });

  it("progression from 500ms reaches near-max within ~8 steps", () => {
    // 500 → 1000 → 2000 → 4000 → 8000 → 16000 → 30000 (capped)
    // Without jitter this is exactly 7 doublings; with jitter it may vary slightly.
    // Generous upper bound of 20 steps to tolerate jitter.
    let backoff = BACKOFF_INITIAL_MS;
    let steps = 0;
    while (backoff < 24_000 && steps < 20) {
      backoff = nextBackoff(backoff);
      steps++;
    }
    expect(steps).toBeLessThanOrEqual(20);
    expect(backoff).toBeGreaterThanOrEqual(24_000);
  });
});

// ---------------------------------------------------------------------------
// Helpers for constructing fake ReadableStream chunks.
// ---------------------------------------------------------------------------

function makeStream(chunks: string[]): ReadableStream<Uint8Array> {
  const encoder = new TextEncoder();
  let idx = 0;
  return new ReadableStream<Uint8Array>({
    pull(controller) {
      if (idx < chunks.length) {
        controller.enqueue(encoder.encode(chunks[idx++]));
      } else {
        controller.close();
      }
    },
  });
}

// ---------------------------------------------------------------------------
// SSE client — teardown behaviour (Fix 5a)
// ---------------------------------------------------------------------------

describe("SSE client — teardown", () => {
  let fetchCalls = 0;

  beforeEach(() => {
    fetchCalls = 0;
    vi.useFakeTimers();

    vi.stubGlobal(
      "fetch",
      vi.fn(() => {
        fetchCalls++;
        // Return a promise that never resolves (simulates a pending connection).
        return new Promise<Response>(() => {
          // intentionally never resolves until aborted
        });
      }),
    );
  });

  afterEach(async () => {
    closeSseConnection();
    await vi.runAllTimersAsync();
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it("closeSseConnection() aborts the fetch and no reconnect loop runs after teardown", async () => {
    openSseConnection();
    // Allow the fetch call to be issued.
    await Promise.resolve();
    await Promise.resolve();

    expect(fetchCalls).toBe(1);

    // Close before the response arrives — should abort and exit the loop.
    closeSseConnection();
    await Promise.resolve();
    await Promise.resolve();

    // Advance all timers — no backoff sleep should trigger another fetch.
    await vi.runAllTimersAsync();

    // Only one fetch was ever made.
    expect(fetchCalls).toBe(1);
  });
});

// ---------------------------------------------------------------------------
// SSE client — reconcile() invoked on successful connection (Fix 5b)
// ---------------------------------------------------------------------------

describe("SSE client — reconcile on connect", () => {
  let fetchCallIdx = 0;

  beforeEach(() => {
    fetchCallIdx = 0;
    vi.useFakeTimers();

    vi.stubGlobal(
      "fetch",
      vi.fn(() => {
        fetchCallIdx++;
        if (fetchCallIdx === 1) {
          // First fetch = SSE /events — return an open stream with one ping, then close.
          const stream = makeStream(["event: ping\ndata: \n\n"]);
          return Promise.resolve(
            new Response(stream, {
              status: 200,
              headers: { "Content-Type": "text/event-stream" },
            }),
          );
        }
        // Subsequent fetches = reconcile REST calls (reminders).
        const body = JSON.stringify({ data: [], pagination: { nextCursor: null, hasMore: false } });
        return Promise.resolve(
          new Response(body, {
            status: 200,
            headers: { "Content-Type": "application/json" },
          }),
        );
      }),
    );
  });

  afterEach(async () => {
    closeSseConnection();
    await vi.runAllTimersAsync();
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it("reconcile() is invoked on successful connection — fetches reminders after /events opens", async () => {
    openSseConnection();
    // Drain enough microtask turns for connection + reconcile to fire.
    for (let i = 0; i < 20; i++) {
      await Promise.resolve();
    }

    // fetch must have been called at least twice: /events then /api/v1/reminders.
    expect(fetchCallIdx).toBeGreaterThanOrEqual(2);

    closeSseConnection();
    await vi.runAllTimersAsync();
  });
});

// ---------------------------------------------------------------------------
// reconcile() — multi-page reminders accumulation (Fix 2)
// ---------------------------------------------------------------------------

describe("reconcile() — reminders pagination", () => {
  const fetchedUrls: string[] = [];
  // Access the mocked setReminders to assert on its calls.
  let setRemindersMock: ReturnType<typeof vi.fn>;

  beforeEach(async () => {
    fetchedUrls.length = 0;
    vi.useFakeTimers();

    // Get a reference to the mocked setReminders from the hoisted mock.
    const remindersModule = await import("$lib/stores/reminders.svelte.js");
    setRemindersMock = remindersModule.setReminders as ReturnType<typeof vi.fn>;
    setRemindersMock.mockClear();

    const page1Reminder = {
      id: "r1",
      userId: "u1",
      title: "R1",
      dueAt: "2030-01-01T00:00:00Z",
      tz: "UTC",
      autoComplete: true,
      status: "active",
      createdAt: "2025-01-01T00:00:00Z",
      updatedAt: "2025-01-01T00:00:00Z",
    };
    const page2Reminder = {
      id: "r2",
      userId: "u1",
      title: "R2",
      dueAt: "2030-01-02T00:00:00Z",
      tz: "UTC",
      autoComplete: true,
      status: "active",
      createdAt: "2025-01-01T00:00:00Z",
      updatedAt: "2025-01-01T00:00:00Z",
    };

    vi.stubGlobal(
      "fetch",
      vi.fn((input: RequestInfo | URL) => {
        const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
        fetchedUrls.push(url);

        // SSE endpoint.
        if (url === "/events") {
          const stream = makeStream(["event: ping\ndata: \n\n"]);
          return Promise.resolve(
            new Response(stream, { status: 200, headers: { "Content-Type": "text/event-stream" } }),
          );
        }

        // /api/v1/reminders page 1 (no cursor).
        if (!url.includes("cursor=")) {
          const body = JSON.stringify({
            data: [page1Reminder],
            pagination: { nextCursor: "page2", hasMore: true },
          });
          return Promise.resolve(new Response(body, { status: 200, headers: { "Content-Type": "application/json" } }));
        }

        // /api/v1/reminders?cursor=page2.
        const body = JSON.stringify({
          data: [page2Reminder],
          pagination: { nextCursor: null, hasMore: false },
        });
        return Promise.resolve(new Response(body, { status: 200, headers: { "Content-Type": "application/json" } }));
      }),
    );
  });

  afterEach(async () => {
    closeSseConnection();
    await vi.runAllTimersAsync();
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it("pages through all reminder pages and calls setReminders with the full accumulated list", async () => {
    openSseConnection();

    // Drain enough microtask turns for connection + multi-page reconcile to complete.
    for (let i = 0; i < 30; i++) {
      await Promise.resolve();
    }

    // setReminders must have been called exactly once with both pages' items.
    expect(setRemindersMock).toHaveBeenCalledOnce();
    const [allReminders] = setRemindersMock.mock.calls[0] as [unknown[]];
    expect(allReminders).toHaveLength(2);
    expect((allReminders[0] as { id: string }).id).toBe("r1");
    expect((allReminders[1] as { id: string }).id).toBe("r2");

    closeSseConnection();
    await vi.runAllTimersAsync();
  });
});

// ---------------------------------------------------------------------------
// makeHandlers() — reminder.fired dispatch seam (FIX 2)
//
// Tests the onReminderEvent branch inside makeHandlers():
//   • a well-formed reminder.fired payload forwards to notifyReminderFired
//   • a malformed payload (missing / non-string fields) is NOT forwarded
//   • reminder.created routes to upsertReminder (regression guard)
//   • reminder.deleted routes to removeReminder (regression guard)
// ---------------------------------------------------------------------------

describe("makeHandlers() — onReminderEvent dispatch", () => {
  let notifyReminderFiredMock: ReturnType<typeof vi.fn>;
  let upsertReminderMock: ReturnType<typeof vi.fn>;
  let removeReminderMock: ReturnType<typeof vi.fn>;

  beforeEach(async () => {
    const notificationsModule = await import("$lib/notifications/reminder-fired.svelte.js");
    notifyReminderFiredMock = notificationsModule.notifyReminderFired as ReturnType<typeof vi.fn>;
    notifyReminderFiredMock.mockClear();

    const remindersModule = await import("$lib/stores/reminders.svelte.js");
    upsertReminderMock = remindersModule.upsertReminder as ReturnType<typeof vi.fn>;
    upsertReminderMock.mockClear();
    removeReminderMock = remindersModule.removeReminder as ReturnType<typeof vi.fn>;
    removeReminderMock.mockClear();
  });

  const VALID_FIRED_PAYLOAD = {
    reminderId: "rem-1",
    title: "Call dentist",
    dueAt: "2030-01-15T09:00:00Z",
    firedAt: "2030-01-15T09:00:01Z",
  };

  it("forwards a well-formed reminder.fired payload to notifyReminderFired", () => {
    const handlers = makeHandlers();
    handlers.onReminderEvent("reminder.fired", VALID_FIRED_PAYLOAD);
    expect(notifyReminderFiredMock).toHaveBeenCalledOnce();
    expect(notifyReminderFiredMock).toHaveBeenCalledWith(VALID_FIRED_PAYLOAD);
  });

  it("does NOT call notifyReminderFired when reminderId is missing", () => {
    const handlers = makeHandlers();
    handlers.onReminderEvent("reminder.fired", {
      title: "X",
      dueAt: "2030-01-01T00:00:00Z",
      firedAt: "2030-01-01T00:00:01Z",
    });
    expect(notifyReminderFiredMock).not.toHaveBeenCalled();
  });

  it("does NOT call notifyReminderFired when title is not a string", () => {
    const handlers = makeHandlers();
    handlers.onReminderEvent("reminder.fired", {
      reminderId: "rem-1",
      title: 42,
      dueAt: "2030-01-01T00:00:00Z",
      firedAt: "2030-01-01T00:00:01Z",
    });
    expect(notifyReminderFiredMock).not.toHaveBeenCalled();
  });

  it("does NOT call notifyReminderFired when dueAt is not a string", () => {
    const handlers = makeHandlers();
    handlers.onReminderEvent("reminder.fired", {
      reminderId: "rem-1",
      title: "X",
      dueAt: null,
      firedAt: "2030-01-01T00:00:01Z",
    });
    expect(notifyReminderFiredMock).not.toHaveBeenCalled();
  });

  it("does NOT call notifyReminderFired when firedAt is not a string", () => {
    const handlers = makeHandlers();
    handlers.onReminderEvent("reminder.fired", {
      reminderId: "rem-1",
      title: "X",
      dueAt: "2030-01-01T00:00:00Z",
      firedAt: undefined,
    });
    expect(notifyReminderFiredMock).not.toHaveBeenCalled();
  });

  it("routes reminder.created to upsertReminder (regression guard)", () => {
    const reminder = { id: "rem-1", userId: "u1", title: "T", dueAt: "2030-01-01T00:00:00Z", status: "active" };
    const handlers = makeHandlers();
    handlers.onReminderEvent("reminder.created", reminder);
    expect(upsertReminderMock).toHaveBeenCalledOnce();
    expect(upsertReminderMock).toHaveBeenCalledWith(reminder);
    expect(notifyReminderFiredMock).not.toHaveBeenCalled();
  });

  it("routes reminder.deleted to removeReminder (regression guard)", () => {
    const handlers = makeHandlers();
    handlers.onReminderEvent("reminder.deleted", { id: "rem-1" });
    expect(removeReminderMock).toHaveBeenCalledOnce();
    expect(removeReminderMock).toHaveBeenCalledWith("rem-1");
    expect(notifyReminderFiredMock).not.toHaveBeenCalled();
  });
});

// ---------------------------------------------------------------------------
// makeHandlers() — onMessageCompleted reconciles conversation history
//
// On message.completed the authoritative assistant message is persisted
// server-side. The handler refetches the conversation and replaces history with
// server truth, healing any deltas the client missed (e.g. a mid-turn reload
// that reconnected after the early deltas had already been streamed to the old
// connection — reload-resilience, journey 5).
// ---------------------------------------------------------------------------

describe("makeHandlers() — onMessageCompleted conversation reconcile", () => {
  let getActiveConversationIdMock: ReturnType<typeof vi.fn>;
  let setConversationHistoryMock: ReturnType<typeof vi.fn>;
  let finalizeStreamingMessageMock: ReturnType<typeof vi.fn>;

  beforeEach(async () => {
    const convModule = await import("$lib/stores/conversation.svelte.js");
    getActiveConversationIdMock = convModule.getActiveConversationId as ReturnType<typeof vi.fn>;
    setConversationHistoryMock = convModule.setConversationHistory as ReturnType<typeof vi.fn>;
    finalizeStreamingMessageMock = convModule.finalizeStreamingMessage as ReturnType<typeof vi.fn>;
    getActiveConversationIdMock.mockReturnValue("conv-1");
    setConversationHistoryMock.mockClear();
    finalizeStreamingMessageMock.mockClear();

    vi.stubGlobal(
      "fetch",
      vi.fn((input: RequestInfo | URL) => {
        const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
        if (url.includes("/conversations/conv-1")) {
          const body = JSON.stringify({
            messages: [
              {
                id: "m-full",
                role: "assistant",
                content: "Once upon a time, in a land far away.",
                createdAt: "2025-01-01T00:00:00Z",
                abandoned: false,
              },
            ],
          });
          return Promise.resolve(new Response(body, { status: 200, headers: { "Content-Type": "application/json" } }));
        }
        return Promise.resolve(new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } }));
      }),
    );
  });

  afterEach(() => {
    getActiveConversationIdMock.mockReturnValue(null);
    vi.unstubAllGlobals();
  });

  it("refetches the conversation and replaces history with server truth on completion", async () => {
    const handlers = makeHandlers();
    handlers.onMessageCompleted("conv-1", "m-full", "stop");

    // Drain microtask turns so the async reconcile fetch resolves.
    for (let i = 0; i < 20; i++) {
      await Promise.resolve();
    }

    // The streaming slot is finalized synchronously first…
    expect(finalizeStreamingMessageMock).toHaveBeenCalledWith("m-full", undefined, "stop");
    // …then the authoritative history replaces it.
    expect(setConversationHistoryMock).toHaveBeenCalledOnce();
    const [messages] = setConversationHistoryMock.mock.calls[0] as [{ id: string; content: string }[]];
    expect(messages[0]?.id).toBe("m-full");
    expect(messages[0]?.content).toBe("Once upon a time, in a land far away.");
  });

  it("does not reconcile when the completion is for a different conversation", async () => {
    getActiveConversationIdMock.mockReturnValue("conv-other");
    const handlers = makeHandlers();
    handlers.onMessageCompleted("conv-1", "m-full", "stop");

    for (let i = 0; i < 20; i++) {
      await Promise.resolve();
    }

    expect(finalizeStreamingMessageMock).not.toHaveBeenCalled();
    expect(setConversationHistoryMock).not.toHaveBeenCalled();
  });
});

// ---------------------------------------------------------------------------
// M3 — 401 on /events must invoke the unauthorizedHandler teardown seam
//
// When the bare fetch("/events") returns 401, the SSE loop must call
// callUnauthorizedHandler() (the same authority the REST path uses) so that
// notifyTearDown / resetSession / goto("/login") fire. Merely setting
// _active = false is insufficient — the session store keeps reporting
// authenticated and the user is never redirected.
// ---------------------------------------------------------------------------

describe("SSE client — 401 on /events invokes the unauthorized handler (M3)", () => {
  let handlerSpy: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    vi.useFakeTimers();
    handlerSpy = vi.fn();
    // Register a spy as the central 401 authority. Cast required: vi.fn() returns
    // a generic Mock type that is wider than (() => void) | (() => Promise<void>).
    onUnauthorized(handlerSpy as () => void);
  });

  afterEach(async () => {
    closeSseConnection();
    await vi.runAllTimersAsync();
    vi.useRealTimers();
    vi.unstubAllGlobals();
    // Restore a no-op so the handler doesn't bleed between suites.
    onUnauthorized(() => {
      // no-op
    });
  });

  it("calls callUnauthorizedHandler when /events returns 401", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() =>
        Promise.resolve(
          new Response(null, {
            status: 401,
            headers: { "Content-Type": "application/problem+json" },
          }),
        ),
      ),
    );

    openSseConnection();

    // Drain microtask turns for the connection attempt and 401 handling to complete.
    for (let i = 0; i < 20; i++) {
      await Promise.resolve();
    }

    expect(handlerSpy).toHaveBeenCalledOnce();
  });

  it("callUnauthorizedHandler is the seam — the registered handler is invoked", () => {
    const spy = vi.fn();
    // Cast required: vi.fn() returns Mock which is wider than (() => void) | (() => Promise<void>).
    onUnauthorized(spy as () => void);
    void callUnauthorizedHandler();
    expect(spy).toHaveBeenCalledOnce();
  });
});

// ---------------------------------------------------------------------------
// Fix 2 — reconcile() does NOT wipe reminders on a server error response
//
// When Api.GET("/reminders") returns a problem+json error (non-2xx),
// openapi-fetch returns { data: undefined, error } without throwing.
// The reconcile loop must detect result.error and bail out — leaving the
// reminders store untouched — rather than calling setReminders([]).
// ---------------------------------------------------------------------------

describe("reconcile() — reminders store is left untouched on server error (Fix 2)", () => {
  let setRemindersMock: ReturnType<typeof vi.fn>;

  beforeEach(async () => {
    vi.useFakeTimers();

    const remindersModule = await import("$lib/stores/reminders.svelte.js");
    setRemindersMock = remindersModule.setReminders as ReturnType<typeof vi.fn>;
    setRemindersMock.mockClear();
  });

  afterEach(async () => {
    closeSseConnection();
    await vi.runAllTimersAsync();
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it("does not call setReminders when the /reminders endpoint returns a non-2xx error", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn((input: RequestInfo | URL) => {
        const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;

        // SSE /events — return a successful open stream so reconcile is triggered.
        if (url === "/events") {
          const stream = new ReadableStream<Uint8Array>({
            start(controller) {
              controller.enqueue(new TextEncoder().encode("event: ping\ndata: \n\n"));
              controller.close();
            },
          });
          return Promise.resolve(
            new Response(stream, { status: 200, headers: { "Content-Type": "text/event-stream" } }),
          );
        }

        // /api/v1/reminders — return a 503 problem+json error.
        const body = JSON.stringify({
          type: "https://qovira.com/problems/service-unavailable",
          title: "Service Unavailable",
          status: 503,
          detail: "Upstream database is unreachable",
          code: "service_unavailable",
          requestId: "req-test-1",
        });
        return Promise.resolve(
          new Response(body, { status: 503, headers: { "Content-Type": "application/problem+json" } }),
        );
      }),
    );

    openSseConnection();

    // Drain enough microtask turns for connection + reconcile attempt.
    for (let i = 0; i < 40; i++) {
      await Promise.resolve();
    }

    // setReminders must NOT have been called — the store is left as-is.
    expect(setRemindersMock).not.toHaveBeenCalled();
  });
});

// ---------------------------------------------------------------------------
// Fix 3 — readStream dispatches events from CRLF-terminated frames
//
// The SSE spec allows \r\n line endings. A CRLF-framed stream (\r\n\r\n frame
// boundaries) must dispatch events identically to a LF-framed stream.
// Without normalization, lastIndexOf("\n\n") never matches \r\n\r\n and the
// client buffers forever, dispatching nothing.
// ---------------------------------------------------------------------------

describe("readStream() — CRLF-framed events are dispatched (Fix 3)", () => {
  it("dispatches an event from a CRLF-terminated frame", async () => {
    const dispatchedEvents: { event: string; data: string }[] = [];

    // Build a mock RouterHandlers whose onReminderEvent captures dispatches.
    // We use reminder.created as a canary — any routable event works.
    const handlers = makeHandlers();

    // Intercept the stores to capture what gets routed.
    const remindersModule = await import("$lib/stores/reminders.svelte.js");
    const upsertReminderMock = remindersModule.upsertReminder as ReturnType<typeof vi.fn>;
    upsertReminderMock.mockClear();
    upsertReminderMock.mockImplementation((r: unknown) => {
      dispatchedEvents.push({ event: "reminder.created", data: JSON.stringify(r) });
    });

    // Build a CRLF-framed SSE event: lines end with \r\n, frame ends with \r\n\r\n.
    const reminderPayload = JSON.stringify({
      id: "rem-crlf",
      userId: "u1",
      title: "CRLF test",
      dueAt: "2030-01-01T00:00:00Z",
      tz: "UTC",
      autoComplete: false,
      status: "active",
      createdAt: "2025-01-01T00:00:00Z",
      updatedAt: "2025-01-01T00:00:00Z",
    });
    const crlfFrame = `event: reminder.created\r\ndata: ${reminderPayload}\r\n\r\n`;

    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(new TextEncoder().encode(crlfFrame));
        controller.close();
      },
    });

    const controller = new AbortController();
    await readStream(stream, controller.signal, handlers);

    expect(dispatchedEvents).toHaveLength(1);
    expect(dispatchedEvents[0]?.event).toBe("reminder.created");

    upsertReminderMock.mockRestore();
  });
});
