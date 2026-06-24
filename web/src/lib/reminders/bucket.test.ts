// Tests for the pure reminder-bucketing logic.
//
// Plain unit test (vitest project "browser", happy-dom env) — the file uses no
// runes, no fetch, no Svelte rendering; it purely tests bucketReminders().
//
// Bucket boundary choice (documented here and in bucket.ts):
//   - Overdue:    dueAt < now
//   - Today:      now <= dueAt < next local midnight (start of tomorrow)
//   - This week:  next local midnight <= dueAt < start of next local calendar week (Mon 00:00)
//                 "Start of week" = ISO week (Mon=1). Within the current week the remaining
//                 days (today exclusive) fall in "This week"; everything >= next Monday is "Later".
//   - Later:      dueAt >= start of next Monday local
//
// Test strategy: use a fixed `now` but compute boundary timestamps using the
// same local-time methods the implementation uses, so tests are portable across
// any host timezone. We do NOT hard-code UTC strings for boundary tests.

import { describe, expect, it } from "vitest";
import { bucketReminders, shouldShowPlaceholder, type BucketedReminders } from "./bucket.js";
import type { ReminderItem } from "$lib/stores/reminders.svelte.js";

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/** Build a minimal ReminderItem with sensible defaults. */
function makeReminder(overrides: Partial<ReminderItem> = {}): ReminderItem {
  return {
    id: "r1",
    userId: "u1",
    title: "Test reminder",
    dueAt: new Date(Date.now()).toISOString(), // overridden below
    tz: "UTC",
    autoComplete: true,
    status: "active",
    createdAt: "2025-01-01T00:00:00Z",
    updatedAt: "2025-01-01T00:00:00Z",
    ...overrides,
  };
}

// ---------------------------------------------------------------------------
// Fixed reference point: a Wednesday at 10:00 AM local.
// We construct it using local-time methods so the value is always on Wed 10:00
// in whatever timezone the test host uses.
// ---------------------------------------------------------------------------

function makeNow(): Date {
  // Find the nearest Wednesday at 10:00 local.
  // We create a date that is definitely a Wednesday in local time.
  // Strategy: start with a known Wednesday in UTC (2030-06-12 is Wed),
  // then use local date methods to pin it to 10:00 local.
  const d = new Date(2030, 5, 12, 10, 0, 0, 0); // 2030-06-12 10:00:00 local
  return d;
}

const NOW = makeNow();

/** Compute next local midnight from now (same as implementation). */
function makeNextMidnight(now: Date): Date {
  const d = new Date(now);
  d.setHours(0, 0, 0, 0);
  d.setDate(d.getDate() + 1);
  return d;
}

/** Compute next Monday 00:00 local (same logic as implementation). */
function makeNextMonday(now: Date): Date {
  const d = new Date(now);
  d.setHours(0, 0, 0, 0);
  const dayOfWeek = d.getDay(); // 0=Sun … 6=Sat
  const daysUntilMonday = dayOfWeek === 1 ? 7 : (8 - dayOfWeek) % 7 || 7;
  d.setDate(d.getDate() + daysUntilMonday);
  return d;
}

const MIDNIGHT = makeNextMidnight(NOW);
const NEXT_MONDAY = makeNextMonday(NOW);

/** Offset `base` date by `ms` milliseconds. */
function offsetMs(base: Date, ms: number): Date {
  return new Date(base.getTime() + ms);
}

const ONE_SEC_MS = 1_000;
const ONE_MIN_MS = 60_000;
const ONE_HOUR_MS = 3_600_000;

// Convenience: run bucketing with the fixed NOW.
function bucket(reminders: ReminderItem[]): BucketedReminders {
  return bucketReminders(reminders, NOW);
}

// ---------------------------------------------------------------------------
// Empty input
// ---------------------------------------------------------------------------

describe("bucketReminders — empty input", () => {
  it("returns all empty buckets when the list is empty", () => {
    const result = bucket([]);
    expect(result.overdue).toEqual([]);
    expect(result.today).toEqual([]);
    expect(result.thisWeek).toEqual([]);
    expect(result.later).toEqual([]);
    expect(result.done).toEqual([]);
  });
});

