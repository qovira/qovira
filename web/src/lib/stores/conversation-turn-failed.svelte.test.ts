// Tests for turn.failed handling in the conversation store.
//
// Vitest project: runes — node + Svelte compiler.
// Verifies that setTurnFailed() and clearTurnError() work correctly, and that getTurnError() returns the correct
// state.

import { flushSync } from "svelte";
import { afterEach, describe, expect, it } from "vitest";

import {
  getTurnError,
  setActiveConversation,
  setTurnFailed,
  clearTurnError,
  resetConversation,
} from "./conversation.svelte.js";

afterEach(() => {
  resetConversation();
  flushSync();
});

describe("setTurnFailed()", () => {
  it("sets a turn error when the conversationId matches the active conversation", () => {
    setActiveConversation("conv-1", []);
    flushSync();

    setTurnFailed("conv-1", "turn_error");
    flushSync();

    expect(getTurnError()).toBe("turn_error");
  });

  it("ignores turn.failed for a non-active conversation", () => {
    setActiveConversation("conv-1", []);
    flushSync();

    setTurnFailed("conv-other", "turn_error");
    flushSync();

    expect(getTurnError()).toBeNull();
  });

  it("is a no-op when no active conversation", () => {
    setTurnFailed("conv-1", "turn_error");
    flushSync();

    expect(getTurnError()).toBeNull();
  });
});

describe("clearTurnError()", () => {
  it("clears the turn error", () => {
    setActiveConversation("conv-1", []);
    flushSync();

    setTurnFailed("conv-1", "turn_error");
    flushSync();

    expect(getTurnError()).toBe("turn_error");

    clearTurnError();
    flushSync();

    expect(getTurnError()).toBeNull();
  });
});

describe("getTurnError()", () => {
  it("returns null by default", () => {
    expect(getTurnError()).toBeNull();
  });

  it("is cleared by resetConversation()", () => {
    setActiveConversation("conv-1", []);
    flushSync();

    setTurnFailed("conv-1", "turn_error");
    flushSync();

    resetConversation();
    flushSync();

    expect(getTurnError()).toBeNull();
  });

  it("is cleared when setActiveConversation is called again", () => {
    setActiveConversation("conv-1", []);
    flushSync();

    setTurnFailed("conv-1", "turn_error");
    flushSync();

    expect(getTurnError()).toBe("turn_error");

    // Switching to a new conversation clears the error.
    setActiveConversation("conv-2", []);
    flushSync();

    expect(getTurnError()).toBeNull();
  });
});
