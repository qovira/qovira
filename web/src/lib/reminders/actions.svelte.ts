// Reminder optimistic-update actions.
//
// Exports thin, testable helper functions that encapsulate the apply/confirm/
// revert steps for each of the three interactions:
//   1. Quick-add  — applyOptimisticCreate / confirmCreate / revertCreate
//   2. Complete   — applyOptimisticComplete / confirmComplete / revertComplete
//   3. Edit save  — buildNextDueAt / makeReminderPatchBody (no optimism — server-only)
//
// These are the pure or store-driving helpers called by the page component.
// Keeping them here makes TDD straightforward: the page just wires the UI;
// the logic lives here and is proven by actions.svelte.test.ts.

import type { ReminderItem } from "$lib/stores/reminders.svelte.js";
import { getReminderById, upsertReminder, removeReminder } from "$lib/stores/reminders.svelte.js";

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

/** Recurrence presets the edit sheet exposes. */
export type RrulePreset = "none" | "daily" | "weekly" | "monthly" | "keep";

/** The form values collected by the edit sheet. */
export interface EditFormValues {
  title: string;
  /** RFC3339 UTC string (already converted from the datetime-local input). */
  dueAt: string;
  notes?: string;
  rrulePreset: RrulePreset;
}

/**
 * The PATCH body sent to the server — only the changed fields, with null to
 * clear nullable fields. exactOptionalPropertyTypes is on: absent keys omitted.
 */
export interface ReminderPatchBody {
  title?: string;
  dueAt?: string;
  notes?: string | null;
  rrule?: string | null;
  autoComplete?: boolean;
  status?: "active" | "completed";
}

// ---------------------------------------------------------------------------
// RRULE preset map
// ---------------------------------------------------------------------------

const RRULE_BY_PRESET: Record<Exclude<RrulePreset, "none" | "keep">, string> = {
  daily: "FREQ=DAILY",
  weekly: "FREQ=WEEKLY",
  monthly: "FREQ=MONTHLY",
};

// ---------------------------------------------------------------------------
// 1. Quick-add: optimistic create
// ---------------------------------------------------------------------------

/**
 * Insert the temp reminder into the store immediately (optimistic).
 * Caller provides the fully-built temp ReminderItem with a ulid temp id.
 */
export function applyOptimisticCreate(temp: ReminderItem): void {
  upsertReminder(temp);
}

/**
 * On success: remove temp, upsert the real server reminder.
 * The server's `reminder.created` SSE echo will idempotently upsert the same
 * id again — that is harmless.
 */
export function confirmCreate(tempId: string, real: ReminderItem): void {
  removeReminder(tempId);
  upsertReminder(real);
}

/**
 * On rejection (AC5): remove the temp reminder. Store returns to prior state.
 */
export function revertCreate(tempId: string): void {
  removeReminder(tempId);
}

// ---------------------------------------------------------------------------
// 2. Complete: optimistic complete
// ---------------------------------------------------------------------------

/**
 * Optimistically mark the reminder as completed.
 * Returns a snapshot of the prior state (for revert), or null if not found.
 */
export function applyOptimisticComplete(id: string): ReminderItem | null {
  const current = getReminderById(id);
  if (current === null) return null;

  // Snapshot before mutation (spread to clone — proxy-safe).
  const snapshot: ReminderItem = { ...current };

  upsertReminder({
    ...current,
    status: "completed",
    completedAt: new Date().toISOString(),
  });

  return snapshot;
}

/**
 * On success: upsert the authoritative server reminder (which may have
 * different completedAt, lastFiredAt, etc.). The SSE echo also reconciles
 * idempotently.
 */
export function confirmComplete(real: ReminderItem): void {
  upsertReminder(real);
}

/**
 * On rejection (AC5): restore the exact prior snapshot.
 */
export function revertComplete(snapshot: ReminderItem): void {
  upsertReminder(snapshot);
}

// ---------------------------------------------------------------------------
// 3. Edit: utility helpers (non-optimistic — page calls PATCH directly)
// ---------------------------------------------------------------------------

/**
 * Convert a `datetime-local` string value ("2030-06-15T09:00") to an
 * RFC3339 UTC ISO string ("2030-06-15T07:00:00.000Z" in UTC+2, etc.).
 *
 * Returns an empty string for empty input (caller validates before submit).
 */
