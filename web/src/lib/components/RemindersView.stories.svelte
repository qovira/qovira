<script module lang="ts">
  // RemindersView stories — covers the reminders view buckets, the recurring ↻ chip, and the empty state.
  //
  // These are app compositions, not library primitives. They use the real reminders store (setReminders /
  // resetReminders) seeded via beforeEach so the actual bucketing logic (bucketReminders, shouldShowPlaceholder) is
  // exercised.
  //
  // The `now` prop on RemindersView accepts a fixed Date so bucket boundaries are deterministic regardless of when the
  // story runs.

  import { defineMeta } from "@storybook/addon-svelte-csf";
  import { expect, userEvent } from "storybook/test";
  import { setReminders, resetReminders } from "$lib/stores/reminders.svelte.js";
  import type { ReminderItem } from "$lib/stores/reminders.svelte.js";
  import RemindersView from "./RemindersView.svelte";

  // Fixed reference point: Wednesday 2026-06-17 12:00 UTC.
  // Week boundaries (local = UTC for stories):
  //   today:      2026-06-17
  //   this week:  2026-06-18..2026-06-21 (Thu–Sun, Mon-based ISO week)
  //   next week:  from 2026-06-22 onward
  const NOW = new Date("2026-06-17T12:00:00Z");

  const userId = "user-story";

  // ---------------------------------------------------------------------------
  // Shared fixture factories — produce ReminderItem values relative to NOW.
  // ---------------------------------------------------------------------------

  function active(overrides: Partial<ReminderItem> & { id: string; title: string; dueAt: string }): ReminderItem {
    return {
      userId,
      status: "active",
      autoComplete: true,
      tz: "UTC",
      createdAt: NOW.toISOString(),
      updatedAt: NOW.toISOString(),
      ...overrides,
    };
  }

  function completed(overrides: Partial<ReminderItem> & { id: string; title: string; dueAt: string }): ReminderItem {
    return {
      userId,
      status: "completed",
      autoComplete: true,
      tz: "UTC",
      createdAt: NOW.toISOString(),
      updatedAt: NOW.toISOString(),
      completedAt: NOW.toISOString(),
      ...overrides,
    };
  }

  // Overdue: due before NOW.
  const OVERDUE_ITEMS: ReminderItem[] = [
    active({ id: "r1", title: "Submit expense report", dueAt: "2026-06-16T09:00:00Z" }),
    active({ id: "r2", title: "Call dentist", dueAt: "2026-06-15T14:00:00Z" }),
  ];

  // Today: due after NOW but before midnight 2026-06-18T00:00:00Z.
  const TODAY_ITEMS: ReminderItem[] = [
    active({ id: "r3", title: "Take afternoon meds", dueAt: "2026-06-17T15:00:00Z" }),
    active({
      id: "r4",
      title: "Water the plants",
      dueAt: "2026-06-17T18:00:00Z",
      rrule: "FREQ=DAILY",
    }),
  ];

  // This week: 2026-06-18..2026-06-21 (Tue–Sun remaining this week)
  const THIS_WEEK_ITEMS: ReminderItem[] = [
    active({ id: "r5", title: "Team retrospective", dueAt: "2026-06-19T10:00:00Z" }),
    active({
      id: "r6",
      title: "Review PR",
      dueAt: "2026-06-20T16:00:00Z",
      rrule: "FREQ=WEEKLY",
    }),
  ];

  // Later: from 2026-06-22 onward.
  const LATER_ITEMS: ReminderItem[] = [
    active({ id: "r7", title: "Dentist appointment", dueAt: "2026-06-23T11:00:00Z" }),
    active({
      id: "r8",
      title: "Monthly review",
      dueAt: "2026-07-01T09:00:00Z",
      rrule: "FREQ=MONTHLY",
    }),
  ];

  const { Story } = defineMeta({
    title: "Reminders/RemindersView",
    component: RemindersView,
    tags: ["autodocs"],
    args: {
      // Pass the fixed reference timestamp so bucket boundaries are deterministic.
      now: NOW,
    },
    parameters: {
      layout: "padded",
    },
  });
