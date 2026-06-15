// Reminders store — module-level singleton, reset on logout.
//
// Safe as a module-level singleton: ssr=false in +layout.ts, browser-only.
// Holds the live list of reminders as patched by SSE events + REST reconcile.
// The SSE router calls upsertReminder / removeReminder; the REST reconcile calls
// upsertReminder in bulk. The layout resets the store on logout / 401 teardown.

import type { components } from "$lib/api/schema.d.ts";

export type ReminderItem = components["schemas"]["Reminder"];

// ---------------------------------------------------------------------------
// Module-level $state — safe: ssr=false, browser-only singleton.
// ---------------------------------------------------------------------------

let _reminders = $state<ReminderItem[]>([]);

// ---------------------------------------------------------------------------
// Read API
// ---------------------------------------------------------------------------

/** Returns the current list of reminders (reactive). */
export function getReminders(): ReminderItem[] {
  return _reminders;
}

/** Returns the reminder with the given id, or null if not found. */
export function getReminderById(id: string): ReminderItem | null {
  return _reminders.find((r) => r.id === id) ?? null;
}

// ---------------------------------------------------------------------------
// Write API
// ---------------------------------------------------------------------------

/**
 * Insert or update a reminder in place.
 * If a reminder with the same id already exists it is replaced; otherwise the
 * new item is appended. Called by the SSE router on reminder.* events and by
 * the REST reconcile on reconnect.
 */
export function upsertReminder(reminder: ReminderItem): void {
  const idx = _reminders.findIndex((r) => r.id === reminder.id);
  if (idx === -1) {
    _reminders.push(reminder);
  } else {
    _reminders[idx] = reminder;
  }
}

/**
 * Remove the reminder with the given id.
 * No-op when the id is not found. Called by the SSE router on reminder.deleted.
 */
export function removeReminder(id: string): void {
  const idx = _reminders.findIndex((r) => r.id === id);
  if (idx !== -1) {
    _reminders.splice(idx, 1);
  }
}

/**
 * Replace the entire reminders list with the given items.
 * Called by the REST reconcile on reconnect to resync from server truth.
 */
export function setReminders(items: ReminderItem[]): void {
  _reminders = items;
}

/**
 * Reset the reminders store to empty.
 * Call on logout or 401 teardown. Safe when already empty.
 */
export function resetReminders(): void {
  _reminders = [];
}