export function buildNextDueAt(datetimeLocal: string): string {
  if (!datetimeLocal) return "";
  const d = new Date(datetimeLocal);
  if (Number.isNaN(d.getTime())) return "";
  return d.toISOString();
}

/**
 * Compute the default datetime-local value for the due-date input,
 * set to the top of the next hour (a sensible near-future default).
 *
 * Returns a string in the format expected by `<input type="datetime-local">`,
 * i.e. "YYYY-MM-DDTHH:MM" (no seconds, no Z — local time).
 */
export function defaultDueAtLocal(): string {
  const now = new Date();
  // Advance to the top of the next hour.
  now.setMinutes(0, 0, 0);
  now.setHours(now.getHours() + 1);
  // Format as "YYYY-MM-DDTHH:MM" in local time.
  const pad = (n: number): string => String(n).padStart(2, "0");
  const y = String(now.getFullYear());
  const mo = pad(now.getMonth() + 1);
  const d = pad(now.getDate());
  const h = pad(now.getHours());
  const m = pad(now.getMinutes());
  return `${y}-${mo}-${d}T${h}:${m}`;
}

/**
 * Convert a UTC RFC3339 dueAt to a `datetime-local` string for the edit
 * sheet input (local time, no seconds, no Z).
 */
export function dueAtToLocal(dueAt: string): string {
  if (!dueAt) return "";
  const d = new Date(dueAt);
  if (Number.isNaN(d.getTime())) return "";
  const pad = (n: number): string => String(n).padStart(2, "0");
  const y = String(d.getFullYear());
  const mo = pad(d.getMonth() + 1);
  const day = pad(d.getDate());
  const h = pad(d.getHours());
  const m = pad(d.getMinutes());
  return `${y}-${mo}-${day}T${h}:${m}`;
}

/**
 * Infer the RrulePreset from an existing rrule string.
 * Known presets → their preset key. Unknown → "keep". Absent → "none".
 */
export function rruleToPreset(rrule: string | undefined): RrulePreset {
  if (rrule === undefined) return "none";
  if (rrule === "FREQ=DAILY") return "daily";
  if (rrule === "FREQ=WEEKLY") return "weekly";
  if (rrule === "FREQ=MONTHLY") return "monthly";
  return "keep";
}

/**
 * Build the PATCH body containing only the fields that changed.
 *
 * - Present + changed  → include the new value
 * - Absent from `next` → omit (exactOptionalPropertyTypes: omit, don't pass undefined)
 * - Nullable fields:
 *     notes: "" means "clear" → send null; absent/unchanged → omit
 *     rrule: preset "none" + original had rrule → send null; "keep" → omit
 */
export function makeReminderPatchBody(original: ReminderItem, next: EditFormValues): ReminderPatchBody {
  const body: ReminderPatchBody = {};

  // title
  if (next.title !== original.title) {
    body.title = next.title;
  }

  // dueAt: compare by instant (getTime()), not raw string.
  // The server stores second-truncated RFC3339 ("…:00Z") while buildNextDueAt
  // produces full ISO ("…:00.000Z"). They are the same instant but differ as
  // strings, causing a spurious patch on every save if we compare strings.
  // Using getTime() avoids the false positive and prevents silently zeroing
  // sub-minute seconds that dueAtToLocal drops from the input form value.
  if (new Date(next.dueAt).getTime() !== new Date(original.dueAt).getTime()) {
    body.dueAt = next.dueAt;
  }

  // notes — empty string → clear (null); changed non-empty → set; unchanged → omit
  const originalNotes = original.notes ?? "";
  const nextNotes = next.notes ?? "";
  if (nextNotes !== originalNotes) {
    body.notes = nextNotes === "" ? null : nextNotes;
  }

  // rrule from preset
  switch (next.rrulePreset) {
    case "none": {
      // Clear rrule only if there was one before.
      if (original.rrule !== undefined) {
        body.rrule = null;
      }
      break;
    }
    case "keep": {
      // Preserve whatever the server has — do not include in patch.
      break;
    }
    default: {
      // daily / weekly / monthly
      const newRrule = RRULE_BY_PRESET[next.rrulePreset];
      if (newRrule !== original.rrule) {
        body.rrule = newRrule;
      }
      break;
    }
  }

  return body;
}
