<script lang="ts">
  // ToolCallChip — renders the lifecycle of a single tool call inline in the
  // assistant turn.
  //
  // States:
  //   started   → in-progress chip showing the argsSummary
  //   completed → entity card linking the affected row (or a calm "Done" for unknown shapes)
  //   failed    → soft recoverable error showing the model-safe error text
  //
  // Security: argsSummary and error text flow through renderSafeMarkdown()
  // before {@html} — the same pipeline used for assistant message content.
  // No untrusted content is ever placed in {@html} without sanitization.
  //
  // list_reminders (RiskRead) resolves quietly: it shows no entity card link,
  // just a minimal "Done" chip that fades into the background.

  import type { ToolCallEntry } from "$lib/stores/tool-calls.svelte.js";
  import { renderSafeMarkdown } from "$lib/markdown/sanitize.js";
  import { formatDueAt } from "$lib/format/datetime.js";
  import {
    tool_chip_creating,
    tool_chip_updating,
    tool_chip_deleting,
    tool_chip_working,
    tool_chip_done,
    tool_chip_error_label,
    tool_chip_entity_reminder,
    tool_chip_entity_deleted,
  } from "$lib/paraglide/messages.js";

  // ---------------------------------------------------------------------------
  // Reminder result shape — parsed defensively (result is opaque unknown).
  // ---------------------------------------------------------------------------

  interface ReminderResult {
    id: string;
    title: string;
    dueAt?: string | null;
  }

  // ---------------------------------------------------------------------------
  // Props
  // ---------------------------------------------------------------------------

  interface Props {
    entry: ToolCallEntry;
  }

  const { entry }: Props = $props();

  // ---------------------------------------------------------------------------
  // Reminder tool names — the only entity-producing tools in v0.1.
  // list_reminders is a read and resolves quietly (no entity card).
  // ---------------------------------------------------------------------------

  const REMINDER_WRITE_TOOLS = new Set(["create_reminder", "update_reminder", "complete_reminder", "delete_reminder"]);
  const REMINDER_READ_TOOLS = new Set(["list_reminders"]);

  // ---------------------------------------------------------------------------
  // Derived: in-progress label from tool name + risk
  // ---------------------------------------------------------------------------

  const inProgressLabel = $derived.by((): string => {
    if (entry.name === "delete_reminder") return tool_chip_deleting();
    if (entry.name.startsWith("update_") || entry.name.startsWith("complete_")) return tool_chip_updating();
    if (entry.name.startsWith("create_")) return tool_chip_creating();
    return tool_chip_working();
  });

  // ---------------------------------------------------------------------------
  // Derived: reminder entity from completed result (null when shape unrecognized).
  // Parsed defensively — result is opaque unknown.
  // ---------------------------------------------------------------------------

  function parseReminderResult(result: unknown): ReminderResult | null {
    if (result === null || typeof result !== "object") return null;
    const r = result as Record<string, unknown>;
    if (typeof r.id !== "string" || typeof r.title !== "string") return null;
    const dueAt = typeof r.dueAt === "string" ? r.dueAt : null;
    return { id: r.id, title: r.title, dueAt };
  }

  const completedEntry = $derived(entry.state === "completed" ? entry : null);

  const reminderEntity = $derived(
    completedEntry !== null && REMINDER_WRITE_TOOLS.has(entry.name) ? parseReminderResult(completedEntry.result) : null,
  );

  // A quiet read tool (list_reminders) resolves without an entity card.
  const isQuietRead = $derived(REMINDER_READ_TOOLS.has(entry.name));
</script>

