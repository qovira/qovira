// ConfirmationCard interaction stories — adds play functions for the key behaviors
// not covered by the static-state Svelte CSF stories:
//
//   DenyClick                    — resolveConfirmation called with "deny"; POST fires once.
//   ApproveClick                 — resolveConfirmation called with "approve"; POST fires once.
//   NoDoubleResolveWhileResolving — a second click during in-flight resolve is blocked by
//                                   the `disabled={buttonsDisabled || resolving}` binding.
//                                   A third click via fireEvent (which bypasses disabled)
//                                   then directly tests the `if (buttonsDisabled || resolving) return;`
//                                   JS guard in resolve() itself.
//   ExpiryTimerDisablesButtons   — an already-expired card (expiresAt in the past) has
//                                  buttons disabled without needing fake timers. The
//                                  component's $derived `buttonsDisabled` covers this via
//                                  `secondsRemaining === 0` from computeSecondsRemaining().
//
// NOTE: vi.useFakeTimers() is intentionally NOT used in these stories. In Vitest Browser
// Mode (Storybook project), fake timers break the story rendering loop (Storybook's
// internal timer-based rendering stalls). The expiry timer behavior is covered by the
// "already-expired" path below and by the existing unit tests in confirmations.svelte.test.ts.
//
// CSF 3 format (not Svelte CSF) because vi.mock hoisting is required for mocking Api.POST.

import { vi } from "vitest";
import type { Meta, StoryObj } from "@storybook/sveltekit";
import { expect, waitFor, userEvent, fireEvent } from "storybook/test";
import type { ConfirmationEntry, PendingConfirmation } from "$lib/stores/confirmations.svelte.js";
import ConfirmationCard from "./ConfirmationCard.svelte";

// ---------------------------------------------------------------------------
// Module mocks — hoisted by Vitest above imports.
// Mock Api minimally: only POST is needed for these interaction tests.
// ---------------------------------------------------------------------------

vi.mock("$lib/api/index.js", () => ({
  Api: {
    POST: vi.fn(),
    GET: vi.fn(),
    PATCH: vi.fn(),
    DELETE: vi.fn(),
    PUT: vi.fn(),
  },
  ProblemError: class ProblemError extends Error {
    name = "ProblemError";
    constructor(raw: { code: string; detail: string }) {
      super(`${raw.code}: ${raw.detail}`);
    }
  },
}));

// ---------------------------------------------------------------------------
// Import after mock registration
// ---------------------------------------------------------------------------

import { Api } from "$lib/api/index.js";
import { resetConfirmations, confirmationRequired } from "$lib/stores/confirmations.svelte.js";

// ---------------------------------------------------------------------------
// Type helper for accessing Api.POST mock call arguments.
// Api.POST is typed as Client<paths>["POST"] (a complex overloaded generic).
// We cast the mock call args to the concrete shape that resolveConfirmation
// passes — a single `as Array<…>` cast (no double-cast through unknown).
// ---------------------------------------------------------------------------

interface PostCallOptions {
  body?: { decision: string };
}

function getFirstPostCallDecision(): string | undefined {
  const calls = vi.mocked(Api.POST).mock.calls as [unknown, PostCallOptions][];
  return calls[0]?.[1]?.body?.decision;
}

// ---------------------------------------------------------------------------
// Meta
// ---------------------------------------------------------------------------

const meta = {
  title: "Chat/ConfirmationCard/Interactions",
  component: ConfirmationCard,
  tags: ["autodocs"],
  parameters: {
    layout: "padded",
    // color-contrast: same reasoning as ConfirmationCard.stories.svelte
    a11y: { config: { rules: [{ id: "color-contrast", enabled: false }] } },
  },
} satisfies Meta<typeof ConfirmationCard>;

export default meta;
type Story = StoryObj<typeof meta>;

// ---------------------------------------------------------------------------
// Shared pending entry fixture
// ---------------------------------------------------------------------------

const CONV_ID = "conv-interact-1";

function makePendingEntry(callId: string): PendingConfirmation {
  return {
    state: "pending",
    callId,
    conversationId: CONV_ID,
    assistantMessageId: "__confirmation_streaming__",
    name: "update_reminder",
    risk: "write",
    args: { id: "rem-abc", title: "Buy oat milk" },
    expiresAt: new Date(Date.now() + 120_000).toISOString(),
  };
}

// ---------------------------------------------------------------------------
// ExpiryTimerDisablesButtons — uses an already-expired card (no fake timers needed).
// The component's buttonsDisabled = $derived(entry.state !== "pending" || secondsRemaining === 0).
// An entry with state "expired" renders with no action buttons at all (via #if entry.state !== "expired").
// So we test via state="pending" + expiresAt in the past → secondsRemaining=0 on first compute.
// ---------------------------------------------------------------------------
export const ExpiryTimerDisablesButtons: Story = {
  args: {
    entry: {
      state: "pending",
      callId: "confirm-expiry-already",
      conversationId: CONV_ID,
      assistantMessageId: "__confirmation_streaming__",
      name: "update_reminder",
      risk: "write",
      args: { id: "rem-abc", title: "Buy oat milk" },
      // expiresAt is 5 seconds in the past — computeSecondsRemaining() returns 0 on mount.
      expiresAt: new Date(Date.now() - 5_000).toISOString(),
    } satisfies ConfirmationEntry,
  },
  play: async ({ canvas }) => {
    // Buttons must be rendered (state is "pending", not "expired").
    const approveBtn = await canvas.findByRole("button", { name: /approve/i });
    const denyBtn = canvas.getByRole("button", { name: /deny/i });

    // Both must be disabled: secondsRemaining=0 because expiresAt is already past.
    await expect(approveBtn).toBeDisabled();
    await expect(denyBtn).toBeDisabled();
  },
};