// ---------------------------------------------------------------------------
// Active/completed split
// ---------------------------------------------------------------------------

describe("bucketReminders — active / completed split", () => {
  it("routes completed reminders to done, not to active buckets", () => {
    const overdueTs = offsetMs(NOW, -ONE_HOUR_MS).toISOString(); // 1h before now
    const active = makeReminder({ id: "a", status: "active", dueAt: overdueTs });
    const done = makeReminder({
      id: "d",
      status: "completed",
      dueAt: overdueTs,
      completedAt: offsetMs(NOW, -30 * ONE_MIN_MS).toISOString(),
    });
    const result = bucket([active, done]);
    expect(result.overdue).toHaveLength(1);
    expect(result.overdue[0]?.id).toBe("a");
    expect(result.done).toHaveLength(1);
    expect(result.done[0]?.id).toBe("d");
  });

  it("completed reminders never appear in active buckets", () => {
    const items = [
      makeReminder({
        id: "c1",
        status: "completed",
        dueAt: offsetMs(NOW, -ONE_HOUR_MS).toISOString(),
      }),
      makeReminder({
        id: "c2",
        status: "completed",
        dueAt: offsetMs(NOW, ONE_HOUR_MS).toISOString(),
      }),
    ];
    const result = bucket(items);
    expect(result.overdue).toHaveLength(0);
    expect(result.today).toHaveLength(0);
    expect(result.thisWeek).toHaveLength(0);
    expect(result.later).toHaveLength(0);
    expect(result.done).toHaveLength(2);
  });
});

// ---------------------------------------------------------------------------
// Overdue bucket
// ---------------------------------------------------------------------------

describe("bucketReminders — overdue bucket", () => {
  it("places a reminder due 1 second before now in overdue", () => {
    const r = makeReminder({ dueAt: offsetMs(NOW, -ONE_SEC_MS).toISOString() });
    expect(bucket([r]).overdue).toHaveLength(1);
  });

  it("places a reminder due 1 hour before now in overdue", () => {
    const r = makeReminder({ dueAt: offsetMs(NOW, -ONE_HOUR_MS).toISOString() });
    expect(bucket([r]).overdue).toHaveLength(1);
  });

  it("does NOT place a reminder due exactly at now in overdue (boundary: now <= dueAt → today)", () => {
    const r = makeReminder({ dueAt: NOW.toISOString() });
    const result = bucket([r]);
    expect(result.overdue).toHaveLength(0);
    expect(result.today).toHaveLength(1);
  });
});

// ---------------------------------------------------------------------------
// Today bucket
// ---------------------------------------------------------------------------

describe("bucketReminders — today bucket", () => {
  it("places a reminder due exactly at now in today", () => {
    const r = makeReminder({ dueAt: NOW.toISOString() }); // exactly now
    expect(bucket([r]).today).toHaveLength(1);
  });

  it("places a reminder due 1 minute before local midnight in today", () => {
    // 1 minute before midnight = still today
    const r = makeReminder({ dueAt: offsetMs(MIDNIGHT, -ONE_MIN_MS).toISOString() });
    expect(bucket([r]).today).toHaveLength(1);
  });

  it("does NOT place a reminder at exactly local midnight in today (start of tomorrow)", () => {
    const r = makeReminder({ dueAt: MIDNIGHT.toISOString() }); // exactly midnight = next day
    const result = bucket([r]);
    expect(result.today).toHaveLength(0);
    // Falls into thisWeek or later depending on day
    const total = result.thisWeek.length + result.later.length;
    expect(total).toBe(1);
  });
});

// ---------------------------------------------------------------------------
// This week bucket
// ---------------------------------------------------------------------------
// now = Wed 2030-06-12 10:00 local.
// "This week" = next midnight (Thu 00:00) through next Monday 00:00 (exclusive).
// Thu, Fri, Sat, Sun land here. Mon = Later.

