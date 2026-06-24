// Tests for the reminders store rune logic.
// Rune environment: node + Svelte compiler (vitest project "runes"). Uses flushSync to drain $derived updates
// synchronously.
import { flushSync } from "svelte";
import { afterEach, describe, expect, it } from "vitest";

import {
  getReminders,
  getReminderById,
  upsertReminder,
  removeReminder,
  resetReminders,
  type ReminderItem,
} from "./reminders.svelte.js";

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
// Reset between tests — the module singleton bleeds between suites.
// ---------------------------------------------------------------------------

afterEach(() => {
  resetReminders();
  flushSync();
});

// ---------------------------------------------------------------------------
// Default state
// ---------------------------------------------------------------------------

describe("reminders store — initial state", () => {
  it("starts with an empty list", () => {
    expect(getReminders()).toEqual([]);
  });

  it("returns null for an unknown id", () => {
    expect(getReminderById("nonexistent")).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// upsertReminder — insert and update in place
// ---------------------------------------------------------------------------

describe("upsertReminder()", () => {
  it("inserts a new reminder into the list", () => {
    const r = makeReminder();
    upsertReminder(r);
    flushSync();
    expect(getReminders()).toHaveLength(1);
    expect(getReminders()[0]).toEqual(r);
  });

  it("is retrievable by id after insert", () => {
    const r = makeReminder({ id: "r-abc" });
    upsertReminder(r);
    flushSync();
    expect(getReminderById("r-abc")).toEqual(r);
  });

  it("updates an existing reminder in place (patch)", () => {
    const original = makeReminder({ id: "r1", title: "Original" });
    upsertReminder(original);
    flushSync();

    const updated = makeReminder({ id: "r1", title: "Updated", status: "completed" });
    upsertReminder(updated);
    flushSync();

    expect(getReminders()).toHaveLength(1);
    expect(getReminders()[0]?.title).toBe("Updated");
    expect(getReminders()[0]?.status).toBe("completed");
  });

  it("appends multiple distinct reminders", () => {
    upsertReminder(makeReminder({ id: "r1" }));
    upsertReminder(makeReminder({ id: "r2", title: "Second" }));
    flushSync();
    expect(getReminders()).toHaveLength(2);
  });

  it("does not duplicate when the same id is upserted twice", () => {
    const r = makeReminder({ id: "dup" });
    upsertReminder(r);
    upsertReminder(r);
    flushSync();
    expect(getReminders()).toHaveLength(1);
  });
});

// ---------------------------------------------------------------------------
// removeReminder — delete by id
// ---------------------------------------------------------------------------

describe("removeReminder()", () => {
  it("removes the reminder with the given id", () => {
    upsertReminder(makeReminder({ id: "r1" }));
    upsertReminder(makeReminder({ id: "r2" }));
    flushSync();

    removeReminder("r1");
    flushSync();

    expect(getReminders()).toHaveLength(1);
    expect(getReminderById("r1")).toBeNull();
    expect(getReminderById("r2")).not.toBeNull();
  });

  it("is a no-op when the id does not exist", () => {
    upsertReminder(makeReminder({ id: "r1" }));
    flushSync();

    removeReminder("nonexistent");
    flushSync();

    expect(getReminders()).toHaveLength(1);
  });
});

// ---------------------------------------------------------------------------
// resetReminders — clears all state (called on logout)
// ---------------------------------------------------------------------------

describe("resetReminders()", () => {
  it("clears all reminders", () => {
    upsertReminder(makeReminder({ id: "r1" }));
    upsertReminder(makeReminder({ id: "r2" }));
    flushSync();

    resetReminders();
    flushSync();

    expect(getReminders()).toEqual([]);
  });

  it("is a no-op when already empty", () => {
    resetReminders();
    flushSync();
    expect(getReminders()).toEqual([]);
  });
});

// ---------------------------------------------------------------------------
// Reactive consistency — getReminders and getReminderById reflect mutations
// ---------------------------------------------------------------------------

describe("reactive consistency", () => {
  it("getReminderById reflects upsert immediately", () => {
    expect(getReminderById("x")).toBeNull();
    upsertReminder(makeReminder({ id: "x", title: "X" }));
    expect(getReminderById("x")?.title).toBe("X");
  });

  it("getReminders length follows upsert/remove sequence", () => {
    upsertReminder(makeReminder({ id: "a" }));
    upsertReminder(makeReminder({ id: "b" }));
    expect(getReminders()).toHaveLength(2);
    removeReminder("a");
    expect(getReminders()).toHaveLength(1);
    removeReminder("b");
    expect(getReminders()).toHaveLength(0);
  });
});
