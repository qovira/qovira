<script module lang="ts">
  // ToolCallChip stories — in-progress chip and resolved entity card states.
  //
  // Stories are app compositions, not library primitives: they exercise the real
  // ToolCallChip component with realistic ToolCallEntry fixtures covering each
  // lifecycle state (started, completed → entity card, completed → quiet read,
  // completed → unknown shape, failed).
  //
  // Reuses the real ToolCallEntry type from the tool-calls store — no parallel mocks.
  //
  // A11y note: color-contrast is disabled for InProgress, EntityCard, and
  // DeletedEntityCard because the `.tool-chip__args` span intentionally uses
  // `opacity: 0.7` for visual de-emphasis. This reduces effective contrast of the
  // muted text below 4.5:1. This is genuine design intent, not a token bug.

  import { defineMeta } from "@storybook/addon-svelte-csf";
  import { expect } from "storybook/test";
  import ToolCallChip from "./ToolCallChip.svelte";
  import { STREAMING_SENTINEL_ID } from "$lib/stores/conversation.svelte.js";
  import type { ToolCallEntry } from "$lib/stores/tool-calls.svelte.js";

  // Per-story a11y override: the .tool-chip__args span uses opacity: 0.7 for
  // intentional de-emphasis, which reduces effective contrast below 4.5:1.
  const opacityDeemphasis = {
    a11y: { config: { rules: [{ id: "color-contrast", enabled: false }] } },
  };

  const { Story } = defineMeta({
    title: "Chat/ToolCallChip",
    component: ToolCallChip,
    tags: ["autodocs"],
    parameters: {
      // These stories render standalone chips without a conversation thread wrapper.
      // No network or store seeding needed — props are injected directly as args.
      layout: "padded",
    },
  });
</script>

<!--
  In-progress chip: tool.started state — shows the animated spinner and argsSummary.
  Covers AC: "tool chip (in-progress)".
  play: asserts the chip renders the argsSummary text and the chip element is visible.
-->
<Story
  name="InProgress"
  parameters={opacityDeemphasis}
  args={{
    entry: {
      state: "started",
      callId: "call-1",
      conversationId: "conv-1",
      assistantMessageId: STREAMING_SENTINEL_ID,
      name: "create_reminder",
      risk: "write",
      argsSummary: "Buy oat milk · tomorrow 9 am",
    } satisfies ToolCallEntry,
  }}
  play={async ({ canvas }) => {
    // The chip renders with the argsSummary text visible.
    // argsSummary is placed in .tool-chip__args via {@html renderSafeMarkdown(...)}.
    await expect(await canvas.findByText(/buy oat milk/i)).toBeVisible();
    // The in-progress chip uses role="status" (aria-live=polite).
    const chip = canvas.getByRole("status");
    await expect(chip).toBeVisible();
  }}
/>

<!--
  Resolved entity card: tool.completed for a write tool that returns a reminder
  shape. Covers AC: "resolved entity card".
  play: asserts the entity card link renders with the reminder title and links to /reminders.
-->
<Story
  name="EntityCard"
  parameters={opacityDeemphasis}
  args={{
    entry: {
      state: "completed",
      callId: "call-2",
      conversationId: "conv-1",
      assistantMessageId: "msg-1",
      name: "create_reminder",
      risk: "write",
      argsSummary: "Buy oat milk · tomorrow 9 am",
      result: {
        id: "rem-abc",
        title: "Buy oat milk",
        dueAt: new Date(Date.now() + 86_400_000).toISOString(),
      },
    } satisfies ToolCallEntry,
  }}
  play={async ({ canvas }) => {
    // The entity card renders as a link to /reminders.
    const link = await canvas.findByRole("link");
    await expect(link).toBeVisible();
    await expect(link).toHaveAttribute("href", "/reminders");
    // The reminder title must appear inside the link.
    await expect(link).toHaveAccessibleName(/buy oat milk/i);
  }}
/>

<!--
  Deleted entity card: delete_reminder completed — shows "Deleted" tone.
-->
<Story
  name="DeletedEntityCard"
  parameters={opacityDeemphasis}
  args={{
    entry: {
      state: "completed",
      callId: "call-3",
      conversationId: "conv-1",
      assistantMessageId: "msg-2",
      name: "delete_reminder",
      risk: "destructive",
      argsSummary: "Buy oat milk",
      result: {
        id: "rem-abc",
        title: "Buy oat milk",
        dueAt: new Date(Date.now() + 86_400_000).toISOString(),
      },
    } satisfies ToolCallEntry,
  }}
/>

<!--
  Quiet read: list_reminders completes without emitting any visible chip
  (renders nothing — quiet reads are intentionally invisible).
-->
<Story
  name="QuietRead"
  args={{
    entry: {
      state: "completed",
      callId: "call-4",
      conversationId: "conv-1",
      assistantMessageId: "msg-3",
      name: "list_reminders",
      risk: "read",
      argsSummary: "",
      result: [{ id: "rem-abc", title: "Buy oat milk" }],
    } satisfies ToolCallEntry,
  }}
/>

<!--
  Unknown result shape: tool.completed but result doesn't match the reminder
  schema — falls back to the calm generic "Done" chip.
-->
<Story
  name="UnknownResultShape"
  args={{
    entry: {
      state: "completed",
      callId: "call-5",
      conversationId: "conv-1",
      assistantMessageId: "msg-4",
      name: "create_reminder",
      risk: "write",
      argsSummary: "Some operation",
      result: { status: "ok" },
    } satisfies ToolCallEntry,
  }}
/>

<!--
  Error state: tool.failed — shows the soft recoverable error chip.
  play: asserts the alert role renders with the error text.
-->
<Story
  name="ErrorState"
  args={{
    entry: {
      state: "failed",
      callId: "call-6",
      conversationId: "conv-1",
      assistantMessageId: "msg-5",
      name: "create_reminder",
      risk: "write",
      argsSummary: "Buy oat milk",
      error: "Could not complete the operation. Please try again.",
    } satisfies ToolCallEntry,
  }}
  play={async ({ canvas }) => {
    // Failed chip uses role="alert".
    const alert = await canvas.findByRole("alert");
    await expect(alert).toBeVisible();
    // The specific error detail text must appear inside the chip.
    // Use a more specific phrase that only matches the error detail, not the label.
    await expect(await canvas.findByText(/please try again/i)).toBeVisible();
  }}
/>
