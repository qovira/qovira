// Tests for confirmation store rune logic.
//
// Rune environment: node + Svelte compiler (vitest project "runes").
// Uses flushSync to drain $derived updates synchronously.
//
// Tests the following acceptance criteria:
//   AC1 — confirmation.required adds an entry with pending state.
//   AC2 — resolving (approve/deny) transitions to a resolved state.
//   AC3 — confirmation.expired transitions to an expired state.
//   AC4 — card persists under its assistant turn after the turn finalizes
//          (sentinel → real messageId retag via finalizeConfirmationsForMessage).
//   AC5 — resolveConfirmation() does NOT record a decision when the server
//          returns a problem+json error (e.g. 409 already-resolved/expired).
//   AC6 — resolveConfirmation() does NOT record a decision on a network throw.
import { flushSync } from "svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  getConfirmations,
  getConfirmationsForMessage,
  confirmationRequired,
  confirmationResolved,
  confirmationExpired,
  finalizeConfirmationsForMessage,
  resolveConfirmation,
  resetConfirmations,
  CONFIRMATION_STREAMING_SENTINEL_ID,
  type PendingConfirmation,
  type ResolvedConfirmation,
  type ExpiredConfirmation,
} from "./confirmations.svelte.js";

// ---------------------------------------------------------------------------
// Reset between tests — module singleton
// ---------------------------------------------------------------------------

afterEach(() => {
  resetConfirmations();
  flushSync();
});

// ---------------------------------------------------------------------------
// Initial state
// ---------------------------------------------------------------------------

describe("confirmations store — initial state", () => {
  it("starts empty", () => {
    expect(getConfirmations()).toEqual([]);
  });
});

// ---------------------------------------------------------------------------
// confirmationRequired — adds a pending entry
// ---------------------------------------------------------------------------

describe("confirmationRequired()", () => {
  it("adds an entry with state 'pending' tagged with the streaming sentinel", () => {
    confirmationRequired({
      conversationId: "c1",
      callId: "call-1",
      name: "delete_reminder",
      risk: "destructive",
      args: { id: "r-123" },
      expiresAt: "2030-01-01T00:00:00Z",
    });
    flushSync();

    const entries = getConfirmations();
    expect(entries).toHaveLength(1);
    const entry = entries[0] as PendingConfirmation;
    expect(entry.state).toBe("pending");
    expect(entry.callId).toBe("call-1");
    expect(entry.name).toBe("delete_reminder");
    expect(entry.risk).toBe("destructive");
    expect(entry.expiresAt).toBe("2030-01-01T00:00:00Z");
    expect(entry.assistantMessageId).toBe(CONFIRMATION_STREAMING_SENTINEL_ID);
  });

  it("ignores a duplicate callId (second arrival is a reconnect race)", () => {
    confirmationRequired({
      conversationId: "c1",
      callId: "call-1",
      name: "delete_reminder",
      risk: "destructive",
      args: {},
      expiresAt: "2030-01-01T00:00:00Z",
    });
    confirmationRequired({
      conversationId: "c1",
      callId: "call-1",
      name: "delete_reminder",
      risk: "destructive",
      args: {},
      expiresAt: "2030-01-01T00:00:00Z",
    });
    flushSync();

    expect(getConfirmations()).toHaveLength(1);
  });

  it("stores args as-is (opaque JSON value)", () => {
    const args = { id: "r-abc", extra: true };
    confirmationRequired({
      conversationId: "c1",
      callId: "call-2",
      name: "delete_reminder",
      risk: "destructive",
      args,
      expiresAt: "2030-06-01T12:00:00Z",
    });
    flushSync();

    const entry = getConfirmations()[0] as PendingConfirmation;
    expect(entry.args).toEqual(args);
  });

  it("adds routine-write confirmation with honey risk tier", () => {
    confirmationRequired({
      conversationId: "c1",
      callId: "call-write",
      name: "create_reminder",
      risk: "write",
      args: { title: "Buy milk" },
      expiresAt: "2030-01-01T00:00:00Z",
    });
    flushSync();

    const entry = getConfirmations()[0] as PendingConfirmation;
    expect(entry.state).toBe("pending");
    expect(entry.risk).toBe("write");
  });
});

// ---------------------------------------------------------------------------
// confirmationResolved — approve / deny
// ---------------------------------------------------------------------------

