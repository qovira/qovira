// Tests for tool-call state rune logic.
// Rune environment: node + Svelte compiler (vitest project "runes"). Uses flushSync to drain $derived updates
// synchronously.
import { flushSync } from "svelte";
import { afterEach, describe, expect, it } from "vitest";

import {
  getToolCalls,
  toolCallStarted,
  toolCallCompleted,
  toolCallFailed,
  resetToolCalls,
  type CompletedToolCall,
  type FailedToolCall,
} from "./tool-calls.svelte.js";

// ---------------------------------------------------------------------------
// Reset between tests — module singleton
// ---------------------------------------------------------------------------

afterEach(() => {
  resetToolCalls();
  flushSync();
});

// ---------------------------------------------------------------------------
// Initial state
// ---------------------------------------------------------------------------

describe("tool-calls store — initial state", () => {
  it("starts empty", () => {
    expect(getToolCalls()).toEqual([]);
  });
});

// ---------------------------------------------------------------------------
// toolCallStarted — adds an in-progress entry
// ---------------------------------------------------------------------------

describe("toolCallStarted()", () => {
  it("adds an entry with state 'started'", () => {
    toolCallStarted({
      conversationId: "c1",
      callId: "call-1",
      name: "create_reminder",
      risk: "write",
      argsSummary: '{"title":"Call dentist"}',
    });
    flushSync();

    const calls = getToolCalls();
    expect(calls).toHaveLength(1);
    const entry = calls[0];
    expect(entry?.callId).toBe("call-1");
    expect(entry?.name).toBe("create_reminder");
    expect(entry?.argsSummary).toBe('{"title":"Call dentist"}');
    expect(entry?.state).toBe("started");
  });

  it("maintains insertion order across multiple tool calls", () => {
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

    const calls = getToolCalls();
    expect(calls).toHaveLength(2);
    const c0 = calls[0];
    const c1 = calls[1];
    expect(c0?.callId).toBe("call-1");
    expect(c1?.callId).toBe("call-2");
  });

  it("does not duplicate an existing callId — ignores second start", () => {
    toolCallStarted({
      conversationId: "c1",
      callId: "call-1",
      name: "create_reminder",
      risk: "write",
      argsSummary: "{}",
    });
    toolCallStarted({
      conversationId: "c1",
      callId: "call-1",
      name: "create_reminder",
      risk: "write",
      argsSummary: "{}",
    });
    flushSync();

    expect(getToolCalls()).toHaveLength(1);
  });
});

// ---------------------------------------------------------------------------
// toolCallCompleted — resolves the chip with a result
// ---------------------------------------------------------------------------

describe("toolCallCompleted()", () => {
  it("transitions state to 'completed' and stores result", () => {
    toolCallStarted({
      conversationId: "c1",
      callId: "call-1",
      name: "create_reminder",
      risk: "write",
      argsSummary: '{"title":"Dentist"}',
    });
    flushSync();

    toolCallCompleted("c1", "call-1", {
      id: "r-123",
      title: "Call dentist",
      dueAt: "2030-01-10T09:00:00Z",
      status: "active",
    });
    flushSync();

    const entry = getToolCalls()[0] as CompletedToolCall;
    expect(entry.state).toBe("completed");
    expect(entry.result).toMatchObject({ id: "r-123", title: "Call dentist" });
  });

  it("correlates by callId — retrieves the tool name from the started entry", () => {
    toolCallStarted({
      conversationId: "c1",
      callId: "call-1",
      name: "create_reminder",
      risk: "write",
      argsSummary: "{}",
    });
    flushSync();

    toolCallCompleted("c1", "call-1", { id: "r-abc" });
    flushSync();

    const entry = getToolCalls()[0] as CompletedToolCall;
    expect(entry.name).toBe("create_reminder");
    expect(entry.state).toBe("completed");
  });

  it("is a no-op when the callId is unknown (completed without started)", () => {
    toolCallCompleted("c1", "call-unknown", { id: "r-x" });
    flushSync();

    expect(getToolCalls()).toHaveLength(0);
  });

  it("handles unrecognized result shapes gracefully — state is still 'completed'", () => {
    toolCallStarted({ conversationId: "c1", callId: "call-1", name: "some_tool", risk: "write", argsSummary: "{}" });
    flushSync();

    // Result is an unexpected shape — no crash expected.
    toolCallCompleted("c1", "call-1", { unexpected: true });
    flushSync();

    const entry = getToolCalls()[0] as CompletedToolCall;
    expect(entry.state).toBe("completed");
    expect(entry.result).toEqual({ unexpected: true });
  });

  it("handles null result gracefully", () => {
    toolCallStarted({
      conversationId: "c1",
      callId: "call-1",
      name: "delete_reminder",
      risk: "destructive",
      argsSummary: "{}",
    });
    flushSync();

    toolCallCompleted("c1", "call-1", null);
    flushSync();

    const entry = getToolCalls()[0] as CompletedToolCall;
    expect(entry.state).toBe("completed");
    expect(entry.result).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// toolCallFailed — transitions to error state
// ---------------------------------------------------------------------------

describe("toolCallFailed()", () => {
  it("transitions state to 'failed' and stores the error text", () => {
    toolCallStarted({
      conversationId: "c1",
      callId: "call-1",
      name: "create_reminder",
      risk: "write",
      argsSummary: "{}",
    });
    flushSync();

    toolCallFailed("c1", "call-1", "The title field is required.");
    flushSync();

    const entry = getToolCalls()[0] as FailedToolCall;
    expect(entry.state).toBe("failed");
    expect(entry.error).toBe("The title field is required.");
  });

  it("is a no-op when the callId is unknown", () => {
    toolCallFailed("c1", "call-unknown", "some error");
    flushSync();

    expect(getToolCalls()).toHaveLength(0);
  });

  it("preserves the tool name from the started entry after failure", () => {
    toolCallStarted({
      conversationId: "c1",
      callId: "call-1",
      name: "update_reminder",
      risk: "write",
      argsSummary: "{}",
    });
    flushSync();

    toolCallFailed("c1", "call-1", "Not found.");
    flushSync();

    const entry = getToolCalls()[0] as FailedToolCall;
    expect(entry.name).toBe("update_reminder");
  });
});

// ---------------------------------------------------------------------------
// resetToolCalls — clears all entries
// ---------------------------------------------------------------------------

describe("resetToolCalls()", () => {
  it("clears all tool-call entries", () => {
    toolCallStarted({
      conversationId: "c1",
      callId: "call-1",
      name: "create_reminder",
      risk: "write",
      argsSummary: "{}",
    });
    toolCallCompleted("c1", "call-1", { id: "r-1" });
    flushSync();

    resetToolCalls();
    flushSync();

    expect(getToolCalls()).toEqual([]);
  });

  it("is a no-op when already empty", () => {
    resetToolCalls();
    flushSync();
    expect(getToolCalls()).toEqual([]);
  });
});

// ---------------------------------------------------------------------------
// list_reminders (RiskRead) — quiet resolution
// ---------------------------------------------------------------------------

describe("RiskRead routine reads resolve quietly", () => {
  it("marks list_reminders as completed without crashing", () => {
    toolCallStarted({
      conversationId: "c1",
      callId: "call-lr",
      name: "list_reminders",
      risk: "read",
      argsSummary: "{}",
    });
    flushSync();

    toolCallCompleted("c1", "call-lr", [{ id: "r-1", title: "Foo" }]);
    flushSync();

    const entry = getToolCalls()[0] as CompletedToolCall;
    expect(entry.state).toBe("completed");
    expect(entry.name).toBe("list_reminders");
  });
});
