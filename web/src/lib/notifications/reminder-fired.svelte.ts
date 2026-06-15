// Reminder-fired notification helper.
//
// Called by the SSE client on every `reminder.fired` event. Raises:
//   1. An in-app toast (always, via @qovira/ui imperative API).
//   2. An OS-level Notification (only when Notification.permission === "granted").
//
// Permission ask (AC3 — deferred):
//   The first time a reminder fires while permission is "default" (unset), a
//   session-scoped prompt flag is set. The layout reads this flag and renders a
//   gentle dismissible banner offering to enable OS notifications. Once the user
//   decides (or dismisses), the flag is cleared and the prompt never re-raises
//   during this session. resetNotificationPromptState() is called on logout to
//   clear for the next session.
//
// Module-level state is safe here: ssr=false is set in +layout.ts — this module
// only ever runs in the browser, one instance per page load.

import { toast } from "@qovira/ui";
import { notification_reminder_fired_body, notification_reminder_fired_title } from "$lib/paraglide/messages.js";
import { formatDueAt } from "$lib/format/datetime.js";

// ---------------------------------------------------------------------------
// Payload type — mirrors Go FiredEventPayload
// ---------------------------------------------------------------------------

export interface FiredEventPayload {
  reminderId: string;
  title: string;
  dueAt: string;
  firedAt: string;
}

// ---------------------------------------------------------------------------
// Module-level permission-prompt state (session-scoped, CSR-only)
//
// _promptPending is $state so components can derive from isPendingPermissionPrompt()
// reactively. Module-level $state is safe here: ssr=false in +layout.ts means
// this module only ever runs in the browser (one instance per page load, never
// shared across SSR requests).
// ---------------------------------------------------------------------------

/** Whether the deferred OS-notification permission prompt is currently pending. */
let _promptPending = $state(false);

/**
 * Whether the user has already decided (accepted or dismissed) the permission
 * prompt this session. Prevents re-prompting.
 */
let _promptDecided = false;

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

/**
 * Handle a `reminder.fired` SSE event.
 *
 * Raises an in-app toast every time. When `Notification.permission` is
 * `"granted"`, also raises an OS `Notification`. When permission is `"default"`
 * (unset) and the user has not yet been asked this session, sets the prompt
 * flag so the layout can render the deferred permission ask.
 */
export function notifyReminderFired(payload: FiredEventPayload): void {
  // 1. In-app toast — always (AC1). Include the formatted due date in the body so
  //    the user sees when the reminder was due without opening the reminders list.
  const toastTitle = notification_reminder_fired_title({ title: payload.title });
  const toastBody = notification_reminder_fired_body({ dueAt: formatDueAt(payload.dueAt) });
  toast.info(`${toastTitle} — ${toastBody}`, { duration: 7000 });

  // 2. OS notification — only when explicitly granted (AC2, AC4).
  if (typeof Notification !== "undefined" && Notification.permission === "granted") {
    const body = notification_reminder_fired_body({ dueAt: formatDueAt(payload.dueAt) });
    // Bare title is intentional: OS chrome already frames this as a notification,
    // so the "Reminder: …" prefix used in the toast title would be redundant here.
    new Notification(payload.title, { body });
  }

  // 3. Deferred permission prompt (AC3): only when unset and not yet decided.
  if (typeof Notification !== "undefined" && Notification.permission === "default" && !_promptDecided) {
    _promptPending = true;
  }
}

/**
 * Returns true when the permission prompt should be shown.
 * Read reactively in the layout via a `$derived` or checked in an effect.
 */
export function isPendingPermissionPrompt(): boolean {
  return _promptPending;
}

/**
 * Dismiss the permission prompt without requesting permission.
 * Marks the decision as made so the prompt is not raised again this session.
 */
export function dismissNotificationPrompt(): void {
  _promptPending = false;
  _promptDecided = true;
}

/**
 * Call Notification.requestPermission() and close the prompt.
 * Safe to call even when the Notification API is unavailable.
 */
export async function requestOsNotificationPermission(): Promise<void> {
  _promptPending = false;
  _promptDecided = true;
  if (typeof Notification !== "undefined") {
    await Notification.requestPermission();
  }
}

/**
 * Reset per-session notification state.
 * Call on logout so the next session starts with a clean slate.
 */
export function resetNotificationPromptState(): void {
  _promptPending = false;
  _promptDecided = false;
}