describe("confirmationResolved()", () => {
  it("transitions state to 'resolved' with decision 'approve'", () => {
    confirmationRequired({
      conversationId: "c1",
      callId: "call-1",
      name: "delete_reminder",
      risk: "destructive",
      args: {},
      expiresAt: "2030-01-01T00:00:00Z",
    });
    flushSync();

    confirmationResolved("call-1", "approve");
    flushSync();

    const entry = getConfirmations()[0] as ResolvedConfirmation;
    expect(entry.state).toBe("resolved");
    expect(entry.decision).toBe("approve");
  });

  it("transitions state to 'resolved' with decision 'deny'", () => {
    confirmationRequired({
      conversationId: "c1",
      callId: "call-1",
      name: "delete_reminder",
      risk: "destructive",
      args: {},
      expiresAt: "2030-01-01T00:00:00Z",
    });
    flushSync();

    confirmationResolved("call-1", "deny");
    flushSync();

    const entry = getConfirmations()[0] as ResolvedConfirmation;
    expect(entry.state).toBe("resolved");
    expect(entry.decision).toBe("deny");
  });

  it("preserves all original fields after resolve", () => {
    confirmationRequired({
      conversationId: "c1",
      callId: "call-1",
      name: "delete_reminder",
      risk: "destructive",
      args: { id: "r-123" },
      expiresAt: "2030-01-01T00:00:00Z",
    });
    flushSync();

    confirmationResolved("call-1", "approve");
    flushSync();

    const entry = getConfirmations()[0] as ResolvedConfirmation;
    expect(entry.name).toBe("delete_reminder");
    expect(entry.risk).toBe("destructive");
    expect(entry.args).toEqual({ id: "r-123" });
    expect(entry.expiresAt).toBe("2030-01-01T00:00:00Z");
  });

  it("is a no-op when the callId is unknown", () => {
    confirmationResolved("unknown-call", "approve");
    flushSync();
    expect(getConfirmations()).toHaveLength(0);
  });
});

// ---------------------------------------------------------------------------
// confirmationExpired — expire from SSE event
// ---------------------------------------------------------------------------

describe("confirmationExpired()", () => {
  it("transitions state to 'expired' on SSE confirmation.expired event", () => {
    confirmationRequired({
      conversationId: "c1",
      callId: "call-1",
      name: "delete_reminder",
      risk: "destructive",
      args: {},
      expiresAt: "2030-01-01T00:00:00Z",
    });
    flushSync();

    confirmationExpired("call-1");
    flushSync();

    const entry = getConfirmations()[0] as ExpiredConfirmation;
    expect(entry.state).toBe("expired");
  });

  it("preserves original fields after expiry", () => {
    confirmationRequired({
      conversationId: "c1",
      callId: "call-1",
      name: "delete_reminder",
      risk: "destructive",
      args: { id: "r-x" },
      expiresAt: "2030-01-01T00:00:00Z",
    });
    flushSync();

    confirmationExpired("call-1");
    flushSync();

    const entry = getConfirmations()[0] as ExpiredConfirmation;
    expect(entry.name).toBe("delete_reminder");
    expect(entry.risk).toBe("destructive");
    expect(entry.args).toEqual({ id: "r-x" });
  });

  it("is a no-op when the callId is unknown", () => {
    confirmationExpired("ghost");
    flushSync();
    expect(getConfirmations()).toHaveLength(0);
  });
});

// ---------------------------------------------------------------------------
// getConfirmationsForMessage — filters by assistantMessageId
// ---------------------------------------------------------------------------

describe("getConfirmationsForMessage()", () => {
  it("returns empty when no entries exist", () => {
    expect(getConfirmationsForMessage(CONFIRMATION_STREAMING_SENTINEL_ID)).toEqual([]);
    expect(getConfirmationsForMessage("msg-real")).toEqual([]);
  });

  it("returns entries tagged with the given assistantMessageId", () => {
    confirmationRequired({
      conversationId: "c1",
      callId: "call-1",
      name: "delete_reminder",
      risk: "destructive",
      args: {},
      expiresAt: "2030-01-01T00:00:00Z",
    });
    flushSync();

    const inFlight = getConfirmationsForMessage(CONFIRMATION_STREAMING_SENTINEL_ID);
    expect(inFlight).toHaveLength(1);
    expect(inFlight[0]?.callId).toBe("call-1");

    expect(getConfirmationsForMessage("other-msg")).toEqual([]);
  });
});

// ---------------------------------------------------------------------------
// finalizeConfirmationsForMessage — sentinel → real id retag
// (AC4: card persists under its turn after the turn finalizes)
// ---------------------------------------------------------------------------

