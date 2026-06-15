// Tests for the SSE client — backoff utility and client-level teardown / reconnect behaviour.
// Environment: browser (happy-dom) — included via src/lib/**/*.test.ts pattern.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { nextBackoff, BACKOFF_INITIAL_MS } from "./backoff.js";
import { openSseConnection, closeSseConnection } from "./client.js";

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
  finalizeStreamingMessage: vi.fn(),
  getActiveConversationId: vi.fn(() => null),
  setConversationHistory: vi.fn(),
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
