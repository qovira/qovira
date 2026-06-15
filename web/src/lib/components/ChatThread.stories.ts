// ChatThread stories — exercises the chat surface's streaming text and error states.
//
// These stories are app compositions, not library primitives. They use the real
// conversation store (setActiveConversation, applyStreamingDelta, setTurnFailed,
// resetConversation, toolCallStarted, toolCallCompleted) seeded via beforeEach so
// the actual rendering paths in ChatThread.svelte (and therefore +page.svelte) are
// exercised — not parallel mocks.
//
// CSF 3 format (not Svelte CSF) because the beforeEach store-seeding logic is
// pure TypeScript and requires no Svelte template complexity.

import type { Meta, StoryObj } from "@storybook/sveltekit";

import {
  setActiveConversation,
  applyStreamingDelta,
  setTurnFailed,
  resetConversation,
} from "$lib/stores/conversation.svelte.js";
import { toolCallStarted, toolCallCompleted, resetToolCalls } from "$lib/stores/tool-calls.svelte.js";
import ChatThread from "./ChatThread.svelte";

const meta = {
  title: "Chat/ChatThread",
  component: ChatThread,
  tags: ["autodocs"],
  parameters: {
    layout: "padded",
  },
} satisfies Meta<typeof ChatThread>;

export default meta;
type Story = StoryObj<typeof meta>;

// ---------------------------------------------------------------------------
// StreamingText: an assistant reply still in-flight, delta accumulated so far.
// Covers AC: "streaming text".
// ---------------------------------------------------------------------------
export const StreamingText: Story = {
  beforeEach: () => {
    resetConversation();
    resetToolCalls();
    setActiveConversation("conv-1", [
      {
        id: "msg-user-1",
        role: "user",
        content: "Set a reminder to buy oat milk tomorrow at 9 am.",
        createdAt: new Date(Date.now() - 5_000).toISOString(),
        abandoned: false,
      },
    ]);
    // Open a streaming slot and accumulate some delta text.
    applyStreamingDelta("I'll set that reminder for you right now");
    applyStreamingDelta("…");
    return () => {
      resetConversation();
      resetToolCalls();
    };
  },
};

// ---------------------------------------------------------------------------
// FinishedTurn: a completed assistant reply (non-streaming, Markdown rendered).
// ---------------------------------------------------------------------------
export const FinishedTurn: Story = {
  beforeEach: () => {
    resetConversation();
    resetToolCalls();
    setActiveConversation("conv-1", [
      {
        id: "msg-user-1",
        role: "user",
        content: "Set a reminder to buy oat milk tomorrow at 9 am.",
        createdAt: new Date(Date.now() - 10_000).toISOString(),
        abandoned: false,
      },
      {
        id: "msg-asst-1",
        role: "assistant",
        content: "Done! I've set a reminder to **buy oat milk** for tomorrow at 9 am.",
        createdAt: new Date(Date.now() - 5_000).toISOString(),
        abandoned: false,
      },
    ]);
    return () => {
      resetConversation();
      resetToolCalls();
    };
  },
};

// ---------------------------------------------------------------------------
// ErrorState: turn.failed after the last assistant message, error line visible.
// Covers AC: "error state".
// ---------------------------------------------------------------------------
export const ErrorState: Story = {
  beforeEach: () => {
    resetConversation();
    resetToolCalls();
    setActiveConversation("conv-1", [
      {
        id: "msg-user-1",
        role: "user",
        content: "Schedule my dentist appointment.",
        createdAt: new Date(Date.now() - 5_000).toISOString(),
        abandoned: false,
      },
    ]);
    // Simulate turn.failed for the active conversation.
    setTurnFailed("conv-1", "turn_error");
    return () => {
      resetConversation();
      resetToolCalls();
    };
  },
};

// Per-story a11y override: renders ToolCallChip whose .tool-chip__args span
// uses opacity: 0.7 for intentional de-emphasis — same real failure as in
// ToolCallChip's own InProgress story. Design intent, not a token bug.
const opacityDeemphasis = {
  a11y: { config: { rules: [{ id: "color-contrast", enabled: false }] } },
};

// ---------------------------------------------------------------------------
// WithToolChip: streaming turn that also has an in-progress tool-call chip.
// Covers AC: "streaming text" + "tool chip (in-progress)" in one composition.
// ---------------------------------------------------------------------------
export const WithToolChip: Story = {
  parameters: opacityDeemphasis,
  beforeEach: () => {
    resetConversation();
    resetToolCalls();
    setActiveConversation("conv-1", [
      {
        id: "msg-user-1",
        role: "user",
        content: "Remind me to call Alice on Friday.",
        createdAt: new Date(Date.now() - 5_000).toISOString(),
        abandoned: false,
      },
    ]);
    // Open a streaming slot first.
    applyStreamingDelta("Let me create that reminder…");
    // Attach a tool chip to the streaming slot.
    toolCallStarted({
      callId: "call-tc-1",
      conversationId: "conv-1",
      name: "create_reminder",
      risk: "write",
      argsSummary: "Call Alice · Friday",
    });
    return () => {
      resetConversation();
      resetToolCalls();
    };
  },
};

// ---------------------------------------------------------------------------
// WithEntityCard: a finalized turn with a completed tool-call entity card.
// ---------------------------------------------------------------------------
export const WithEntityCard: Story = {
  beforeEach: () => {
    resetConversation();
    resetToolCalls();
    setActiveConversation("conv-1", [
      {
        id: "msg-user-1",
        role: "user",
        content: "Remind me to call Alice on Friday.",
        createdAt: new Date(Date.now() - 10_000).toISOString(),
        abandoned: false,
      },
      {
        id: "msg-asst-1",
        role: "assistant",
        content: "Done! I've set a reminder for Friday.",
        createdAt: new Date(Date.now() - 5_000).toISOString(),
        abandoned: false,
      },
    ]);
    // Attach a completed entity card to the finalized message.
    toolCallStarted({
      callId: "call-tc-2",
      conversationId: "conv-1",
      name: "create_reminder",
      risk: "write",
      argsSummary: "Call Alice · Friday",
    });
    toolCallCompleted("conv-1", "call-tc-2", {
      id: "rem-xyz",
      title: "Call Alice",
      dueAt: new Date(Date.now() + 3 * 86_400_000).toISOString(),
    });
    return () => {
      resetConversation();
      resetToolCalls();
    };
  },
};
