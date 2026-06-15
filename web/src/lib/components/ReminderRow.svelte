<script lang="ts">
  // ReminderRow — single row in the reminders list.
  //
  // Shared between the reminders route (+page.svelte) and the RemindersView story
  // wrapper so both exercise the real rendering path — no parallel mocks.
  //
  // Active rows render a tappable check-circle button (sibling to the row-body button)
  // so keyboard users get two independent tab stops: check → row-body.
  // Done rows render a decorative (non-interactive) check icon and only the row-body button.
  //
  // Callback props are optional so the story wrapper can omit them without crashing.

  import { formatDueAt } from "$lib/format/datetime.js";
  import {
    reminders_complete_label,
    reminders_recurring_chip_label,
    reminders_row_open_edit,
  } from "$lib/paraglide/messages.js";
  import type { ReminderItem } from "$lib/stores/reminders.svelte.js";

  interface Props {
    reminder: ReminderItem;
    isDone?: boolean;
    /** Called when the check-circle button is activated (active rows only). */
    oncomplete?: (id: string) => void;
    /** Called when the row body is activated (opens the edit sheet). */
    onedit?: (reminder: ReminderItem) => void;
  }

  const { reminder, isDone = false, oncomplete, onedit }: Props = $props();
</script>

<li class="reminder-row {isDone ? 'reminder-row--done' : ''}">
  {#if !isDone}
    <!--
      Check-circle: interactive button for active reminders.
      A11y: sibling button (NOT nested inside the row-body button).
    -->
    <button
      type="button"
      class="reminder-row__check-btn"
      aria-label={reminders_complete_label()}
      onclick={() => oncomplete?.(reminder.id)}
    >
      <!-- Soft check-circle icon -->
      <svg
        class="reminder-row__check"
        xmlns="http://www.w3.org/2000/svg"
        width="16"
        height="16"
        viewBox="0 0 256 256"
        aria-hidden="true"
      >
        <path
          fill="currentColor"
          d="M173.66 98.34a8 8 0 0 1 0 11.32l-56 56a8 8 0 0 1-11.32 0l-24-24a8 8 0 0 1 11.32-11.32L112 148.69l50.34-50.35a8 8 0 0 1 11.32 0ZM232 128A104 104 0 1 1 128 24a104.11 104.11 0 0 1 104 104Zm-16 0a88 88 0 1 0-88 88a88.1 88.1 0 0 0 88-88Z"
        />
      </svg>
    </button>
  {:else}
    <!-- Done rows: decorative non-interactive check icon -->
    <svg
      class="reminder-row__check"
      xmlns="http://www.w3.org/2000/svg"
      width="16"
      height="16"
      viewBox="0 0 256 256"
      aria-hidden="true"
    >
      <path
        fill="currentColor"
        d="M173.66 98.34a8 8 0 0 1 0 11.32l-56 56a8 8 0 0 1-11.32 0l-24-24a8 8 0 0 1 11.32-11.32L112 148.69l50.34-50.35a8 8 0 0 1 11.32 0ZM232 128A104 104 0 1 1 128 24a104.11 104.11 0 0 1 104 104Zm-16 0a88 88 0 1 0-88 88a88.1 88.1 0 0 0 88-88Z"
      />
    </svg>
  {/if}

  <!-- Row body: tapping opens the edit sheet. Sibling to the check button. -->
  <button
    type="button"
    class="reminder-row__body"
    onclick={() => onedit?.(reminder)}
    aria-label={reminders_row_open_edit()}
  >
    <span class="reminder-row__title">{reminder.title}</span>
    <span class="reminder-row__spacer" aria-hidden="true"></span>
    {#if reminder.rrule !== undefined}
      <span class="reminder-row__recurring-chip" aria-label={reminders_recurring_chip_label()}>↻</span>
    {/if}
    <span class="reminder-row__due">{formatDueAt(reminder.dueAt)}</span>
  </button>
</li>

<style>
  /* =========================================================================
     Reminder row — restructured for sibling-button a11y pattern.
     Layout: [check-btn | row-body-btn (title · spacer · chip · due)]
     The check-btn and row-body-btn are SIBLINGS, never nested.
     ========================================================================= */
  .reminder-row {
    display: flex;
    align-items: center;
    gap: 0.25rem;
    border-radius: 0.375rem;
    min-height: 2.25rem;
    transition: background-color 80ms ease;
  }

  .reminder-row:hover {
    background-color: var(--color-surface-raised, oklch(0.95 0 0));
  }

  .reminder-row--done {
    opacity: 0.6;
  }

  /* Check-circle button (active rows) */
  .reminder-row__check-btn {
    flex-shrink: 0;
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 0.375rem;
    border: none;
    background: transparent;
    cursor: pointer;
    border-radius: 50%;
    color: var(--color-text-muted, #5c4a37);
    transition: color 80ms ease;
  }

  .reminder-row__check-btn:hover {
    color: var(--color-primary, oklch(0.5 0.2 250));
  }

  .reminder-row__check-btn:focus-visible {
    outline: 2px solid currentColor;
    outline-offset: 1px;
  }

  /* Check icon — shared by active (inside button) and done (standalone decorative) */
  .reminder-row__check {
    flex-shrink: 0;
    color: inherit;
  }

  /* Done rows: the check icon is a bare SVG (not a button), needs the same padding */
  .reminder-row--done > .reminder-row__check {
    padding: 0.375rem;
    color: var(--color-text-muted, #5c4a37);
  }

  /* Row body button — fills the rest of the row, opens the edit sheet */
  .reminder-row__body {
    flex: 1;
    display: flex;
    align-items: center;
    gap: 0.5rem;
    padding: 0.5rem 0.25rem;
    border: none;
    background: transparent;
    cursor: pointer;
    text-align: left;
    min-width: 0;
    border-radius: 0.25rem;
  }

  .reminder-row__body:focus-visible {
    outline: 2px solid currentColor;
    outline-offset: 1px;
  }

  /* Title */
  .reminder-row__title {
    font-size: 0.875rem;
    color: var(--color-text, oklch(0.2 0 0));
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  /* Spacer */
  .reminder-row__spacer {
    flex: 1;
  }

  /* Recurring chip */
  .reminder-row__recurring-chip {
    flex-shrink: 0;
    font-size: 0.6875rem;
    font-weight: 600;
    line-height: 1;
    padding: 0.125rem 0.375rem;
    border-radius: 0.25rem;
    background-color: var(--color-honey-100, #f8e6c8);
    color: var(--color-honey-800, #7e4f1c);
  }

  /* Due time */
  .reminder-row__due {
    flex-shrink: 0;
    font-size: 0.75rem;
    color: var(--color-text-muted, #5c4a37);
    white-space: nowrap;
  }
</style>