{#if entry.state === "started"}
  <!--
    In-progress chip: shows the in-progress label and the argsSummary.
    argsSummary is model-produced JSON — sanitized before {@html}.
  -->
  <div class="tool-chip tool-chip--started" role="status" aria-live="polite">
    <span class="tool-chip__spinner" aria-hidden="true"></span>
    <span class="tool-chip__label">{inProgressLabel}</span>
    <!-- eslint-disable-next-line svelte/no-at-html-tags -->
    <span class="tool-chip__args">{@html renderSafeMarkdown(entry.argsSummary)}</span>
  </div>
{:else if entry.state === "completed"}
  {#if isQuietRead}
    <!-- Routine reads (list_reminders) resolve without visual noise. -->
  {:else if reminderEntity !== null}
    <!--
      Entity card: links to /reminders (the reminders surface owns the row).
      No per-id route exists in v0.1.
    -->
    <a
      href="/reminders"
      class="tool-chip tool-chip--entity"
      aria-label="{entry.name === 'delete_reminder'
        ? tool_chip_entity_deleted()
        : tool_chip_entity_reminder()}: {reminderEntity.title}"
    >
      <span class="tool-chip__entity-type">{tool_chip_entity_reminder()}</span>
      <span class="tool-chip__entity-dot" aria-hidden="true">·</span>
      <span class="tool-chip__entity-title">{reminderEntity.title}</span>
      {#if reminderEntity.dueAt}
        <span class="tool-chip__entity-dot" aria-hidden="true">·</span>
        <span class="tool-chip__entity-due">{formatDueAt(reminderEntity.dueAt)}</span>
      {/if}
      <span class="tool-chip__entity-arrow" aria-hidden="true">→</span>
    </a>
  {:else}
    <!-- Unknown/unrecognized result shape — calm generic "Done" card, no crash. -->
    <div class="tool-chip tool-chip--done" role="status">
      <span class="tool-chip__label">{tool_chip_done()}</span>
    </div>
  {/if}
{:else if entry.state === "failed"}
  <!--
    Soft recoverable error: model-safe error text only.
    error text is server-produced — sanitized before {@html}.
  -->
  <div class="tool-chip tool-chip--failed" role="alert">
    <span class="tool-chip__error-label">{tool_chip_error_label()}</span>
    <span class="tool-chip__error-dot" aria-hidden="true">·</span>
    <!-- eslint-disable-next-line svelte/no-at-html-tags -->
    <span class="tool-chip__error-text">{@html renderSafeMarkdown(entry.error)}</span>
  </div>
{/if}

<style>
  /* Base chip: a subtle inline row that doesn't compete with conversation text. */
  .tool-chip {
    display: inline-flex;
    align-items: baseline;
    gap: 0.375rem;
    font-size: 0.75rem;
    border-radius: 0.375rem;
    padding: 0.25rem 0.625rem;
    max-width: 100%;
    overflow: hidden;
  }

  /* In-progress chip: muted surface, pulse animation on the spinner dot. */
  .tool-chip--started {
    background-color: var(--color-surface-raised, oklch(0.95 0 0));
    color: var(--color-fg-muted, #5c4a37);
  }

  .tool-chip__spinner {
    display: inline-block;
    width: 0.5rem;
    height: 0.5rem;
    border-radius: 50%;
    background-color: currentColor;
    animation: chip-pulse 1.4s ease-in-out infinite;
    flex-shrink: 0;
    align-self: center;
  }

  @keyframes chip-pulse {
    0%,
    100% {
      opacity: 0.3;
    }
    50% {
      opacity: 1;
    }
  }

  .tool-chip__args {
    opacity: 0.7;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  /* Entity card: calm link styled as a subtle pill. */
  .tool-chip--entity {
    background-color: var(--color-surface-raised, oklch(0.95 0 0));
    color: var(--color-fg, oklch(0.2 0 0));
    text-decoration: none;
    cursor: pointer;
    transition: background-color 120ms ease;
  }

  .tool-chip--entity:hover {
    background-color: var(--color-surface-overlay, oklch(0.9 0 0));
  }

  .tool-chip--entity:focus-visible {
    outline: 2px solid currentColor;
    outline-offset: 2px;
  }

  .tool-chip__entity-type {
    color: var(--color-fg-muted, #5c4a37);
  }

  .tool-chip__entity-dot {
    color: var(--color-fg-muted, #5c4a37);
  }

  .tool-chip__entity-title {
    font-weight: 500;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 20ch;
  }

  .tool-chip__entity-due {
    color: var(--color-fg-muted, #5c4a37);
  }

  .tool-chip__entity-arrow {
    color: var(--color-fg-muted, #5c4a37);
    font-size: 0.65rem;
    align-self: center;
  }

  /* Done chip: invisible/minimal for successful reads. */
  .tool-chip--done {
    color: var(--color-fg-muted, #5c4a37);
    background-color: transparent;
    padding-inline: 0;
  }

  /* Error chip: soft, non-alarming, recoverable. */
  .tool-chip--failed {
    background-color: var(--color-surface-raised, oklch(0.95 0 0));
    color: var(--color-fg-error, #a8331f);
  }

  .tool-chip__error-label {
    font-weight: 500;
  }

  .tool-chip__error-dot {
    color: var(--color-fg-muted, #5c4a37);
  }

  .tool-chip__error-text {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
</style>
