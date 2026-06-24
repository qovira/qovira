<script lang="ts">
  // ReminderBucketSection — shared section scaffold for reminder buckets.
  //
  // Encapsulates the aria-labelledby section, the h2 heading, and the role="list" ul so that RemindersView
  // (Storybook) and reminders/+page.svelte (the route) share one copy of the structural a11y wiring. The `row` snippet
  // is supplied by each caller so the route can pass oncomplete/onedit callbacks while the presentational wrapper
  // omits them.

  import type { Snippet } from "svelte";
  import type { ReminderItem } from "$lib/stores/reminders.svelte.js";

  interface Props {
    /** Rendered label in the h2 heading. */
    label: string;
    /** Unique id for the h2; referenced by aria-labelledby on the section. */
    headingId: string;
    /** When true, applies the --overdue color modifier to the heading. */
    overdue?: boolean;
    /** The list of reminders in this bucket. */
    items: ReminderItem[];
    /** Snippet called once per item; receives the reminder as its argument. */
    row: Snippet<[ReminderItem]>;
  }

  const { label, headingId, overdue = false, items, row }: Props = $props();
</script>

<section class="reminders-bucket" aria-labelledby={headingId}>
  <h2 id={headingId} class="reminders-bucket__heading{overdue ? ' reminders-bucket__heading--overdue' : ''}">
    {label}
  </h2>
  <ul class="reminders-list" role="list">
    {#each items as reminder (reminder.id)}
      {@render row(reminder)}
    {/each}
  </ul>
</section>

<style>
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

  .reminders-bucket__heading--overdue {
    color: var(--color-fg-error, #a8331f);
  }

  .reminders-list {
    display: flex;
    flex-direction: column;
    list-style: none;
    padding: 0;
    margin: 0;
  }
</style>
