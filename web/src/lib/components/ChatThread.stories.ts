// ChatThread stories — exercises the chat surface's streaming text and error states.
//
// These stories are app compositions, not library primitives. They use the real conversation store
// (setActiveConversation, applyStreamingDelta, setTurnFailed, resetConversation, toolCallStarted,
// toolCallCompleted) seeded via beforeEach so the actual rendering paths in ChatThread.svelte (and therefore
// +page.svelte) are exercised — not parallel mocks.
//
// CSF 3 format (not Svelte CSF) because the beforeEach store-seeding logic is pure TypeScript and requires no
// Svelte template complexity.

import type { Meta, StoryObj } from "@storybook/sveltekit";
import { expect } from "storybook/test";

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
// play: asserts the streaming cursor dot is visible (aria-hidden pulse span).
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
  play: async ({ canvas }) => {
    // The accumulated streaming text must be in the DOM (inside the assistant slot).
    await expect(await canvas.findByText(/I'll set that reminder/i)).toBeVisible();
    // The animated cursor dot is an aria-hidden span (invisible to screen readers) with no role, label, or
    // testid. A CSS-class selector is the only available locator; this assertion is low value but documents
    // the streaming-state rendering path. To improve: add data-testid="streaming-cursor" to
    // ChatThread.svelte's span.
    const cursorDot = document.querySelector(".animate-pulse");
    await expect(cursorDot).not.toBeNull();
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
// play: asserts the error alert is visible.
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
  play: async ({ canvas }) => {
    // The error alert (role="alert") must be visible.
    const alert = await canvas.findByRole("alert");
    await expect(alert).toBeVisible();
  },
};

// Per-story a11y override: renders ToolCallChip whose .tool-chip__args span uses opacity: 0.7 for intentional
// de-emphasis — same real failure as in ToolCallChip's own InProgress story. Design intent, not a token bug.
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

// ---------------------------------------------------------------------------
// XssBoundary: assistant message containing adversarial HTML — proves the renderSafeMarkdown() sanitize
// boundary at the ChatThread level.
//
// Seeds the conversation store with two XSS payloads:
//   1. <img src=x onerror=alert(1)>  — event handler injection
//   2. [x](javascript:alert(1))       — javascript: URI injection
//
// Asserts against the RENDERED DOM that:
//   - No element has an onerror attribute
//   - No <script> element exists
//   - No <a> element has a javascript: href
//
// This pins the sanitize boundary at the component level (not just the helper unit test).
//
// A11y note: image-alt is disabled for this story. The XSS payload `<img src=x>` may survive DOMPurify as an
// img without an alt attribute (onerror is stripped, but the img element itself is in the allowlist). This is
// a real test of the onerror-strip behavior — the alt-attr concern is a separate a11y issue in the input
// content, not in the component rendering, and is out of scope for this security boundary test.
// ---------------------------------------------------------------------------
export const XssBoundary: Story = {
  parameters: {
    a11y: { config: { rules: [{ id: "image-alt", enabled: false }] } },
  },
  beforeEach: () => {
    resetConversation();
    resetToolCalls();
    setActiveConversation("conv-xss", [
      {
        id: "msg-user-xss",
        role: "user",
        content: "Is this safe?",
        createdAt: new Date(Date.now() - 5_000).toISOString(),
        abandoned: false,
      },
      {
        id: "msg-asst-xss",
        role: "assistant",
        // Three XSS payloads in a single message:
        //   1. <img onerror=…>      — event-handler injection (onerror stripped by DOMPurify)
        //   2. [x](javascript:…)    — javascript: URI injection (href stripped by DOMPurify)
        //   3. <script>alert(1)</script> — script tag injection (removed by DOMPurify)
        // Including the <script> payload makes the `scripts.length === 0` assertion load-bearing: without
        // sanitization the script tag would survive in the DOM.
        content: "<img src=x onerror=alert(1)>\n\n[x](javascript:alert(1))\n\n<script>alert(1)</script>",
        createdAt: new Date(Date.now() - 2_000).toISOString(),
        abandoned: false,
      },
    ]);
    return () => {
      resetConversation();
      resetToolCalls();
    };
  },
  play: async ({ canvas }) => {
    // Wait for the user message to render first (confirms the thread loaded).
    await canvas.findByText("Is this safe?");

    // Assert against the DOM — no onerror attribute anywhere in the document.
    const onerrorEls = document.querySelectorAll("[onerror]");
    await expect(onerrorEls.length).toBe(0);

    // No <script> elements must exist inside the canvas.
    const canvasContainer = canvas.getByRole("list");
    const scripts = canvasContainer.querySelectorAll("script");
    await expect(scripts.length).toBe(0);

    // No <a> with javascript: href anywhere in the document.
    const links = document.querySelectorAll("a[href]");
    const jsLinks = Array.from(links).filter((a) =>
      (a.getAttribute("href") ?? "").toLowerCase().startsWith("javascript:"),
    );
    await expect(jsLinks.length).toBe(0);
  },
};
