// Tests for reminder optimistic-update actions (quick-add, complete, edit).
//
// Rune environment: node + Svelte compiler (vitest project "runes").
// Tests the apply/confirm/revert steps of each optimistic flow against the real reminders store, with a mocked Api.
// This is the highest-value test target per the issue: the reconcile logic must be deterministic and proven correct.
//
// Pattern for each flow:
//   1. Setup initial store state.
//   2. Watch them fail first (TDD) — then implement to green.
//   3. Apply the optimistic mutation, verify store reflects it.
//   4. Confirm (success path): verify temp removed + real inserted, no duplicate.
//   5. Revert (failure path): verify store back to prior state.

import { flushSync } from "svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import { getReminders, upsertReminder, resetReminders, type ReminderItem } from "$lib/stores/reminders.svelte.js";
import {
  applyOptimisticCreate,
  confirmCreate,
  revertCreate,
  applyOptimisticComplete,
  confirmComplete,
  revertComplete,
  buildNextDueAt,
  defaultDueAtLocal,
  dueAtToLocal,
  rruleToPreset,
  makeReminderPatchBody,
} from "./actions.svelte.js";

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function makeReminder(overrides: Partial<ReminderItem> = {}): ReminderItem {
  return {
    id: "r1",
    userId: "u1",
    title: "Test reminder",
    dueAt: "2030-01-01T12:00:00Z",
    tz: "UTC",
    autoComplete: true,
    status: "active",
    createdAt: "2025-01-01T00:00:00Z",
    updatedAt: "2025-01-01T00:00:00Z",
    ...overrides,
  };
}

// ---------------------------------------------------------------------------
// Reset between tests — the store singleton bleeds across suites.
// ---------------------------------------------------------------------------

afterEach(() => {
  resetReminders();
  flushSync();
  vi.restoreAllMocks();
});

// ---------------------------------------------------------------------------
// Quick-add: applyOptimisticCreate / confirmCreate / revertCreate
// ---------------------------------------------------------------------------

describe("quick-add: applyOptimisticCreate", () => {
  it("inserts the temp reminder into the store", () => {
    const temp = makeReminder({ id: "temp-01", title: "Buy milk" });
    applyOptimisticCreate(temp);
    flushSync();

    expect(getReminders()).toHaveLength(1);
    expect(getReminders()[0]?.id).toBe("temp-01");
    expect(getReminders()[0]?.title).toBe("Buy milk");
  });

  it("stores with status=active so it lands in an active bucket", () => {
    const temp = makeReminder({ id: "temp-02", status: "active" });
    applyOptimisticCreate(temp);
    flushSync();

    expect(getReminders()[0]?.status).toBe("active");
  });
});

describe("quick-add: confirmCreate (success path)", () => {
  it("removes the temp reminder and upserts the real one — no duplicate", () => {
    const temp = makeReminder({ id: "temp-03", title: "Walk dog" });
    applyOptimisticCreate(temp);
    flushSync();

    const real = makeReminder({ id: "real-server-id-03", title: "Walk dog" });
    confirmCreate("temp-03", real);
    flushSync();

    const list = getReminders();
    expect(list).toHaveLength(1);
    expect(list[0]?.id).toBe("real-server-id-03");
    expect(list.find((r) => r.id === "temp-03")).toBeUndefined();
  });

  it("leaves other reminders untouched", () => {
    const other = makeReminder({ id: "other-r" });
    upsertReminder(other);

    const temp = makeReminder({ id: "temp-04", title: "New one" });
    applyOptimisticCreate(temp);
    flushSync();

    const real = makeReminder({ id: "real-04", title: "New one" });
    confirmCreate("temp-04", real);
    flushSync();

    const list = getReminders();
    expect(list).toHaveLength(2);
    expect(list.find((r) => r.id === "other-r")).toBeDefined();
    expect(list.find((r) => r.id === "real-04")).toBeDefined();
  });
});

describe("quick-add: revertCreate (failure / rejection path — AC5)", () => {
  it("removes the temp reminder from the store", () => {
    const temp = makeReminder({ id: "temp-05", title: "Buy coffee" });
    applyOptimisticCreate(temp);
    flushSync();

    expect(getReminders()).toHaveLength(1);

    revertCreate("temp-05");
    flushSync();

    expect(getReminders()).toHaveLength(0);
  });

  it("leaves other reminders in the store when reverting", () => {
    const other = makeReminder({ id: "keep-me" });
    upsertReminder(other);

    const temp = makeReminder({ id: "temp-06" });
    applyOptimisticCreate(temp);
    flushSync();

    revertCreate("temp-06");
    flushSync();

    const list = getReminders();
    expect(list).toHaveLength(1);
    expect(list[0]?.id).toBe("keep-me");
  });
});

