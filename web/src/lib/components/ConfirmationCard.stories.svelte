<script module lang="ts">
  // ConfirmationCard stories — covers the pending (routine + destructive),
  // resolved (approved, denied), and expired states of the confirmation card.
  //
  // The ConfirmationCard uses the confirmations store's `resolveConfirmation`
  // for the POST action; stories supply injected entries directly as props —
  // no network call is made during story render.
  //
  // Reuses the real ConfirmationEntry types from the confirmations store.
  //
  // A11y note: color-contrast is disabled for all stories. The timer text uses
  // `opacity: 0.75` and the args text uses `opacity: 0.8` for intentional
  // de-emphasis, reducing effective contrast below 4.5:1. The Expired story
  // additionally applies `opacity: 0.5` + `filter: grayscale(0.6)`. All are
  // genuine design intent, not token bugs.

  import { defineMeta } from "@storybook/addon-svelte-csf";
  import ConfirmationCard from "./ConfirmationCard.svelte";
  import type { ConfirmationEntry } from "$lib/stores/confirmations.svelte.js";

  const { Story } = defineMeta({
    title: "Chat/ConfirmationCard",
    component: ConfirmationCard,
    tags: ["autodocs"],
    parameters: {
      layout: "padded",
      // color-contrast disabled: timer (opacity: 0.75) and args (opacity: 0.8)
      // are intentional de-emphasis and do not reach 4.5:1.
      a11y: { config: { rules: [{ id: "color-contrast", enabled: false }] } },
    },
  });
</script>

<!--
  Pending routine-write card: honey-bordered style, active approve/deny buttons.
  Covers AC: "confirmation card".
-->
<Story
  name="PendingRoutine"
  args={{
    entry: {
      state: "pending",
      callId: "confirm-1",
      conversationId: "conv-1",
      assistantMessageId: "__confirmation_streaming__",
      name: "update_reminder",
      risk: "write",
      args: { id: "rem-abc", title: "Buy oat milk", dueAt: "2026-06-16T09:00:00Z" },
      expiresAt: new Date(Date.now() + 120_000).toISOString(),
    } satisfies ConfirmationEntry,
  }}
/>

<!--
  Pending destructive card: clay/error surface, high-risk tone.
-->
<Story
  name="PendingDestructive"
  args={{
    entry: {
      state: "pending",
      callId: "confirm-2",
      conversationId: "conv-1",
      assistantMessageId: "__confirmation_streaming__",
      name: "delete_reminder",
      risk: "destructive",
      args: { id: "rem-abc" },
      expiresAt: new Date(Date.now() + 120_000).toISOString(),
    } satisfies ConfirmationEntry,
  }}
/>

<!--
  Resolved approved: buttons disabled, "Approved" status shown.
-->
<Story
  name="ResolvedApproved"
  args={{
    entry: {
      state: "resolved",
      callId: "confirm-3",
      conversationId: "conv-1",
      assistantMessageId: "msg-1",
      name: "delete_reminder",
      risk: "destructive",
      args: { id: "rem-abc" },
      expiresAt: new Date(Date.now() + 120_000).toISOString(),
      decision: "approve",
    } satisfies ConfirmationEntry,
  }}
/>

<!--
  Resolved denied: buttons disabled, "Denied" status shown.
-->
<Story
  name="ResolvedDenied"
  args={{
    entry: {
      state: "resolved",
      callId: "confirm-4",
      conversationId: "conv-1",
      assistantMessageId: "msg-2",
      name: "update_reminder",
      risk: "write",
      args: { id: "rem-abc", title: "Buy oat milk" },
      expiresAt: new Date(Date.now() + 120_000).toISOString(),
      decision: "deny",
    } satisfies ConfirmationEntry,
  }}
/>

<!--
  Expired: greyed out, no action buttons visible.
  A11y: covered by the meta-level disable (opacity: 0.5 + grayscale(0.6) on top of timer/args de-emphasis).
-->
<Story
  name="Expired"
  args={{
    entry: {
      state: "expired",
      callId: "confirm-5",
      conversationId: "conv-1",
      assistantMessageId: "msg-3",
      name: "delete_reminder",
      risk: "destructive",
      args: { id: "rem-abc" },
      expiresAt: new Date(Date.now() - 1_000).toISOString(),
    } satisfies ConfirmationEntry,
  }}
/>