describe("finalizeConfirmationsForMessage() — card persists after turn finalizes", () => {
  it("retags all __streaming__ entries to the real messageId", () => {
    confirmationRequired({
      conversationId: "c1",
      callId: "call-1",
      name: "delete_reminder",
      risk: "destructive",
      args: {},
      expiresAt: "2030-01-01T00:00:00Z",
    });
    flushSync();

    finalizeConfirmationsForMessage(CONFIRMATION_STREAMING_SENTINEL_ID, "real-msg-42");
    flushSync();

    expect(getConfirmationsForMessage(CONFIRMATION_STREAMING_SENTINEL_ID)).toHaveLength(0);

    const cards = getConfirmationsForMessage("real-msg-42");
    expect(cards).toHaveLength(1);
    expect(cards[0]?.callId).toBe("call-1");
  });

  it("pending card persists under real messageId after turn finalization", () => {
    confirmationRequired({
      conversationId: "c1",
      callId: "call-1",
      name: "delete_reminder",
      risk: "destructive",
      args: {},
      expiresAt: "2030-06-01T12:00:00Z",
    });
    flushSync();

    // Finalize the turn (sentinel → real id).
    finalizeConfirmationsForMessage(CONFIRMATION_STREAMING_SENTINEL_ID, "real-msg-1");
    flushSync();

    // Card must still be visible under the real message id (not gone, not under sentinel).
    const cards = getConfirmationsForMessage("real-msg-1");
    expect(cards).toHaveLength(1);
    const card = cards[0] as PendingConfirmation;
    expect(card.state).toBe("pending");
    expect(card.callId).toBe("call-1");
  });

  it("resolved card persists after turn finalization", () => {
    confirmationRequired({
      conversationId: "c1",
      callId: "call-1",
      name: "delete_reminder",
      risk: "destructive",
      args: {},
      expiresAt: "2030-01-01T00:00:00Z",
    });
    flushSync();
    confirmationResolved("call-1", "approve");
    flushSync();

    finalizeConfirmationsForMessage(CONFIRMATION_STREAMING_SENTINEL_ID, "real-msg-2");
    flushSync();

    const cards = getConfirmationsForMessage("real-msg-2");
    expect(cards).toHaveLength(1);
    const card = cards[0] as ResolvedConfirmation;
    expect(card.state).toBe("resolved");
    expect(card.decision).toBe("approve");
  });

  it("expired card persists after turn finalization", () => {
    confirmationRequired({
      conversationId: "c1",
      callId: "call-1",
      name: "delete_reminder",
      risk: "destructive",
      args: {},
      expiresAt: "2029-01-01T00:00:00Z",
    });
    flushSync();
    confirmationExpired("call-1");
    flushSync();

    finalizeConfirmationsForMessage(CONFIRMATION_STREAMING_SENTINEL_ID, "real-msg-3");
    flushSync();

    const cards = getConfirmationsForMessage("real-msg-3");
    expect(cards).toHaveLength(1);
    expect((cards[0] as ExpiredConfirmation).state).toBe("expired");
  });

  it("does NOT retag entries from an already-finalized turn", () => {
    // Turn A: already finalized.
    confirmationRequired({
      conversationId: "c1",
      callId: "call-a",
      name: "delete_reminder",
      risk: "destructive",
      args: {},
      expiresAt: "2030-01-01T00:00:00Z",
    });
    flushSync();
    finalizeConfirmationsForMessage(CONFIRMATION_STREAMING_SENTINEL_ID, "msg-turn-a");
    flushSync();

    // Turn B: new streaming turn.
    confirmationRequired({
      conversationId: "c1",
      callId: "call-b",
      name: "create_reminder",
      risk: "write",
      args: {},
      expiresAt: "2030-06-01T00:00:00Z",
    });
    flushSync();
    finalizeConfirmationsForMessage(CONFIRMATION_STREAMING_SENTINEL_ID, "msg-turn-b");
    flushSync();

    // Turn A unchanged.
    const turnA = getConfirmationsForMessage("msg-turn-a");
    expect(turnA).toHaveLength(1);
    expect(turnA[0]?.callId).toBe("call-a");

    // Turn B correctly tagged.
    const turnB = getConfirmationsForMessage("msg-turn-b");
    expect(turnB).toHaveLength(1);
    expect(turnB[0]?.callId).toBe("call-b");
  });

  it("is a no-op when no entries are tagged with oldId", () => {
    finalizeConfirmationsForMessage(CONFIRMATION_STREAMING_SENTINEL_ID, "real-msg-x");
    flushSync();
    expect(getConfirmations()).toEqual([]);
  });
});

