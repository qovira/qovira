<script lang="ts">
  // Reminders page — grouped chronological view with live SSE updates.
  //
  // Renders four time buckets (Overdue · Today · This week · Later) plus a
  // "Done" section loaded on demand. Live updates fall out for free because all
  // derives are over the module-level $state in the reminders store, which the
  // SSE client already patches on every reminder.* event.
  //
  // Bucket boundary choice (see also src/lib/reminders/bucket.ts):
  //   Overdue:    dueAt < now
  //   Today:      now <= dueAt < next local midnight
  //   This week:  next local midnight <= dueAt < next Monday 00:00 local (ISO week)
  //   Later:      dueAt >= next Monday 00:00 local
  //   Done:       status === "completed", loaded on demand via GET /reminders?status=completed
  //
  // Security: reminder.title flows through {reminder.title} — Svelte escapes it
  // automatically. Never use {@html} for user-supplied content.

  import { Api } from "$lib/api/index.js";
  import { getReminders, upsertReminder } from "$lib/stores/reminders.svelte.js";
  import type { ReminderItem } from "$lib/stores/reminders.svelte.js";
  import { bucketReminders, shouldShowPlaceholder } from "$lib/reminders/bucket.js";
  import { formatDueAt } from "$lib/format/datetime.js";
  import {
    reminders_placeholder,
    reminders_bucket_overdue,
    reminders_bucket_today,
    reminders_bucket_this_week,
    reminders_bucket_later,
    reminders_bucket_done,
    reminders_done_toggle_label,
    reminders_recurring_chip_label,
    reminders_done_loading,
    reminders_done_empty,
    reminders_done_load_error,
  } from "$lib/paraglide/messages.js";

  // ---------------------------------------------------------------------------
  // "now" — seeded at component init. Correct for the initial render.
  // A $derived recomputes automatically when used inside $derived.
  // For a periodic refresh (optional polish), a setInterval could update now,
  // but the issue marks that out of scope. A single seed is sufficient for AC2/3
  // (live SSE updates retrigger $derived recomputation automatically).
  // ---------------------------------------------------------------------------
  let now = $state(new Date());

  // ---------------------------------------------------------------------------
  // Bucketed views — derived from the live store, recompute on every mutation.
  // ---------------------------------------------------------------------------
  const buckets = $derived(bucketReminders(getReminders(), now));

  // Aliases for template readability.
  const overdue = $derived(buckets.overdue);
  const today = $derived(buckets.today);
  const thisWeek = $derived(buckets.thisWeek);
  const later = $derived(buckets.later);
  const done = $derived(buckets.done);

  // True when there are no active reminders at all (any bucket).
  const hasNoActive = $derived(
    overdue.length === 0 && today.length === 0 && thisWeek.length === 0 && later.length === 0,
  );

  // Fix 2: Show placeholder only when there are NO reminders at all — no active
  // ones and no done ones. When done.length > 0 with no active reminders, the
  // Done section still has content; suppress the placeholder.
  const showPlaceholder = $derived(shouldShowPlaceholder(hasNoActive, done.length));

  // ---------------------------------------------------------------------------
  // Done section — loaded on demand (disclosure pattern).
  // ---------------------------------------------------------------------------

  /** True once the Done section has been opened at least once. */
  let doneOpen = $state(false);
  /** True while the initial Done fetch is in flight. */
  let doneLoading = $state(false);
  /** Error message if the Done fetch failed. */
  let doneError = $state<string | null>(null);
  // Fix 3: dedicated flag decoupled from done.length — set once in the finally
  // block so we never re-fetch even if done.length is still 0 after the server
  // confirms there are no completed reminders.
  let doneFetched = $state(false);

  /**
   * Open the Done section and lazily fetch all completed reminders exactly once.
   * Subsequent opens skip the fetch — the store is up-to-date and future
   * reminder.completed events keep it current via upsertReminder.
   */
  async function openDone(): Promise<void> {
    doneOpen = true;

    // Fix 3: gate on the dedicated fetched flag, not done.length.
    if (doneFetched || doneLoading) return;

    doneLoading = true;
    doneError = null;

    try {
      let cursor: string | null = null;

      for (;;) {
        // exactOptionalPropertyTypes: spread cursor so absent key is omitted.
        const query: { status: "completed"; cursor?: string } =
          cursor !== null ? { status: "completed", cursor } : { status: "completed" };
        const result = await Api.GET("/reminders", { params: { query } });
        const page = result.data;

        if (page?.data !== undefined) {
          for (const r of page.data) {
            upsertReminder(r);
          }
        }

        const nextCursor: string | null = page?.pagination.nextCursor ?? null;
        if (nextCursor !== null && nextCursor !== "") {
          cursor = nextCursor;
        } else {
          break;
        }
      }
    } catch {
      // Fix 1: use the Paraglide key instead of a hardcoded English string.
      doneError = reminders_done_load_error();
    } finally {
      doneLoading = false;
      // Fix 3: mark as fetched regardless of success/failure so re-opens skip.
      doneFetched = true;
    }
  }

  function toggleDone(): void {
    if (!doneOpen) {
      void openDone();
    } else {
      doneOpen = false;
    }
  }