// ---------------------------------------------------------------------------
// DenyClick — clicking Deny calls resolveConfirmation (POST) with "deny"
// ---------------------------------------------------------------------------
export const DenyClick: Story = {
  args: {
    entry: { ...makePendingEntry("confirm-deny-1") } satisfies ConfirmationEntry,
  },
  beforeEach: () => {
    resetConfirmations();
    // Seed the store so resolveConfirmation can find the entry by callId.
    confirmationRequired({
      callId: "confirm-deny-1",
      conversationId: CONV_ID,
      name: "update_reminder",
      risk: "write",
      args: { id: "rem-abc", title: "Buy oat milk" },
      expiresAt: new Date(Date.now() + 120_000).toISOString(),
    });
    // POST succeeds with 202 (no error field).
    vi.mocked(Api.POST).mockResolvedValue({
      data: undefined,
      error: undefined,
      response: new Response("", { status: 202 }),
    });
    return () => {
      resetConfirmations();
      vi.mocked(Api.POST).mockReset();
    };
  },
  play: async ({ canvas }) => {
    const denyBtn = await canvas.findByRole("button", { name: /deny/i });
    await expect(denyBtn).not.toBeDisabled();

    await userEvent.click(denyBtn);

    // resolveConfirmation must have been invoked via Api.POST.
    await waitFor(async () => {
      await expect(vi.mocked(Api.POST)).toHaveBeenCalledTimes(1);
    });
    // Verify the decision in the POST body.
    await expect(getFirstPostCallDecision()).toBe("deny");
  },
};

// ---------------------------------------------------------------------------
// ApproveClick — clicking Approve calls resolveConfirmation with "approve"
// ---------------------------------------------------------------------------
export const ApproveClick: Story = {
  args: {
    entry: { ...makePendingEntry("confirm-approve-1") } satisfies ConfirmationEntry,
  },
  beforeEach: () => {
    resetConfirmations();
    confirmationRequired({
      callId: "confirm-approve-1",
      conversationId: CONV_ID,
      name: "update_reminder",
      risk: "write",
      args: { id: "rem-abc", title: "Buy oat milk" },
      expiresAt: new Date(Date.now() + 120_000).toISOString(),
    });
    vi.mocked(Api.POST).mockResolvedValue({
      data: undefined,
      error: undefined,
      response: new Response("", { status: 202 }),
    });
    return () => {
      resetConfirmations();
      vi.mocked(Api.POST).mockReset();
    };
  },
  play: async ({ canvas }) => {
    const approveBtn = await canvas.findByRole("button", { name: /approve/i });
    await expect(approveBtn).not.toBeDisabled();

    await userEvent.click(approveBtn);

    await waitFor(async () => {
      await expect(vi.mocked(Api.POST)).toHaveBeenCalledTimes(1);
    });
    await expect(getFirstPostCallDecision()).toBe("approve");
  },
};

// ---------------------------------------------------------------------------
// NoDoubleResolveWhileResolving — a second click during in-flight resolve is ignored
// ---------------------------------------------------------------------------
export const NoDoubleResolveWhileResolving: Story = {
  args: {
    entry: { ...makePendingEntry("confirm-dbl-1") } satisfies ConfirmationEntry,
  },
  beforeEach: () => {
    resetConfirmations();
    confirmationRequired({
      callId: "confirm-dbl-1",
      conversationId: CONV_ID,
      name: "update_reminder",
      risk: "write",
      args: { id: "rem-abc", title: "Buy oat milk" },
      expiresAt: new Date(Date.now() + 120_000).toISOString(),
    });

    // POST hangs (never resolves) so the first click stays "resolving" while we click again.
    let resolvePending: ((v: unknown) => void) | null = null;
    vi.mocked(Api.POST).mockImplementation(
      () =>
        new Promise((resolve) => {
          resolvePending = resolve;
        }),
    );

    return () => {
      // Release the hanging promise to avoid open handles.
      if (resolvePending !== null) {
        resolvePending({ data: undefined, error: undefined, response: new Response("", { status: 202 }) });
      }
      resetConfirmations();
      vi.mocked(Api.POST).mockReset();
    };
  },
  play: async ({ canvas }) => {
    const approveBtn = await canvas.findByRole("button", { name: /approve/i });

    // Click once — starts the in-flight resolve (sets resolving=true internally,
    // which makes the button disabled via `disabled={buttonsDisabled || resolving}`).
    await userEvent.click(approveBtn);

    // Button must be disabled while resolving.
    await expect(approveBtn).toBeDisabled();

    // A second userEvent.click on a disabled button is blocked by the browser —
    // no click event fires, proving the disabled binding guards the UI.
    await userEvent.click(approveBtn);
    await expect(vi.mocked(Api.POST)).toHaveBeenCalledTimes(1);

    // A third click via fireEvent bypasses the disabled attribute to reach
    // the onclick handler directly. The `if (buttonsDisabled || resolving) return;`
    // early-return guard in resolve() prevents a second POST call.
    await fireEvent.click(approveBtn);
    await expect(vi.mocked(Api.POST)).toHaveBeenCalledTimes(1);
  },
};