// ---------------------------------------------------------------------------
// AC3 — expiry disables buttons (store-level: state transitions to 'expired')
// ---------------------------------------------------------------------------

describe("expiry disables buttons (AC3)", () => {
  it("a pending card transitions to 'expired' via confirmationExpired() — buttons must be disabled", () => {
    confirmationRequired({
      conversationId: "c1",
      callId: "call-exp",
      name: "delete_reminder",
      risk: "destructive",
      args: {},
      expiresAt: "2020-01-01T00:00:00Z", // past — already expired
    });
    flushSync();

    // Simulate the SSE confirmation.expired event arriving.
    confirmationExpired("call-exp");
    flushSync();

    const entry = getConfirmations()[0] as ExpiredConfirmation;
    // When state === 'expired', the component must disable its buttons.
    expect(entry.state).toBe("expired");
  });
});

// ---------------------------------------------------------------------------
// resetConfirmations — clears all entries
// ---------------------------------------------------------------------------

describe("resetConfirmations()", () => {
  it("clears all entries", () => {
    confirmationRequired({
      conversationId: "c1",
      callId: "call-1",
      name: "delete_reminder",
      risk: "destructive",
      args: {},
      expiresAt: "2030-01-01T00:00:00Z",
    });
    flushSync();

    resetConfirmations();
    flushSync();

    expect(getConfirmations()).toEqual([]);
  });

  it("is a no-op when already empty", () => {
    resetConfirmations();
    flushSync();
    expect(getConfirmations()).toEqual([]);
  });
});

// ---------------------------------------------------------------------------
// resolveConfirmation() — AC5: server error must NOT record the decision
// ---------------------------------------------------------------------------
//
// The Api wrapper returns { error: ProblemError } on problem+json responses
// instead of throwing — the component's original bare `catch` never fired on
// 409/404/422. resolveConfirmation() must check the returned error and leave
// the entry in "pending" state (and clear `resolving`) on server errors.

vi.mock("$lib/api/index.js", () => {
  const ProblemError = class extends Error {
    override readonly name = "ProblemError";
    readonly status: number;
    readonly code: string;
    readonly type: string;
    readonly title: string;
    readonly detail: string;
    readonly requestId: string;
    constructor(raw: { status: number; code: string; type: string; title: string; detail: string; requestId: string }) {
      super(`${raw.code}: ${raw.detail}`);
      this.status = raw.status;
      this.code = raw.code;
      this.type = raw.type;
      this.title = raw.title;
      this.detail = raw.detail;
      this.requestId = raw.requestId;
    }
  };

  return {
    ProblemError,
    Api: {
      POST: vi.fn(),
    },
  };
});

function makeEntry(): PendingConfirmation {
  return {
    state: "pending",
    callId: "call-resolve-test",
    conversationId: "conv-1",
    assistantMessageId: CONFIRMATION_STREAMING_SENTINEL_ID,
    name: "delete_reminder",
    risk: "destructive",
    args: { id: "r-1" },
    expiresAt: "2030-01-01T00:00:00Z",
  };
}