// ---------------------------------------------------------------------------
// Complete: applyOptimisticComplete / confirmComplete / revertComplete
// ---------------------------------------------------------------------------

describe("complete: applyOptimisticComplete", () => {
  it("marks the reminder as completed in the store", () => {
    const r = makeReminder({ id: "rc-01", status: "active" });
    upsertReminder(r);
    flushSync();

    applyOptimisticComplete("rc-01");
    flushSync();

    const updated = getReminders().find((rem) => rem.id === "rc-01");
    expect(updated?.status).toBe("completed");
    expect(updated?.completedAt).toBeDefined();
  });

  it("returns the snapshot of the prior state for revert", () => {
    const r = makeReminder({ id: "rc-02", status: "active", title: "Original" });
    upsertReminder(r);
    flushSync();

    const snapshot = applyOptimisticComplete("rc-02");
    expect(snapshot).not.toBeNull();
    expect(snapshot?.status).toBe("active");
    expect(snapshot?.title).toBe("Original");
    expect(snapshot?.id).toBe("rc-02");
  });

  it("returns null when the reminder does not exist", () => {
    const snapshot = applyOptimisticComplete("nonexistent");
    expect(snapshot).toBeNull();
  });
});

describe("complete: confirmComplete (success path)", () => {
  it("replaces the optimistic reminder with the server's authoritative copy", () => {
    const r = makeReminder({ id: "rc-03", status: "active" });
    upsertReminder(r);
    flushSync();

    applyOptimisticComplete("rc-03");
    flushSync();

    const serverVersion = makeReminder({
      id: "rc-03",
      status: "completed",
      completedAt: "2030-06-01T10:00:00Z",
    });
    confirmComplete(serverVersion);
    flushSync();

    const stored = getReminders().find((rem) => rem.id === "rc-03");
    expect(stored?.completedAt).toBe("2030-06-01T10:00:00Z");
  });
});

describe("complete: revertComplete (failure / rejection path — AC5)", () => {
  it("restores the exact prior snapshot when the PATCH is rejected", () => {
    const r = makeReminder({ id: "rc-04", status: "active", title: "Must stay active" });
    upsertReminder(r);
    flushSync();

    const snapshot = applyOptimisticComplete("rc-04");
    flushSync();

    // Verify it moved to completed first
    expect(getReminders().find((rem) => rem.id === "rc-04")?.status).toBe("completed");

    // Now revert with the snapshot
    if (snapshot !== null) {
      revertComplete(snapshot);
    }
    flushSync();

    const restored = getReminders().find((rem) => rem.id === "rc-04");
    expect(restored?.status).toBe("active");
    expect(restored?.title).toBe("Must stay active");
    expect(restored?.completedAt).toBeUndefined();
  });
});

// ---------------------------------------------------------------------------
// buildNextDueAt — converts datetime-local value to RFC3339 UTC
// ---------------------------------------------------------------------------

describe("buildNextDueAt", () => {
  it("converts a datetime-local string to a UTC RFC3339 ISO string", () => {
    // "2030-06-15T09:00" is local time; it should become a valid ISO date
    const result = buildNextDueAt("2030-06-15T09:00");
    // Must be a valid Date string
    const d = new Date(result);
    expect(Number.isNaN(d.getTime())).toBe(false);
    // Must end with Z (UTC)
    expect(result.endsWith("Z")).toBe(true);
  });

  it("returns a past-ISO string for an empty input (fallback)", () => {
    // An empty string cannot produce a valid date — we return an empty string which callers treat as "no dueAt
    // provided" (caller validates before submit)
    const result = buildNextDueAt("");
    expect(result).toBe("");
  });
});

// ---------------------------------------------------------------------------
// defaultDueAtLocal — "YYYY-MM-DDTHH:MM" local string at top of next hour
// ---------------------------------------------------------------------------

describe("defaultDueAtLocal", () => {
  it("returns a string matching the datetime-local format YYYY-MM-DDTHH:MM", () => {
    const result = defaultDueAtLocal();
    expect(result).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/);
  });

  it("returns a value with minutes == '00' (top of the hour)", () => {
    const result = defaultDueAtLocal();
    // The minutes component is after the final colon.
    const minutes = result.slice(-2);
    expect(minutes).toBe("00");
  });
});