</script>

<!--
  Single-column reminders view.
  Only renders bucket headings that have rows (empty buckets are skipped).
  Fix 2: the empty state renders only when there are NO reminders at all
  (no active ones and no done ones).
-->

<!--
  Fix 4: single row snippet used across all five lists.
  The `done` parameter (default false) adds the --done modifier for completed rows.
  {#snippet} + {@render} is idiomatic Svelte 5; it does not trigger
  @typescript-eslint/no-confusing-void-expression — that rule fires only on
  void-returning calls in expression position (e.g. arrow functions), not on
  {@render} markup tags.
-->
{#snippet row(reminder: ReminderItem, done: boolean = false)}
  <li class="reminder-row {done ? 'reminder-row--done' : ''}">
    <!-- Soft check-circle icon — decorative, non-interactive -->
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
    <span class="reminder-row__title">{reminder.title}</span>
    <span class="reminder-row__spacer" aria-hidden="true"></span>
    {#if reminder.rrule !== undefined}
      <span class="reminder-row__recurring-chip" aria-label={reminders_recurring_chip_label()}>↻</span>
    {/if}
    <span class="reminder-row__due">{formatDueAt(reminder.dueAt)}</span>
  </li>
{/snippet}

<div class="reminders-page">
  {#if showPlaceholder}
    <!-- Empty state: brand-voice copy from the reminders_placeholder message key. -->
    <p class="text-text-subtle text-sm">{reminders_placeholder()}</p>
  {:else if hasNoActive}
    <!-- Zero active but some done — render no active buckets; Done section below shows them. -->
  {:else}
    <!-- Overdue -->
    {#if overdue.length > 0}
      <section class="reminders-bucket" aria-labelledby="bucket-overdue">
        <h2 id="bucket-overdue" class="reminders-bucket__heading reminders-bucket__heading--overdue">
          {reminders_bucket_overdue()}
        </h2>
        <ul class="reminders-list" role="list">
          {#each overdue as reminder (reminder.id)}
            {@render row(reminder)}
          {/each}
        </ul>
      </section>
    {/if}

    <!-- Today -->
    {#if today.length > 0}
      <section class="reminders-bucket" aria-labelledby="bucket-today">
        <h2 id="bucket-today" class="reminders-bucket__heading">
          {reminders_bucket_today()}
        </h2>
        <ul class="reminders-list" role="list">
          {#each today as reminder (reminder.id)}
            {@render row(reminder)}
          {/each}
        </ul>
      </section>
    {/if}

    <!-- This week -->
    {#if thisWeek.length > 0}
      <section class="reminders-bucket" aria-labelledby="bucket-this-week">
        <h2 id="bucket-this-week" class="reminders-bucket__heading">
          {reminders_bucket_this_week()}
        </h2>
        <ul class="reminders-list" role="list">
          {#each thisWeek as reminder (reminder.id)}
            {@render row(reminder)}
          {/each}
        </ul>
      </section>
    {/if}

    <!-- Later -->
    {#if later.length > 0}
      <section class="reminders-bucket" aria-labelledby="bucket-later">
        <h2 id="bucket-later" class="reminders-bucket__heading">
          {reminders_bucket_later()}
        </h2>
        <ul class="reminders-list" role="list">
          {#each later as reminder (reminder.id)}
            {@render row(reminder)}
          {/each}
        </ul>
      </section>
    {/if}
  {/if}

  <!-- Done section — always available, opened on demand -->
  <section class="reminders-bucket reminders-bucket--done" aria-labelledby="bucket-done">
    <button
      type="button"
      class="reminders-done-toggle"
      onclick={toggleDone}
      aria-expanded={doneOpen}
      aria-label={reminders_done_toggle_label()}
    >
      <h2 id="bucket-done" class="reminders-bucket__heading">
        {reminders_bucket_done()}
        {#if done.length > 0}
          <span class="reminders-done-count" aria-hidden="true">({done.length})</span>
        {/if}
      </h2>
      <!-- Chevron indicator — down when open, right when closed -->
      <svg
        class="reminders-done-chevron {doneOpen ? 'reminders-done-chevron--open' : ''}"
        xmlns="http://www.w3.org/2000/svg"
        width="14"
        height="14"
        viewBox="0 0 256 256"
        aria-hidden="true"
      >
        <path
          fill="currentColor"
          d="m213.66 101.66-80 80a8 8 0 0 1-11.32 0l-80-80A8 8 0 0 1 53.66 90.34L128 164.69l74.34-74.35a8 8 0 0 1 11.32 11.32Z"
        />
      </svg>
    </button>

    {#if doneOpen}
      {#if doneLoading}
        <!-- Fix 1: i18n key instead of hardcoded "Loading…" -->
        <p class="text-text-subtle text-sm" role="status" aria-live="polite">{reminders_done_loading()}</p>
      {:else if doneError !== null}
        <p class="text-text-error text-sm" role="alert">{doneError}</p>
      {:else if done.length === 0}
        <!-- Fix 1: i18n key instead of hardcoded "No completed reminders yet." -->
        <p class="text-text-subtle text-sm">{reminders_done_empty()}</p>
      {:else}
        <ul class="reminders-list" role="list">
          {#each done as reminder (reminder.id)}
            {@render row(reminder, true)}
          {/each}
        </ul>
      {/if}
    {/if}
  </section>
</div>

<style>
  /* Page wrapper — single calm column, no second permanent pane. */
  .reminders-page {
    display: flex;
    flex-direction: column;
    gap: 1.5rem;
  }

  /* Bucket section — heading + list. */
  .reminders-bucket {
    display: flex;
    flex-direction: column;
    gap: 0.375rem;
  }

  /* Bucket heading — understated label above the list. */
  .reminders-bucket__heading {
    display: flex;
    align-items: center;
    gap: 0.375rem;
    font-size: 0.6875rem;
    font-weight: 600;
    letter-spacing: 0.06em;
    text-transform: uppercase;
    color: var(--color-text-subtle, oklch(0.55 0 0));
    padding-bottom: 0.25rem;
    border-bottom: 1px solid var(--color-border, oklch(0.9 0 0));
    margin-bottom: 0.125rem;
    pointer-events: none; /* Heading is inside the toggle button; no redundant click area */
  }

  /* Overdue heading gets a slightly warmer tone for urgency without alarm. */
  .reminders-bucket__heading--overdue {
    color: var(--color-text-error, oklch(0.45 0.18 25));
  }

  /* Done count badge — muted parenthetical. */
  .reminders-done-count {
    color: var(--color-text-subtle, oklch(0.55 0 0));
    font-weight: 400;
  }

  /* Done toggle — looks like the heading, acts as a button. */
  .reminders-done-toggle {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    background: none;
    border: none;
    padding: 0;
    cursor: pointer;
    width: 100%;
    text-align: left;
  }

  .reminders-done-toggle .reminders-bucket__heading {
    pointer-events: auto;
    flex: 1;
    margin-bottom: 0;
    border-bottom: none;
  }

  .reminders-done-toggle:focus-visible {
    outline: 2px solid currentColor;
    outline-offset: 2px;
    border-radius: 0.25rem;
  }

  /* Chevron rotates 0° when closed, 180° when open. */
  .reminders-done-chevron {
    color: var(--color-text-subtle, oklch(0.55 0 0));
    flex-shrink: 0;
    transition: transform 150ms ease;
  }

  .reminders-done-chevron--open {
    transform: rotate(180deg);
  }

  /* Reminder list — vertical stack, no markers. */
  .reminders-list {
    display: flex;
    flex-direction: column;
    list-style: none;
    padding: 0;
    margin: 0;
  }

  /* Reminder row — one row per item: icon · title · spacer · chip · due */
  .reminder-row {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    padding: 0.5rem 0.25rem;
    border-radius: 0.375rem;
    min-height: 2.25rem;
    transition: background-color 80ms ease;
  }

  .reminder-row:hover {
    background-color: var(--color-surface-raised, oklch(0.95 0 0));
  }

  /* Done rows are visually de-emphasised. */
  .reminder-row--done {
    opacity: 0.6;
  }

  /* Check-circle icon — soft, decorative. */
  .reminder-row__check {
    flex-shrink: 0;
    color: var(--color-text-subtle, oklch(0.55 0 0));
  }

  /* Title — primary text, grows to fill available space. */
  .reminder-row__title {
    font-size: 0.875rem;
    color: var(--color-text, oklch(0.2 0 0));
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  /* Spacer — pushes due-time and chip to the trailing edge. */
  .reminder-row__spacer {
    flex: 1;
  }

  /* Honey ↻ chip — recurring indicator. */
  .reminder-row__recurring-chip {
    flex-shrink: 0;
    font-size: 0.6875rem;
    font-weight: 600;
    line-height: 1;
    padding: 0.125rem 0.375rem;
    border-radius: 0.25rem;
    background-color: var(--color-honey-100, #f8e6c8);
    color: var(--color-honey-800, #7e4f1c);
    /* Honey-800 on honey-100 is 5.8:1 — AA compliant. */
  }

  /* Due time — right-aligned, muted, smaller. */
  .reminder-row__due {
    flex-shrink: 0;
    font-size: 0.75rem;
    color: var(--color-text-subtle, oklch(0.55 0 0));
    white-space: nowrap;
  }
</style>
