// Pure bucketing logic for the Reminders view.
//
// Groups a flat ReminderItem[] into five time buckets, computed against a given reference timestamp (`now`). No
// side-effects, no imports from Svelte stores — fully testable as a plain function.
//
// Bucket boundary choice (ISO-week / Monday-based):
//
//   Overdue:    dueAt < now
//   Today:      now <= dueAt < next local midnight (i.e. 00:00 tomorrow local)
//   This week:  next local midnight <= dueAt < next Monday 00:00 local
//               Rationale: use ISO calendar week (Mon = first day). "This week" covers the remaining days of the
//               *current* local calendar week after today. When now is already Mon–Sun, the upper boundary is the
//               *upcoming* Monday at 00:00 local (could be 1–6 days away). If today is Monday, "this week" covers
//               Tue–Sun of the same week. Everything from next Monday onward is "Later".
//   Later:      dueAt >= next Monday 00:00 local
//   Done:       status === "completed", sorted descending by completedAt (fallback dueAt)

import type { ReminderItem } from "$lib/stores/reminders.svelte.js";

// ---------------------------------------------------------------------------
// Return type
// ---------------------------------------------------------------------------

export interface BucketedReminders {
  /** Active reminders due before `now`. Sorted ascending by dueAt. */
  overdue: ReminderItem[];
  /** Active reminders due today (now <= dueAt < next midnight). Sorted ascending. */
  today: ReminderItem[];
  /** Active reminders due later this week (next midnight <= dueAt < next Mon). Sorted ascending. */
  thisWeek: ReminderItem[];
  /** Active reminders due from next Monday onward. Sorted ascending by dueAt. */
  later: ReminderItem[];
  /** Completed reminders. Sorted descending by completedAt, falling back to dueAt. */
  done: ReminderItem[];
}

// ---------------------------------------------------------------------------
// Boundary helpers — compute local-timezone boundaries from `now`
// ---------------------------------------------------------------------------

/**
 * Returns a Date representing 00:00:00.000 local time on the day after `now` (i.e. start of tomorrow local).
 */
function nextMidnight(now: Date): Date {
  const d = new Date(now);
  d.setHours(0, 0, 0, 0); // floor to today midnight local
  d.setDate(d.getDate() + 1); // advance to tomorrow
  return d;
}

/**
 * Returns a Date representing 00:00:00.000 local time on the next Monday (the start of the following ISO calendar
 * week).
 *
 * If `now` falls on a Monday, returns *next* Monday (7 days away) so the current Monday stays in "Today" and Tue–Sun
 * fall into "This week".
 */
function nextMonday(now: Date): Date {
  const d = new Date(now);
  d.setHours(0, 0, 0, 0);
  const dayOfWeek = d.getDay(); // 0=Sun, 1=Mon, …, 6=Sat
  // Days until next Monday: Mon=7, Tue=6, Wed=5, Thu=4, Fri=3, Sat=2, Sun=1
  const daysUntilMonday = dayOfWeek === 1 ? 7 : (8 - dayOfWeek) % 7 || 7;
  d.setDate(d.getDate() + daysUntilMonday);
  return d;
}

// ---------------------------------------------------------------------------
// Comparators
// ---------------------------------------------------------------------------

function ascByDueAt(a: ReminderItem, b: ReminderItem): number {
  return new Date(a.dueAt).getTime() - new Date(b.dueAt).getTime();
}

function descByCompletedAt(a: ReminderItem, b: ReminderItem): number {
  // Fall back to dueAt when completedAt is absent (one-shot that skipped autoComplete).
  const aTs = a.completedAt !== undefined ? new Date(a.completedAt).getTime() : new Date(a.dueAt).getTime();
  const bTs = b.completedAt !== undefined ? new Date(b.completedAt).getTime() : new Date(b.dueAt).getTime();
  return bTs - aTs; // descending
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

/**
 * Returns true only when the reminders page empty-state placeholder should be shown — i.e. there are absolutely no
 * reminders (no active ones and no completed ones). When there are zero active reminders but at least one completed
 * reminder, the Done section will still show content, so the placeholder must be suppressed.
 *
 * @param hasNoActive - True when all four active buckets (overdue/today/this week/later) are empty.
 * @param doneCount   - The number of completed reminders currently in the Done bucket.
 */
export function shouldShowPlaceholder(hasNoActive: boolean, doneCount: number): boolean {
  return hasNoActive && doneCount === 0;
}

/**
 * Bucket a flat list of reminders into Overdue / Today / This week / Later / Done.
 *
 * @param reminders - The full reminder list (mixed active + completed).
 * @param now       - The reference timestamp. Use `new Date()` in the browser; pass a fixed Date in tests for
 *                    determinism.
 * @returns A BucketedReminders object with sorted, non-overlapping slices.
 */
export function bucketReminders(reminders: ReminderItem[], now: Date): BucketedReminders {
  const midnight = nextMidnight(now);
  const monday = nextMonday(now);
  const nowMs = now.getTime();
  const midnightMs = midnight.getTime();
  const mondayMs = monday.getTime();

  const overdue: ReminderItem[] = [];
  const today: ReminderItem[] = [];
  const thisWeek: ReminderItem[] = [];
  const later: ReminderItem[] = [];
  const done: ReminderItem[] = [];

  for (const r of reminders) {
    if (r.status === "completed") {
      done.push(r);
      continue;
    }

    // Active reminder — bucket by dueAt vs time boundaries.
    const dueMs = new Date(r.dueAt).getTime();

    if (dueMs < nowMs) {
      overdue.push(r);
    } else if (dueMs < midnightMs) {
      today.push(r);
    } else if (dueMs < mondayMs) {
      thisWeek.push(r);
    } else {
      later.push(r);
    }
  }

  overdue.sort(ascByDueAt);
  today.sort(ascByDueAt);
  thisWeek.sort(ascByDueAt);
  later.sort(ascByDueAt);
  done.sort(descByCompletedAt);

  return { overdue, today, thisWeek, later, done };
}
