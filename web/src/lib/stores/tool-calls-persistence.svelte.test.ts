// Tests for tool-call persistence across turn finalization (Fix 1 — AC2).
//
// Verifies that tool-call entries remain associated with an assistant turn AFTER the streaming sentinel id
// (__streaming__) is swapped for the real messageId by finalizeToolCallsForMessage(). This is the key invariant for
// "cards must persist inline after the turn finalizes".
//
// Rune environment: node + Svelte compiler (vitest project "runes").
import { flushSync } from "svelte";
import { afterEach, describe, expect, it } from "vitest";

import {
  getToolCalls,
  getToolCallsForMessage,
  toolCallStarted,
  toolCallCompleted,
  toolCallFailed,
  finalizeToolCallsForMessage,
  resetToolCalls,
  type CompletedToolCall,
  type FailedToolCall,
} from "./tool-calls.svelte.js";
import { STREAMING_SENTINEL_ID } from "./conversation.svelte.js";

const SENTINEL = STREAMING_SENTINEL_ID;

afterEach(() => {
  resetToolCalls();
  flushSync();
});

// ---------------------------------------------------------------------------
// getToolCallsForMessage — filters by assistantMessageId
// ---------------------------------------------------------------------------

describe("getToolCallsForMessage()", () => {
  it("returns an empty array when no tool calls exist", () => {
    expect(getToolCallsForMessage(SENTINEL)).toEqual([]);
    expect(getToolCallsForMessage("msg-real")).toEqual([]);
  });

  it("returns only entries tagged with the given assistantMessageId", () => {
    toolCallStarted({
      conversationId: "c1",
      callId: "call-a",
      name: "create_reminder",
      risk: "write",
      argsSummary: "{}",
    });
    flushSync();

    // Entry was opened during a streaming turn — tagged __streaming__.
    const inFlight = getToolCallsForMessage(SENTINEL);
    expect(inFlight).toHaveLength(1);
    expect(inFlight[0]?.callId).toBe("call-a");

    // No entry under any other id.
    expect(getToolCallsForMessage("some-other-msg")).toEqual([]);
  });

  it("returns entries for multiple tool calls in the same turn", () => {
    toolCallStarted({
      conversationId: "c1",
      callId: "call-1",
      name: "create_reminder",
      risk: "write",
      argsSummary: "{}",
    });
    toolCallStarted({
      conversationId: "c1",
      callId: "call-2",
      name: "list_reminders",
      risk: "read",
      argsSummary: "{}",
    });
    flushSync();

    const calls = getToolCallsForMessage(SENTINEL);
    expect(calls).toHaveLength(2);
    const ids = calls.map((c) => c.callId);
    expect(ids).toContain("call-1");
    expect(ids).toContain("call-2");
  });
});

// ---------------------------------------------------------------------------
// finalizeToolCallsForMessage — sentinel → real id swap
// ---------------------------------------------------------------------------

