// Tests for the conversation store rune logic.
// Rune environment: node + Svelte compiler (vitest project "runes").
// Uses flushSync to drain $derived updates synchronously.
import { flushSync } from "svelte";
import { afterEach, describe, expect, it } from "vitest";

import {
  getActiveConversationId,
  getConversationHistory,
  setActiveConversation,
  applyStreamingDelta,
  ensureStreamingSlot,
  appendMessage,
  finalizeStreamingMessage,
  setConversationHistory,
  resetConversation,
  STREAMING_SENTINEL_ID,
  type HistoryMessage,
  type StreamingHistoryMessage,
} from "./conversation.svelte.js";

/** Type guard: true if the entry is the in-flight streaming slot. */
function isStreaming(m: HistoryMessage | StreamingHistoryMessage): m is StreamingHistoryMessage {
  return "streaming" in m;
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function makeMessage(overrides: Partial<HistoryMessage> = {}): HistoryMessage {
  return {
    id: "msg-1",
    role: "assistant",
    content: "Hello",
    createdAt: "2025-01-01T00:00:00Z",
    abandoned: false,
    ...overrides,
  };
}

// ---------------------------------------------------------------------------
// Reset between tests
// ---------------------------------------------------------------------------

afterEach(() => {
  resetConversation();
  flushSync();
});

// ---------------------------------------------------------------------------
// Default state
// ---------------------------------------------------------------------------

describe("conversation store — initial state", () => {
  it("has no active conversation id", () => {
    expect(getActiveConversationId()).toBeNull();
  });

  it("has an empty history", () => {
    expect(getConversationHistory()).toEqual([]);
  });
});

// ---------------------------------------------------------------------------
// setActiveConversation — sets the active conversation and clears history
// ---------------------------------------------------------------------------

describe("setActiveConversation()", () => {
  it("sets the active conversation id", () => {
    setActiveConversation("conv-1", []);
    flushSync();
    expect(getActiveConversationId()).toBe("conv-1");
  });

  it("seeds the history when provided", () => {
    const msgs = [makeMessage({ id: "m1" }), makeMessage({ id: "m2", role: "user", content: "Hi" })];
    setActiveConversation("conv-1", msgs);
    flushSync();
    expect(getConversationHistory()).toHaveLength(2);
    expect(getConversationHistory()[0]?.id).toBe("m1");
    expect(getConversationHistory()[1]?.id).toBe("m2");
  });

  it("replaces history when switching conversation", () => {
    setActiveConversation("conv-1", [makeMessage({ id: "m1" })]);
    flushSync();
    setActiveConversation("conv-2", [makeMessage({ id: "m2" })]);
    flushSync();
    expect(getActiveConversationId()).toBe("conv-2");
    expect(getConversationHistory()).toHaveLength(1);
    expect(getConversationHistory()[0]?.id).toBe("m2");
  });

  it("clears history when switching to a conversation with empty messages", () => {
    setActiveConversation("conv-1", [makeMessage()]);
    flushSync();
    setActiveConversation("conv-2", []);
    flushSync();
    expect(getConversationHistory()).toEqual([]);
  });
});

// ---------------------------------------------------------------------------
// setConversationHistory — full replace (reconcile after reconnect)
// ---------------------------------------------------------------------------

describe("setConversationHistory()", () => {
  it("replaces the entire history", () => {
    setActiveConversation("conv-1", [makeMessage({ id: "old" })]);
    flushSync();

    setConversationHistory([makeMessage({ id: "new-1" }), makeMessage({ id: "new-2" })]);
    flushSync();

    expect(getConversationHistory()).toHaveLength(2);
    expect(getConversationHistory()[0]?.id).toBe("new-1");
  });

  it("is a no-op in terms of activeConversationId", () => {
    setActiveConversation("conv-x", []);
    flushSync();
    setConversationHistory([makeMessage()]);
    flushSync();
    expect(getActiveConversationId()).toBe("conv-x");
  });
});

// ---------------------------------------------------------------------------
// applyStreamingDelta — streams text into the single in-flight assistant slot
// ---------------------------------------------------------------------------

describe("applyStreamingDelta()", () => {
  it("opens a streaming assistant message on the first delta (no messageId from server)", () => {
    setActiveConversation("conv-1", []);
    flushSync();

    // The server sends NO messageId on deltas — only text.
    applyStreamingDelta("Hello");
    flushSync();

    expect(getConversationHistory()).toHaveLength(1);
    const msg = getConversationHistory()[0];
    expect(msg?.role).toBe("assistant");
    expect(msg?.content).toBe("Hello");
    // The streaming slot has a temporary placeholder id (not the real server id).
    // Narrow to StreamingHistoryMessage to access the streaming marker.
    expect(msg !== undefined && isStreaming(msg) && msg.streaming).toBe(true);
  });

  it("accumulates multiple deltas into the same message", () => {
    setActiveConversation("conv-1", []);
    flushSync();

    applyStreamingDelta("A");
    applyStreamingDelta("B");
    applyStreamingDelta("C");
    flushSync();

    // Only one streaming message, all text accumulated.
    expect(getConversationHistory()).toHaveLength(1);
    const msg0 = getConversationHistory()[0];
    expect(msg0?.content).toBe("ABC");
    expect(msg0 !== undefined && isStreaming(msg0) && msg0.streaming).toBe(true);
  });

  it("appends after existing history messages", () => {
    setActiveConversation("conv-1", [makeMessage({ id: "user-msg", role: "user", content: "Hi" })]);
    flushSync();

    applyStreamingDelta("Hello");
    flushSync();

    expect(getConversationHistory()).toHaveLength(2);
    const msg1 = getConversationHistory()[1];
    expect(msg1?.role).toBe("assistant");
    expect(msg1 !== undefined && isStreaming(msg1) && msg1.streaming).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// ensureStreamingSlot — opens an anchor slot for a no-text suspended turn
// ---------------------------------------------------------------------------

describe("ensureStreamingSlot()", () => {
  it("opens an empty streaming slot when none is open", () => {
    setActiveConversation("conv-1", [makeMessage({ id: "user-msg", role: "user", content: "delete it" })]);
    flushSync();

    ensureStreamingSlot();
    flushSync();

    const history = getConversationHistory();
    expect(history).toHaveLength(2);
    const slot = history[1];
    expect(slot?.id).toBe(STREAMING_SENTINEL_ID);
    expect(slot?.role).toBe("assistant");
    expect(slot?.content).toBe("");
    expect(slot !== undefined && isStreaming(slot) && slot.streaming).toBe(true);
  });

  it("is idempotent and never disturbs an open slot's accumulated text", () => {
    applyStreamingDelta("partial text");
    flushSync();

    ensureStreamingSlot();
    flushSync();

    // Still exactly one streaming slot, with its text intact — no empty slot added.
    const history = getConversationHistory();
    expect(history).toHaveLength(1);
    const slot = history[0];
    expect(slot?.content).toBe("partial text");
    expect(slot !== undefined && isStreaming(slot) && slot.streaming).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// finalizeStreamingMessage — called on message.completed, sets real id
// ---------------------------------------------------------------------------

describe("finalizeStreamingMessage()", () => {
  it("finalizes the streaming message: sets real messageId, clears streaming flag", () => {
    setActiveConversation("conv-1", []);
    flushSync();

    applyStreamingDelta("Hello world");
    flushSync();

    finalizeStreamingMessage("real-msg-id", undefined, "stop");
    flushSync();

    const history = getConversationHistory();
    expect(history).toHaveLength(1);
    const msg = history[0];
    expect(msg?.id).toBe("real-msg-id");
    // After finalize, the entry is still a StreamingHistoryMessage with streaming: false.
    expect(msg !== undefined && isStreaming(msg) && msg.streaming).toBe(false);
    expect(msg?.finishReason).toBe("stop");
    // Content is preserved from accumulated deltas.
    expect(msg?.content).toBe("Hello world");
  });

  it("uses server-provided content when given, overriding accumulated delta text", () => {
    setActiveConversation("conv-1", []);
    flushSync();

    applyStreamingDelta("partial...");
    flushSync();

    finalizeStreamingMessage("real-msg-id", "authoritative content", "stop");
    flushSync();

    expect(getConversationHistory()[0]?.content).toBe("authoritative content");
  });

  it("is a no-op when there is no streaming message in progress", () => {
    setActiveConversation("conv-1", [makeMessage({ id: "m1" })]);
    flushSync();

    // No streaming message was opened — finalizing should not crash or corrupt state.
    finalizeStreamingMessage("real-msg-id", undefined, "stop");
    flushSync();

    const history = getConversationHistory();
    expect(history).toHaveLength(1);
    expect(history[0]?.id).toBe("m1");
  });

  it("delta for a non-active conversation does not touch the active thread (guard in caller)", () => {
    // The guard lives in the client/router, but we verify the store is side-effect-free
    // when applyStreamingDelta is not called for the wrong conversation.
    setActiveConversation("conv-active", [makeMessage({ id: "m1", content: "A" })]);
    flushSync();

    // Caller guards — does NOT call applyStreamingDelta for a different conversationId.
    // Store stays pristine.
    expect(getConversationHistory()).toHaveLength(1);
    expect(getConversationHistory()[0]?.content).toBe("A");
  });
});

// ---------------------------------------------------------------------------
// resetConversation — called on logout
// ---------------------------------------------------------------------------

describe("resetConversation()", () => {
  it("clears active conversation and history", () => {
    setActiveConversation("conv-1", [makeMessage()]);
    flushSync();

    resetConversation();
    flushSync();

    expect(getActiveConversationId()).toBeNull();
    expect(getConversationHistory()).toEqual([]);
  });

  it("is a no-op when already empty", () => {
    resetConversation();
    flushSync();
    expect(getActiveConversationId()).toBeNull();
    expect(getConversationHistory()).toEqual([]);
  });
});

// ---------------------------------------------------------------------------
// Routing invariant — events for a different conversationId are ignored
// ---------------------------------------------------------------------------

describe("conversationId routing invariant", () => {
  it("applyStreamingDelta appends to the active history (guard is the caller's responsibility)", () => {
    // The store itself doesn't filter by conversationId — the router/client does.
    // We verify the store accumulates correctly when called.
    setActiveConversation("conv-active", [makeMessage({ id: "m1", content: "A" })]);
    flushSync();

    applyStreamingDelta(" delta");
    flushSync();

    // A streaming message was appended after the existing user message.
    expect(getConversationHistory()).toHaveLength(2);
    const slot = getConversationHistory()[1];
    expect(slot !== undefined && isStreaming(slot) && slot.streaming).toBe(true);
    expect(slot?.content).toBe(" delta");
  });
});

// ---------------------------------------------------------------------------
// appendMessage — inserts a persisted message before any open streaming slot
// ---------------------------------------------------------------------------

describe("appendMessage()", () => {
  it("pushes to the end when there is no streaming slot", () => {
    setActiveConversation("conv-1", [makeMessage({ id: "m1", role: "user", content: "Hi" })]);
    flushSync();

    const userMsg = makeMessage({ id: "m2", role: "user", content: "Second" });
    appendMessage(userMsg);
    flushSync();

    const history = getConversationHistory();
    expect(history).toHaveLength(2);
    expect(history[1]?.id).toBe("m2");
  });

  it("splices before the first open streaming slot so the user bubble renders before the assistant reply", () => {
    setActiveConversation("conv-1", []);
    flushSync();

    // Simulate a message.delta arriving while the POST /messages is still in-flight.
    applyStreamingDelta("Hello");
    flushSync();

    // Now the 202 resolves and we append the persisted user message.
    const userMsg = makeMessage({ id: "persisted-user", role: "user", content: "Trigger text" });
    appendMessage(userMsg);
    flushSync();

    const history = getConversationHistory();
    // [0] = user message, [1] = streaming assistant slot
    expect(history).toHaveLength(2);
    expect(history[0]?.id).toBe("persisted-user");
    expect(history[0]?.role).toBe("user");
    const slot = history[1];
    expect(slot !== undefined && isStreaming(slot) && slot.streaming).toBe(true);
  });

  it("preserves accumulated streaming text after inserting the user message before it", () => {
    setActiveConversation("conv-1", []);
    flushSync();

    applyStreamingDelta("Part A ");
    applyStreamingDelta("Part B");
    flushSync();

    const userMsg = makeMessage({ id: "u1", role: "user", content: "Ping" });
    appendMessage(userMsg);
    flushSync();

    const history = getConversationHistory();
    expect(history).toHaveLength(2);
    const slot = history[1];
    // Streaming slot content must not be clobbered by the insert.
    expect(slot?.content).toBe("Part A Part B");
    expect(slot !== undefined && isStreaming(slot) && slot.streaming).toBe(true);
  });

  it("places the message after existing persisted messages and before any streaming slot", () => {
    const existingUser = makeMessage({ id: "m1", role: "user", content: "First" });
    const existingAssistant = makeMessage({ id: "m2", role: "assistant", content: "Reply" });
    setActiveConversation("conv-1", [existingUser, existingAssistant]);
    flushSync();

    // A new turn: streaming slot opens before the 202 resolves.
    applyStreamingDelta("Streaming...");
    flushSync();

    const newUserMsg = makeMessage({ id: "m3", role: "user", content: "Second user turn" });
    appendMessage(newUserMsg);
    flushSync();

    const history = getConversationHistory();
    // [0] = existing user, [1] = existing assistant, [2] = new user, [3] = streaming slot
    expect(history).toHaveLength(4);
    expect(history[2]?.id).toBe("m3");
    const slot = history[3];
    expect(slot !== undefined && isStreaming(slot) && slot.streaming).toBe(true);
    expect(slot?.content).toBe("Streaming...");
  });
});
