<script lang="ts">
  // RemindersView — thin presentational wrapper around the bucketed reminders list.
  //
  // Reads from the real reminders store (getReminders) and bucket logic
  // (bucketReminders, shouldShowPlaceholder) so stories exercise the actual rendering
  // paths used in +page.svelte — not parallel mocks. Uses the shared ReminderRow
  // component so the row markup is identical to the route.
  //
  // Omits the Quick-add form and Edit slide-over because those are interactions
  // covered by separate tests. Callback props (oncomplete, onedit) are absent so
  // story interaction with the check-circle or row body is a no-op.
  //
  // "now" is injectable so stories can demonstrate buckets without depending on
  // the wall clock.

  import { getReminders } from "$lib/stores/reminders.svelte.js";
  import { bucketReminders, shouldShowPlaceholder } from "$lib/reminders/bucket.js";
  import {
    reminders_placeholder,
    reminders_bucket_overdue,
    reminders_bucket_today,
    reminders_bucket_this_week,
    reminders_bucket_later,
    reminders_bucket_done,
    reminders_done_toggle_label,
    reminders_done_empty,
  } from "$lib/paraglide/messages.js";
  import ReminderRow from "$lib/components/ReminderRow.svelte";
  import ReminderBucketSection from "$lib/components/ReminderBucketSection.svelte";

  interface Props {
    /**
     * Reference timestamp for bucket boundaries. Defaults to now.
     * Pass a fixed Date in stories for deterministic bucket assignment.
     */
    now?: Date;
  }

  const { now = new Date() }: Props = $props();

  const buckets = $derived(bucketReminders(getReminders(), now));

  const overdue = $derived(buckets.overdue);
  const today = $derived(buckets.today);
  const thisWeek = $derived(buckets.thisWeek);
  const later = $derived(buckets.later);
  const done = $derived(buckets.done);

  const hasNoActive = $derived(
    overdue.length === 0 && today.length === 0 && thisWeek.length === 0 && later.length === 0,
  );

  const showPlaceholder = $derived(shouldShowPlaceholder(hasNoActive, done.length));

  // Done section — static open state for story purposes.
  let doneOpen = $state(false);

  function toggleDone(): void {
    doneOpen = !doneOpen;
  }
</script>

<div class="reminders-page" style="max-width: 480px;">
  {#if showPlaceholder}
    <p class="text-fg-muted text-sm">{reminders_placeholder()}</p>
  {:else if hasNoActive}
    <!-- Zero active but some done — no active buckets; Done section below shows them. -->
  {:else}
    <!-- Overdue -->
    {#if overdue.length > 0}
      <ReminderBucketSection
        label={reminders_bucket_overdue()}
        headingId="bucket-overdue"
        overdue={true}
        items={overdue}
      >
        {#snippet row(reminder)}
          <ReminderRow {reminder} />
        {/snippet}
      </ReminderBucketSection>
    {/if}

    <!-- Today -->
    {#if today.length > 0}
      <ReminderBucketSection label={reminders_bucket_today()} headingId="bucket-today" items={today}>
        {#snippet row(reminder)}
          <ReminderRow {reminder} />
        {/snippet}
      </ReminderBucketSection>
    {/if}

    <!-- This week -->
    {#if thisWeek.length > 0}
      <ReminderBucketSection label={reminders_bucket_this_week()} headingId="bucket-this-week" items={thisWeek}>
        {#snippet row(reminder)}
          <ReminderRow {reminder} />
        {/snippet}
      </ReminderBucketSection>
    {/if}

    <!-- Later -->
    {#if later.length > 0}
      <ReminderBucketSection label={reminders_bucket_later()} headingId="bucket-later" items={later}>
        {#snippet row(reminder)}
          <ReminderRow {reminder} />
        {/snippet}
      </ReminderBucketSection>
    {/if}
  {/if}

  <!-- Done section -->
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
      {#if done.length === 0}
        <p class="text-fg-muted text-sm">{reminders_done_empty()}</p>
      {:else}
        <ul class="reminders-list" role="list">
          {#each done as reminder (reminder.id)}
            <ReminderRow {reminder} isDone={true} />
          {/each}
        </ul>
      {/if}
    {/if}
  </section>
</div>

<style>
  .reminders-page {
    display: flex;
    flex-direction: column;
    gap: 1.5rem;
  }

  /* .reminders-bucket and .reminders-bucket__heading are also defined in
     ReminderBucketSection.svelte for the active-bucket sections. They are kept
     here too because the Done section's <section class="reminders-bucket"> and
     the .reminders-done-toggle .reminders-bucket__heading compound selector are
     inline in this component and rely on these scoped styles. */
  .reminders-bucket {
    display: flex;
    flex-direction: column;
    gap: 0.375rem;
  }

  .reminders-bucket__heading {
    display: flex;
    align-items: center;
    gap: 0.375rem;
    font-size: 0.6875rem;
    font-weight: 600;
    letter-spacing: 0.06em;
    text-transform: uppercase;
    color: var(--color-fg-muted, #5c4a37);
    padding-bottom: 0.25rem;
    border-bottom: 1px solid var(--color-border, oklch(0.9 0 0));
    margin-bottom: 0.125rem;
    pointer-events: none;
  }

  .reminders-done-count {
    color: var(--color-fg-muted, #5c4a37);
    font-weight: 400;
  }

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

  .reminders-done-chevron {
    color: var(--color-fg-muted, #5c4a37);
    flex-shrink: 0;
    transition: transform 150ms ease;
  }

  .reminders-done-chevron--open {
    transform: rotate(180deg);
  }

  .reminders-list {
    display: flex;
    flex-direction: column;
    list-style: none;
    padding: 0;
    margin: 0;
  }
</style>
