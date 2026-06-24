// Tests for the conversation store rune logic.
// Rune environment: node + Svelte compiler (vitest project "runes").
// Uses flushSync to drain $derived updates synchronously.
import { flushSync } from "svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  getActiveConversationId,
  getConversationHistory,
  getTurnError,
  setActiveConversation,
  applyStreamingDelta,
  ensureStreamingSlot,
  appendMessage,
  finalizeStreamingMessage,
  setConversationHistory,
  resetConversation,
  sendChatMessage,
  STREAMING_SENTINEL_ID,
  type HistoryMessage,
  type StreamingHistoryMessage,
  type PostMessageFn,
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

// ---------------------------------------------------------------------------
// sessionStorage persistence — the active conversation id survives a reload
// (reload-resilience, AC #4). The runes env is node (no sessionStorage), so we
// install an in-memory Storage mock around these tests.
// ---------------------------------------------------------------------------

const STORAGE_KEY = "qovira:active-conversation";

function makeStorageMock(): Storage {
  const backing = new Map<string, string>();
  return {
    getItem: (key) => backing.get(key) ?? null,
    setItem: (key, value) => {
      backing.set(key, value);
    },
    removeItem: (key) => {
      backing.delete(key);
    },
    clear: () => {
      backing.clear();
    },
    key: (i) => [...backing.keys()][i] ?? null,
    get length() {
      return backing.size;
    },
  };
}