describe("bucketReminders — this week bucket", () => {
  it("places a reminder at exactly local midnight in this week (start of tomorrow)", () => {
    // Midnight = start of Thu = this week
    const r = makeReminder({ dueAt: MIDNIGHT.toISOString() });
    expect(bucket([r]).thisWeek).toHaveLength(1);
  });

  it("places a reminder 1 minute before next Monday in this week", () => {
    const r = makeReminder({ dueAt: offsetMs(NEXT_MONDAY, -ONE_MIN_MS).toISOString() });
    expect(bucket([r]).thisWeek).toHaveLength(1);
  });

  it("does NOT place a reminder at exactly next Monday in this week (boundary: Later)", () => {
    const r = makeReminder({ dueAt: NEXT_MONDAY.toISOString() });
    const result = bucket([r]);
    expect(result.thisWeek).toHaveLength(0);
    expect(result.later).toHaveLength(1);
  });
});

// ---------------------------------------------------------------------------
// Later bucket
// ---------------------------------------------------------------------------

describe("bucketReminders — later bucket", () => {
  it("places a reminder at next Monday in later", () => {
    const r = makeReminder({ dueAt: NEXT_MONDAY.toISOString() });
    expect(bucket([r]).later).toHaveLength(1);
  });

  it("places a reminder 1 day after next Monday in later", () => {
    const r = makeReminder({ dueAt: offsetMs(NEXT_MONDAY, 24 * ONE_HOUR_MS).toISOString() });
    expect(bucket([r]).later).toHaveLength(1);
  });

  it("places a reminder far in the future in later", () => {
    const r = makeReminder({ dueAt: "2031-01-01T00:00:00Z" });
    expect(bucket([r]).later).toHaveLength(1);
  });
});

// ---------------------------------------------------------------------------
// Sort order within buckets
// ---------------------------------------------------------------------------

describe("bucketReminders — sort order", () => {
  it("sorts overdue ascending by dueAt (earliest first)", () => {
    const items = [
      makeReminder({ id: "r2", dueAt: offsetMs(NOW, -2 * ONE_HOUR_MS).toISOString() }),
      makeReminder({ id: "r1", dueAt: offsetMs(NOW, -3 * ONE_HOUR_MS).toISOString() }),
      makeReminder({ id: "r3", dueAt: offsetMs(NOW, -ONE_HOUR_MS).toISOString() }),
    ];
    const result = bucket(items);
    expect(result.overdue.map((r) => r.id)).toEqual(["r1", "r2", "r3"]);
  });

  it("sorts today ascending by dueAt", () => {
    const items = [
      makeReminder({ id: "b", dueAt: offsetMs(NOW, 2 * ONE_HOUR_MS).toISOString() }),
      makeReminder({ id: "a", dueAt: NOW.toISOString() }),
    ];
    expect(bucket(items).today.map((r) => r.id)).toEqual(["a", "b"]);
  });

  it("sorts this-week ascending by dueAt", () => {
    const items = [
      makeReminder({ id: "b", dueAt: offsetMs(MIDNIGHT, 2 * ONE_HOUR_MS).toISOString() }),
      makeReminder({ id: "a", dueAt: MIDNIGHT.toISOString() }),
    ];
    expect(bucket(items).thisWeek.map((r) => r.id)).toEqual(["a", "b"]);
  });

  it("sorts done descending by completedAt (most-recently-done first)", () => {
    const base = offsetMs(NOW, -48 * ONE_HOUR_MS);
    const items = [
      makeReminder({
        id: "older",
        status: "completed",
        dueAt: offsetMs(base, -ONE_HOUR_MS).toISOString(),
        completedAt: base.toISOString(),
      }),
      makeReminder({
        id: "newer",
        status: "completed",
        dueAt: base.toISOString(),
        completedAt: offsetMs(base, ONE_HOUR_MS).toISOString(),
      }),
    ];
    expect(bucket(items).done.map((r) => r.id)).toEqual(["newer", "older"]);
  });

  it("falls back to dueAt for done sort when completedAt is absent", () => {
    const items = [
      makeReminder({
        id: "a",
        status: "completed",
        dueAt: offsetMs(NOW, -3 * ONE_HOUR_MS).toISOString(),
      }),
      makeReminder({
        id: "b",
        status: "completed",
        dueAt: offsetMs(NOW, -ONE_HOUR_MS).toISOString(),
      }),
    ];
    // descending by dueAt fallback
    expect(bucket(items).done.map((r) => r.id)).toEqual(["b", "a"]);
  });
});