// ---------------------------------------------------------------------------
// dueAtToLocal — UTC RFC3339 → datetime-local local string
// ---------------------------------------------------------------------------

describe("dueAtToLocal", () => {
  it("returns an empty string for empty input", () => {
    expect(dueAtToLocal("")).toBe("");
  });

  it("returns an empty string for an unparseable string", () => {
    expect(dueAtToLocal("not-a-date")).toBe("");
    expect(dueAtToLocal("garbage!!")).toBe("");
  });

  it("round-trips with buildNextDueAt: dueAtToLocal(buildNextDueAt(x)) === x", () => {
    // A well-formed datetime-local value with zero seconds/millis (so no information is lost in the round-trip — the
    // input is already at minute precision, which is what buildNextDueAt → toISOString → dueAtToLocal preserves when
    // reconstructed in local time).
    const input = "2030-06-15T09:00";
    const utc = buildNextDueAt(input);
    expect(utc).not.toBe("");
    const local = dueAtToLocal(utc);
    expect(local).toBe(input);
  });

  it("round-trips a non-zero minute value", () => {
    const input = "2030-11-20T14:30";
    const utc = buildNextDueAt(input);
    expect(utc).not.toBe("");
    expect(dueAtToLocal(utc)).toBe(input);
  });
});

// ---------------------------------------------------------------------------
// rruleToPreset — maps rrule string to RrulePreset
// ---------------------------------------------------------------------------

describe("rruleToPreset", () => {
  it("returns 'none' for undefined (no rrule set)", () => {
    expect(rruleToPreset(undefined)).toBe("none");
  });

  it("returns 'daily' for FREQ=DAILY", () => {
    expect(rruleToPreset("FREQ=DAILY")).toBe("daily");
  });

  it("returns 'weekly' for FREQ=WEEKLY", () => {
    expect(rruleToPreset("FREQ=WEEKLY")).toBe("weekly");
  });

  it("returns 'monthly' for FREQ=MONTHLY", () => {
    expect(rruleToPreset("FREQ=MONTHLY")).toBe("monthly");
  });

  it("returns 'keep' for an unknown / custom rrule (FREQ=WEEKLY;COUNT=3)", () => {
    expect(rruleToPreset("FREQ=WEEKLY;COUNT=3")).toBe("keep");
  });

  it("returns 'keep' for any other unrecognised rrule string", () => {
    expect(rruleToPreset("FREQ=YEARLY")).toBe("keep");
  });
});

// ---------------------------------------------------------------------------
// makeReminderPatchBody — diff builder: only changed fields, null to clear
// ---------------------------------------------------------------------------