</script>

<!--
  EmptyState: no reminders at all → placeholder text is shown. Covers AC: "empty state".
-->
<Story
  name="EmptyState"
  beforeEach={() => {
    resetReminders();
    return () => resetReminders();
  }}
/>

<!--
  AllBuckets: active reminders across all four time buckets.
  Covers AC: "the time buckets" (Overdue, Today, This week, Later).
-->
<Story
  name="AllBuckets"
  beforeEach={() => {
    setReminders([...OVERDUE_ITEMS, ...TODAY_ITEMS, ...THIS_WEEK_ITEMS, ...LATER_ITEMS]);
    return () => resetReminders();
  }}
/>

<!--
  OverdueBucket: only overdue reminders.
-->
<Story
  name="OverdueBucket"
  beforeEach={() => {
    setReminders(OVERDUE_ITEMS);
    return () => resetReminders();
  }}
/>

<!--
  TodayBucket: reminders due today (including the recurring one with ↻ chip). Covers AC: "the recurring ↻ chip".
-->
<Story
  name="TodayBucket"
  beforeEach={() => {
    setReminders(TODAY_ITEMS);
    return () => resetReminders();
  }}
/>

<!--
  RecurringChip: a single recurring reminder in the Later bucket, to isolate the ↻ chip rendering. Covers AC:
  "the recurring ↻ chip".
-->
<Story
  name="RecurringChip"
  beforeEach={() => {
    setReminders([
      active({
        id: "r-rec",
        title: "Weekly standup",
        dueAt: "2026-06-22T09:00:00Z",
        rrule: "FREQ=WEEKLY;BYDAY=MO",
      }),
    ]);
    return () => resetReminders();
  }}
/>

<!--
  ThisWeekBucket: reminders due later this ISO week.
-->
<Story
  name="ThisWeekBucket"
  beforeEach={() => {
    setReminders(THIS_WEEK_ITEMS);
    return () => resetReminders();
  }}
/>

<!--
  LaterBucket: reminders due from next Monday onward.
-->
<Story
  name="LaterBucket"
  beforeEach={() => {
    setReminders(LATER_ITEMS);
    return () => resetReminders();
  }}
/>

<!--
  WithCompleted: mix of active (today) and completed reminders. The Done section accordion shows the completed count.
  play: exercises the accordion expand/collapse — clicking "Done" toggles aria-expanded and shows/hides the completed
  items list.
-->
<Story
  name="WithCompleted"
  beforeEach={() => {
    setReminders([
      ...TODAY_ITEMS,
      completed({ id: "r-done-1", title: "Buy oat milk", dueAt: "2026-06-15T09:00:00Z" }),
      completed({ id: "r-done-2", title: "Call Alice", dueAt: "2026-06-14T14:00:00Z" }),
    ]);
    return () => resetReminders();
  }}
  play={async ({ canvas }) => {
    // The Done toggle button is closed initially (aria-expanded=false). The button's aria-label is
    // "Show completed reminders" (from reminders_done_toggle_label).
    const doneToggle = await canvas.findByRole("button", { name: /show completed reminders/i });
    await expect(doneToggle).toHaveAttribute("aria-expanded", "false");

    // Completed items must NOT be visible when collapsed.
    await expect(canvas.queryByText("Buy oat milk")).toBeNull();

    // Click to expand.
    await userEvent.click(doneToggle);
    await expect(doneToggle).toHaveAttribute("aria-expanded", "true");

    // Completed items must now be visible.
    await expect(await canvas.findByText("Buy oat milk")).toBeVisible();
    await expect(canvas.getByText("Call Alice")).toBeVisible();

    // Click again to collapse.
    await userEvent.click(doneToggle);
    await expect(doneToggle).toHaveAttribute("aria-expanded", "false");

    // Items are hidden again.
    await expect(canvas.queryByText("Buy oat milk")).toBeNull();
  }}
/>