// ---------------------------------------------------------------------------
// Recurring detection (rrule field presence)
// ---------------------------------------------------------------------------

describe("bucketReminders — recurring detection", () => {
  it("reminder with rrule is present in a bucket (it lands normally)", () => {
    const r = makeReminder({ dueAt: offsetMs(NOW, ONE_HOUR_MS).toISOString(), rrule: "FREQ=DAILY;INTERVAL=1" });
    const result = bucket([r]);
    expect(result.today).toHaveLength(1);
    expect(result.today[0]?.rrule).toBe("FREQ=DAILY;INTERVAL=1");
  });

  it("reminder without rrule has undefined rrule field", () => {
    const r = makeReminder({ dueAt: offsetMs(NOW, ONE_HOUR_MS).toISOString() });
    const result = bucket([r]);
    expect(result.today[0]?.rrule).toBeUndefined();
  });
});

// ---------------------------------------------------------------------------
// Mixed scenario
// ---------------------------------------------------------------------------

describe("bucketReminders — mixed scenario", () => {
  it("correctly distributes reminders across all five buckets", () => {
    const items = [
      makeReminder({ id: "overdue", status: "active", dueAt: offsetMs(NOW, -ONE_HOUR_MS).toISOString() }),
      makeReminder({ id: "today", status: "active", dueAt: offsetMs(NOW, ONE_HOUR_MS).toISOString() }),
      makeReminder({ id: "week", status: "active", dueAt: offsetMs(MIDNIGHT, ONE_HOUR_MS).toISOString() }),
      makeReminder({ id: "later", status: "active", dueAt: offsetMs(NEXT_MONDAY, ONE_HOUR_MS).toISOString() }),
      makeReminder({
        id: "done",
        status: "completed",
        dueAt: offsetMs(NOW, -2 * ONE_HOUR_MS).toISOString(),
        completedAt: offsetMs(NOW, -ONE_HOUR_MS).toISOString(),
      }),
    ];
    const result = bucket(items);
    expect(result.overdue.map((r) => r.id)).toEqual(["overdue"]);
    expect(result.today.map((r) => r.id)).toEqual(["today"]);
    expect(result.thisWeek.map((r) => r.id)).toEqual(["week"]);
    expect(result.later.map((r) => r.id)).toEqual(["later"]);
    expect(result.done.map((r) => r.id)).toEqual(["done"]);
  });
});

// ---------------------------------------------------------------------------
// shouldShowPlaceholder (Fix 2)
// ---------------------------------------------------------------------------
// The placeholder ("Nothing on your list yet.") must appear only when there
// are truly NO reminders at all — no active ones AND no done ones. When there
// are zero active reminders but at least one completed one, we suppress the
// placeholder so "Nothing on your list yet." does not sit above a non-empty
// Done section.

describe("shouldShowPlaceholder", () => {
  it("returns true when there are no active reminders and no done reminders", () => {
    expect(shouldShowPlaceholder(true, 0)).toBe(true);
  });

  it("returns false when there are no active reminders but some done reminders", () => {
    // Zero active + ≥1 done → placeholder must NOT show.
    expect(shouldShowPlaceholder(true, 1)).toBe(false);
    expect(shouldShowPlaceholder(true, 5)).toBe(false);
  });

  it("returns false when there are active reminders (regardless of done count)", () => {
    expect(shouldShowPlaceholder(false, 0)).toBe(false);
    expect(shouldShowPlaceholder(false, 3)).toBe(false);
  });
});