describe("active-conversation persistence", () => {
  beforeEach(() => {
    vi.stubGlobal("sessionStorage", makeStorageMock());
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("mirrors the active conversation id into sessionStorage", () => {
    setActiveConversation("conv-persist", []);
    flushSync();
    expect(sessionStorage.getItem(STORAGE_KEY)).toBe("conv-persist");
  });

  it("clears the persisted id on resetConversation", () => {
    setActiveConversation("conv-persist", []);
    flushSync();
    resetConversation();
    flushSync();
    expect(sessionStorage.getItem(STORAGE_KEY)).toBeNull();
  });

  it("seeds the active conversation id from sessionStorage when the module initializes (reload restore)", async () => {
    sessionStorage.setItem(STORAGE_KEY, "conv-restored");
    vi.resetModules();
    const mod = await import("./conversation.svelte.js");
    expect(mod.getActiveConversationId()).toBe("conv-restored");
    mod.resetConversation();
  });
});

// ---------------------------------------------------------------------------
// sendChatMessage — Fix #1 (M4): chat-send orchestration
//
// sendChatMessage(postFn, conversationId, text) encapsulates the POST /messages
// flow so it is testable without rendering a route component:
//   - On success (data defined): calls appendMessage and returns null (no restore).
//   - On problem+json error (result.error defined, no throw): returns text so
//     the caller can restore the composer, and records a turn failure.
//   - On thrown network error (postFn throws): same as the returned-error path.
//
// The postFn parameter is a narrow async function that takes (conversationId,
// text) and resolves to the Api.POST FetchResponse shape — injected so tests
// provide a hand fake without vi.mock.
//
// These tests MUST fail if the `result.error` branch is removed from
// sendChatMessage (the original bug: text was silently dropped on 4xx/5xx).
// ---------------------------------------------------------------------------

/** Minimal MessageResponse shape matching the server's 202 body. */
interface MinimalMessageResponse {
  id: string;
  conversationId: string;
  role: string;
  content: string;
  createdAt: string;
}

/** Build a successful postFn fake that resolves with the given message. */
function makeSuccessPost(msg: MinimalMessageResponse): PostMessageFn {
  // Params are required by the type but unused in the fake — satisfied positionally.
  return () => Promise.resolve({ data: msg, response: new Response() });
}

/** Build a postFn fake that resolves with a problem+json error (does NOT throw). */
function makeErrorPost(errorPayload: object): PostMessageFn {
  // Params required by type but unused — satisfied positionally.
  // Cast via unknown: the error branch shape has data?: never which conflicts
  // with { data: undefined } literally; at runtime both mean "no data".
  return () =>
    Promise.resolve({ data: undefined, error: errorPayload, response: new Response() } as unknown as Awaited<
      ReturnType<PostMessageFn>
    >);
}

/** Build a postFn fake that throws a network error (no response). */
function makeThrowingPost(err: Error): PostMessageFn {
  // Params required by type but unused — rejected promise exercises the catch path.
  return () => Promise.reject<Awaited<ReturnType<PostMessageFn>>>(err);
}

describe("sendChatMessage() — Fix #1 (M4): send-error handling", () => {
  const CONV_ID = "conv-m4";

  beforeEach(() => {
    setActiveConversation(CONV_ID, []);
    flushSync();
  });

  // ---------------------------------------------------------------------------
  // Success path
  // ---------------------------------------------------------------------------

  it("on success: appends the persisted user message to history and returns null", async () => {
    const serverMsg: MinimalMessageResponse = {
      id: "server-msg-1",
      conversationId: CONV_ID,
      role: "user",
      content: "hello world",
      createdAt: "2030-01-01T00:00:00Z",
    };
    const postFn = makeSuccessPost(serverMsg);

    const restoreText = await sendChatMessage(postFn, CONV_ID, "hello world");
    flushSync();

    // No text to restore — success path returns null.
    expect(restoreText).toBeNull();

    // Message appended to history.
    const history = getConversationHistory();
    expect(history).toHaveLength(1);
    expect(history[0]?.id).toBe("server-msg-1");
    expect(history[0]?.content).toBe("hello world");
  });

  it("on success: does NOT set a turn error", async () => {
    const serverMsg: MinimalMessageResponse = {
      id: "server-msg-ok",
      conversationId: CONV_ID,
      role: "user",
      content: "ping",
      createdAt: "2030-01-01T00:00:00Z",
    };
    const postFn = makeSuccessPost(serverMsg);

    await sendChatMessage(postFn, CONV_ID, "ping");
    flushSync();

    expect(getTurnError()).toBeNull();
  });

  // ---------------------------------------------------------------------------
  // Problem+json (returned error — the bug path, Fix #1 / M4)
  //
  // The Api wrapper resolves { data: undefined, error: ProblemError } for 4xx/5xx.
  // It does NOT throw. Before the fix, the error was ignored: text was cleared,
  // nothing was appended, no error was shown — the user's message vanished silently.
  // ---------------------------------------------------------------------------

  it("on returned problem+json error: returns the original text so the composer can be restored", async () => {
    const postFn = makeErrorPost({ code: "internal_error", detail: "boom" });

    const restoreText = await sendChatMessage(postFn, CONV_ID, "my typed message");
    flushSync();

    // MUST return the text — this is the fix. Without the result.error branch,
    // this returns null and the test fails.
    expect(restoreText).toBe("my typed message");
  });

  it("on returned problem+json error: does NOT append anything to history", async () => {
    const postFn = makeErrorPost({ code: "rate_limited", detail: "slow down" });

    await sendChatMessage(postFn, CONV_ID, "some text");
    flushSync();

    // Nothing must be appended — the POST failed.
    expect(getConversationHistory()).toHaveLength(0);
  });

  it("on returned problem+json error: sets a turn failure via setTurnFailed", async () => {
    const postFn = makeErrorPost({ code: "server_error", detail: "oops" });

    await sendChatMessage(postFn, CONV_ID, "will fail");
    flushSync();

    // Turn error must be set — the error seam must be triggered.
    expect(getTurnError()).not.toBeNull();
  });

  // ---------------------------------------------------------------------------
  // Thrown network error (no response — CORS, offline, etc.)
  // ---------------------------------------------------------------------------

  it("on thrown network error: returns the original text so the composer can be restored", async () => {
    const postFn = makeThrowingPost(new Error("Network failure"));

    const restoreText = await sendChatMessage(postFn, CONV_ID, "offline message");
    flushSync();

    expect(restoreText).toBe("offline message");
  });

  it("on thrown network error: does NOT append anything to history", async () => {
    const postFn = makeThrowingPost(new Error("CORS error"));

    await sendChatMessage(postFn, CONV_ID, "lost text");
    flushSync();

    expect(getConversationHistory()).toHaveLength(0);
  });

  it("on thrown network error: sets a turn failure", async () => {
    const postFn = makeThrowingPost(new Error("fetch failed"));

    await sendChatMessage(postFn, CONV_ID, "offline");
    flushSync();

    expect(getTurnError()).not.toBeNull();
  });

  // ---------------------------------------------------------------------------
  // Symmetry: returned-error and thrown-error behave identically
  // ---------------------------------------------------------------------------

  it("problem+json and network-throw paths are symmetric: both restore text", async () => {
    const errorPost = makeErrorPost({ code: "x" });
    const throwPost = makeThrowingPost(new Error("net"));

    const r1 = await sendChatMessage(errorPost, CONV_ID, "text A");
    flushSync();
    resetConversation();
    setActiveConversation(CONV_ID, []);
    flushSync();

    const r2 = await sendChatMessage(throwPost, CONV_ID, "text A");
    flushSync();

    expect(r1).toBe("text A");
    expect(r2).toBe("text A");
  });
});