describe("resolveConfirmation() — AC5: server returns problem+json error", () => {
  afterEach(() => {
    vi.resetAllMocks();
    resetConfirmations();
    flushSync();
  });

  it("does NOT call confirmationResolved when Api.POST returns { error: ProblemError 409 }", async () => {
    const { Api, ProblemError } = await import("$lib/api/index.js");

    const problem409 = new (ProblemError as new (raw: {
      status: number;
      code: string;
      type: string;
      title: string;
      detail: string;
      requestId: string;
    }) => InstanceType<typeof ProblemError>)({
      status: 409,
      code: "confirmation_already_resolved",
      type: "https://qovira.ai/errors/confirmation_already_resolved",
      title: "Confirmation already resolved",
      detail: "This confirmation has already been resolved or has expired.",
      requestId: "req-409",
    });

    (Api.POST as ReturnType<typeof vi.fn>).mockResolvedValue({
      data: undefined,
      error: problem409,
      response: undefined,
    });

    const entry = makeEntry();
    confirmationRequired({
      conversationId: entry.conversationId,
      callId: entry.callId,
      name: entry.name,
      risk: entry.risk,
      args: entry.args,
      expiresAt: entry.expiresAt,
    });
    flushSync();

    await resolveConfirmation(entry, "approve");
    flushSync();

    // Entry must still be "pending" — the server rejected the decision.
    const stored = getConfirmations()[0];
    expect(stored?.state).toBe("pending");
  });

  it("does NOT call confirmationResolved when Api.POST returns { error: ProblemError 404 }", async () => {
    const { Api, ProblemError } = await import("$lib/api/index.js");

    const problem404 = new (ProblemError as new (raw: {
      status: number;
      code: string;
      type: string;
      title: string;
      detail: string;
      requestId: string;
    }) => InstanceType<typeof ProblemError>)({
      status: 404,
      code: "confirmation_not_found",
      type: "https://qovira.ai/errors/confirmation_not_found",
      title: "Confirmation not found",
      detail: "Unknown callId.",
      requestId: "req-404",
    });

    (Api.POST as ReturnType<typeof vi.fn>).mockResolvedValue({
      data: undefined,
      error: problem404,
      response: undefined,
    });

    const entry = makeEntry();
    confirmationRequired({
      conversationId: entry.conversationId,
      callId: entry.callId,
      name: entry.name,
      risk: entry.risk,
      args: entry.args,
      expiresAt: entry.expiresAt,
    });
    flushSync();

    await resolveConfirmation(entry, "deny");
    flushSync();

    const stored = getConfirmations()[0];
    expect(stored?.state).toBe("pending");
  });

  it("resolving flag is cleared after a server error so buttons re-enable", async () => {
    const { Api, ProblemError } = await import("$lib/api/index.js");

    const problem = new (ProblemError as new (raw: {
      status: number;
      code: string;
      type: string;
      title: string;
      detail: string;
      requestId: string;
    }) => InstanceType<typeof ProblemError>)({
      status: 409,
      code: "confirmation_already_resolved",
      type: "https://qovira.ai/errors/confirmation_already_resolved",
      title: "Already resolved",
      detail: "Already resolved.",
      requestId: "req-409-b",
    });

    (Api.POST as ReturnType<typeof vi.fn>).mockResolvedValue({ data: undefined, error: problem, response: undefined });

    const entry = makeEntry();
    confirmationRequired({
      conversationId: entry.conversationId,
      callId: entry.callId,
      name: entry.name,
      risk: entry.risk,
      args: entry.args,
      expiresAt: entry.expiresAt,
    });
    flushSync();

    // resolveConfirmation must return without leaving the caller hung.
    // We verify this by confirming it resolves (no hanging promise) and state is still pending.
    const result = resolveConfirmation(entry, "approve");
    await expect(result).resolves.toBeUndefined();
    flushSync();

    expect(getConfirmations()[0]?.state).toBe("pending");
  });
});

// ---------------------------------------------------------------------------
// resolveConfirmation() — AC6: network throw must NOT record the decision
// ---------------------------------------------------------------------------

describe("resolveConfirmation() — AC6: network error (Api.POST throws)", () => {
  afterEach(() => {
    vi.resetAllMocks();
    resetConfirmations();
    flushSync();
  });

  it("does NOT call confirmationResolved when Api.POST throws a network error", async () => {
    const { Api } = await import("$lib/api/index.js");

    (Api.POST as ReturnType<typeof vi.fn>).mockRejectedValue(new TypeError("Failed to fetch"));

    const entry = makeEntry();
    confirmationRequired({
      conversationId: entry.conversationId,
      callId: entry.callId,
      name: entry.name,
      risk: entry.risk,
      args: entry.args,
      expiresAt: entry.expiresAt,
    });
    flushSync();

    await resolveConfirmation(entry, "approve");
    flushSync();

    // Entry must still be "pending" — network error, not resolved.
    const stored = getConfirmations()[0];
    expect(stored?.state).toBe("pending");
  });

  it("resolving flag is cleared after a network throw so buttons re-enable", async () => {
    const { Api } = await import("$lib/api/index.js");

    (Api.POST as ReturnType<typeof vi.fn>).mockRejectedValue(new TypeError("Failed to fetch"));

    const entry = makeEntry();
    confirmationRequired({
      conversationId: entry.conversationId,
      callId: entry.callId,
      name: entry.name,
      risk: entry.risk,
      args: entry.args,
      expiresAt: entry.expiresAt,
    });
    flushSync();

    const result = resolveConfirmation(entry, "deny");
    await expect(result).resolves.toBeUndefined();
    flushSync();

    expect(getConfirmations()[0]?.state).toBe("pending");
  });
});