describe("makeReminderPatchBody", () => {
  it("returns only changed fields", () => {
    const original = makeReminder({
      id: "p-01",
      title: "Original",
      dueAt: "2030-01-01T12:00:00Z",
      notes: "some note",
    });
    const body = makeReminderPatchBody(original, {
      title: "Updated",
      dueAt: "2030-01-01T12:00:00Z", // unchanged
      notes: "some note", // unchanged
      rrulePreset: "none",
    });
    // Only title changed — dueAt and notes are omitted
    expect(body).toHaveProperty("title", "Updated");
    expect(body).not.toHaveProperty("dueAt");
    expect(body).not.toHaveProperty("notes");
  });

  it("includes dueAt when it changed", () => {
    const original = makeReminder({ id: "p-02", dueAt: "2030-01-01T12:00:00Z" });
    const body = makeReminderPatchBody(original, {
      title: original.title,
      dueAt: "2030-02-01T12:00:00Z",
      rrulePreset: "none",
    });
    expect(body).toHaveProperty("dueAt", "2030-02-01T12:00:00Z");
  });

  it("sends rrule: null when preset is 'none' and original had an rrule", () => {
    const original = makeReminder({ id: "p-03", rrule: "FREQ=DAILY" });
    const body = makeReminderPatchBody(original, {
      title: original.title,
      dueAt: original.dueAt,
      rrulePreset: "none",
    });
    expect(body).toHaveProperty("rrule", null);
  });

  it("sends rrule string when preset changes to daily", () => {
    const original = makeReminder({ id: "p-04" });
    const body = makeReminderPatchBody(original, {
      title: original.title,
      dueAt: original.dueAt,
      rrulePreset: "daily",
    });
    expect(body).toHaveProperty("rrule", "FREQ=DAILY");
  });

  it("sends rrule string when preset changes to weekly", () => {
    const original = makeReminder({ id: "p-05" });
    const body = makeReminderPatchBody(original, {
      title: original.title,
      dueAt: original.dueAt,
      rrulePreset: "weekly",
    });
    expect(body).toHaveProperty("rrule", "FREQ=WEEKLY");
  });

  it("sends rrule string when preset changes to monthly", () => {
    const original = makeReminder({ id: "p-06" });
    const body = makeReminderPatchBody(original, {
      title: original.title,
      dueAt: original.dueAt,
      rrulePreset: "monthly",
    });
    expect(body).toHaveProperty("rrule", "FREQ=MONTHLY");
  });

  it("omits rrule when no change (none→none)", () => {
    const original = makeReminder({ id: "p-07" }); // no rrule
    const body = makeReminderPatchBody(original, {
      title: original.title,
      dueAt: original.dueAt,
      rrulePreset: "none",
    });
    // rrule was absent and stays absent — omit from patch
    expect(body).not.toHaveProperty("rrule");
  });

  it("sends notes: null to clear when original had notes and field is cleared", () => {
    const original = makeReminder({ id: "p-08", notes: "original note" });
    const body = makeReminderPatchBody(original, {
      title: original.title,
      dueAt: original.dueAt,
      notes: "",
      rrulePreset: "none",
    });
    // Empty string means "clear" — send null
    expect(body).toHaveProperty("notes", null);
  });

  it("omits notes when both original and new value are absent/empty", () => {
    const original = makeReminder({ id: "p-09" }); // no notes
    const body = makeReminderPatchBody(original, {
      title: original.title,
      dueAt: original.dueAt,
      notes: "",
      rrulePreset: "none",
    });
    expect(body).not.toHaveProperty("notes");
  });

  it("includes notes when it changed to a non-empty string", () => {
    const original = makeReminder({ id: "p-10", notes: "old" });
    const body = makeReminderPatchBody(original, {
      title: original.title,
      dueAt: original.dueAt,
      notes: "new note",
      rrulePreset: "none",
    });
    expect(body).toHaveProperty("notes", "new note");
  });

  // ---------------------------------------------------------------------------
  // Fix #4: dueAt comparison must use instant equality (getTime()), not raw string equality. The server stores
  // second-truncated RFC3339 ("...:00Z") while buildNextDueAt produces full ISO ("...:00.000Z"). These represent the
  // same instant but differ as strings, causing a spurious patch on every save even when the user did not change the
  // due date.
  // ---------------------------------------------------------------------------

  it("does NOT include dueAt when server RFC3339 and client ISO represent the same instant", () => {
    // Server stores second-truncated RFC3339; client holds .toISOString() form. Both represent 2030-01-01 12:00:00 UTC
    // but differ as strings.
    const serverStored = "2030-01-01T12:00:00Z"; // e.g. what the server returns
    const clientForm = "2030-01-01T12:00:00.000Z"; // what new Date(serverStored).toISOString() produces
    const original = makeReminder({ id: "p-dueAt-same-instant", dueAt: serverStored });
    const body = makeReminderPatchBody(original, {
      title: original.title,
      dueAt: clientForm,
      rrulePreset: "none",
    });
    // Same instant — must NOT include dueAt in the patch body.
    expect(body).not.toHaveProperty("dueAt");
  });

  it("DOES include dueAt when the user picks a genuinely different time", () => {
    const original = makeReminder({ id: "p-dueAt-changed", dueAt: "2030-01-01T12:00:00Z" });
    const body = makeReminderPatchBody(original, {
      title: original.title,
      dueAt: "2030-01-01T13:00:00.000Z", // one hour later
      rrulePreset: "none",
    });
    expect(body).toHaveProperty("dueAt", "2030-01-01T13:00:00.000Z");
  });

  it("does NOT include dueAt when original has sub-second precision and client matches", () => {
    // Guard: if original somehow has millis, they should still match by instant.
    const original = makeReminder({ id: "p-dueAt-millis", dueAt: "2030-06-15T09:00:00.000Z" });
    const body = makeReminderPatchBody(original, {
      title: original.title,
      dueAt: "2030-06-15T09:00:00Z",
      rrulePreset: "none",
    });
    expect(body).not.toHaveProperty("dueAt");
  });
});