describe("finalizeToolCallsForMessage()", () => {
  it("retags all __streaming__ entries to the real messageId", () => {
    toolCallStarted({
      conversationId: "c1",
      callId: "call-1",
      name: "create_reminder",
      risk: "write",
      argsSummary: "{}",
    });
    flushSync();

    finalizeToolCallsForMessage(SENTINEL, "real-msg-42");
    flushSync();

    // No longer under sentinel.
    expect(getToolCallsForMessage(SENTINEL)).toHaveLength(0);

    // Now accessible under the real id.
    const cards = getToolCallsForMessage("real-msg-42");
    expect(cards).toHaveLength(1);
    expect(cards[0]?.callId).toBe("call-1");
  });

  it("retags a completed entry — card persists after finalization", () => {
    toolCallStarted({
      conversationId: "c1",
      callId: "call-1",
      name: "create_reminder",
      risk: "write",
      argsSummary: "{}",
    });
    flushSync();
    toolCallCompleted("c1", "call-1", { id: "r-1", title: "Call dentist", dueAt: "2030-01-10T09:00:00Z" });
    flushSync();

    // Verify state before finalization.
    const beforeCards = getToolCallsForMessage(SENTINEL);
    expect(beforeCards).toHaveLength(1);
    expect((beforeCards[0] as CompletedToolCall).state).toBe("completed");

    // Finalize (sentinel → real id).
    finalizeToolCallsForMessage(SENTINEL, "real-msg-42");
    flushSync();

    // Card is still there, now under the real message id.
    expect(getToolCallsForMessage(SENTINEL)).toHaveLength(0);
    const afterCards = getToolCallsForMessage("real-msg-42");
    expect(afterCards).toHaveLength(1);
    const card = afterCards[0] as CompletedToolCall;
    expect(card.state).toBe("completed");
    expect(card.callId).toBe("call-1");
  });

  it("retags a failed entry — chip persists after finalization", () => {
    toolCallStarted({
      conversationId: "c1",
      callId: "call-1",
      name: "create_reminder",
      risk: "write",
      argsSummary: "{}",
    });
    flushSync();
    toolCallFailed("c1", "call-1", "Something went wrong");
    flushSync();

    finalizeToolCallsForMessage(SENTINEL, "real-msg-99");
    flushSync();

    const cards = getToolCallsForMessage("real-msg-99");
    expect(cards).toHaveLength(1);
    expect((cards[0] as FailedToolCall).state).toBe("failed");
    expect((cards[0] as FailedToolCall).error).toBe("Something went wrong");
  });

  it("retags multiple entries in the same turn", () => {
    toolCallStarted({
      conversationId: "c1",
      callId: "call-1",
      name: "create_reminder",
      risk: "write",
      argsSummary: "{}",
    });
    toolCallStarted({
      conversationId: "c1",
      callId: "call-2",
      name: "list_reminders",
      risk: "read",
      argsSummary: "{}",
    });
    flushSync();
    toolCallCompleted("c1", "call-1", { id: "r-1", title: "Foo" });
    toolCallCompleted("c1", "call-2", []);
    flushSync();

    finalizeToolCallsForMessage(SENTINEL, "real-msg-7");
    flushSync();

    const cards = getToolCallsForMessage("real-msg-7");
    expect(cards).toHaveLength(2);
  });

  it("does NOT retag entries from a different (already-finalized) turn", () => {
    // Turn A: already finalized.
    toolCallStarted({
      conversationId: "c1",
      callId: "call-a",
      name: "create_reminder",
      risk: "write",
      argsSummary: "{}",
    });
    flushSync();
    finalizeToolCallsForMessage(SENTINEL, "msg-turn-a");
    flushSync();

    // Turn B: new streaming turn.
    toolCallStarted({
      conversationId: "c1",
      callId: "call-b",
      name: "update_reminder",
      risk: "write",
      argsSummary: "{}",
    });
    flushSync();
    finalizeToolCallsForMessage(SENTINEL, "msg-turn-b");
    flushSync();

    // Each turn's entries remain under their own message id.
    const turnA = getToolCallsForMessage("msg-turn-a");
    expect(turnA).toHaveLength(1);
    expect(turnA[0]?.callId).toBe("call-a");

    const turnB = getToolCallsForMessage("msg-turn-b");
    expect(turnB).toHaveLength(1);
    expect(turnB[0]?.callId).toBe("call-b");
  });

  it("is a no-op when there are no entries tagged with oldId", () => {
    finalizeToolCallsForMessage(SENTINEL, "real-msg-x");
    flushSync();
    expect(getToolCalls()).toEqual([]);
  });
});

// ---------------------------------------------------------------------------
// Across-turn isolation: entries from two different turns render independently
// ---------------------------------------------------------------------------

describe("multi-turn isolation", () => {
  it("entries from turn 1 and turn 2 are independently accessible by their message ids", () => {
    // Turn 1: create_reminder tool call, then finalize.
    toolCallStarted({
      conversationId: "c1",
      callId: "t1-call",
      name: "create_reminder",
      risk: "write",
      argsSummary: "{}",
    });
    flushSync();
    toolCallCompleted("c1", "t1-call", { id: "r-1", title: "Turn 1 reminder" });
    flushSync();
    finalizeToolCallsForMessage(SENTINEL, "turn1-msg");
    flushSync();

    // Turn 2: delete_reminder tool call, then finalize.
    toolCallStarted({
      conversationId: "c1",
      callId: "t2-call",
      name: "delete_reminder",
      risk: "destructive",
      argsSummary: "{}",
    });
    flushSync();
    toolCallCompleted("c1", "t2-call", null);
    flushSync();
    finalizeToolCallsForMessage(SENTINEL, "turn2-msg");
    flushSync();

    // Turn 1 entries still visible under turn1-msg.
    const t1 = getToolCallsForMessage("turn1-msg");
    expect(t1).toHaveLength(1);
    expect(t1[0]?.callId).toBe("t1-call");

    // Turn 2 entries visible under turn2-msg.
    const t2 = getToolCallsForMessage("turn2-msg");
    expect(t2).toHaveLength(1);
    expect(t2[0]?.callId).toBe("t2-call");
  });
});
