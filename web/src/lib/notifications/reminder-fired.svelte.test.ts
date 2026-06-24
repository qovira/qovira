// Tests for the reminder-fired notification helper. Environment: runes (node + Svelte compiler) — included via
// src/**/*.svelte.test.ts. The module uses $state for _promptPending, which requires the Svelte compiler transform that
// the runes project provides.
//
// Coverage:
//   AC1: toast.info() is called every time reminder.fired arrives.
//   AC2: new Notification() is called when permission is "granted".
//   AC4: new Notification() is NOT called when permission is "denied" or "default".
//   AC3: requestPermission() is called when the prompt is accepted (deferred path).
//   AC3: prompt state resets after resetNotificationPromptState().

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// ---------------------------------------------------------------------------
// Mock @qovira/ui toast before importing the module under test.
// ---------------------------------------------------------------------------

const toastInfoMock = vi.fn(() => "toast-1");

vi.mock("@qovira/ui", () => ({
  toast: { info: toastInfoMock },
}));

// ---------------------------------------------------------------------------
// Mock Paraglide messages — return a fixed string so tests are locale-stable.
// ---------------------------------------------------------------------------

vi.mock("$lib/paraglide/messages.js", () => ({
  notification_reminder_fired_title: (p: { title: string }) => `Reminder: ${p.title}`,
  notification_reminder_fired_body: (p: { dueAt: string }) => `Due ${p.dueAt}`,
  notification_prompt_title: () => "Stay on top of your reminders",
  notification_prompt_body: () => "Get OS notifications when a reminder fires.",
  notification_prompt_enable: () => "Turn on",
  notification_prompt_dismiss: () => "Maybe later",
}));

// ---------------------------------------------------------------------------
// Helpers — reset module-level state between tests
// ---------------------------------------------------------------------------

async function freshModule() {
  // Re-import to get a fresh module-level state after each test.
  vi.resetModules();
  return import("./reminder-fired.svelte.js");
}

// ---------------------------------------------------------------------------
// Notification stub helpers.
//
// The Web Notification API is a constructable global. To avoid ESLint's no-extraneous-class / no-useless-constructor
// errors, we build the stub as a plain function (satisfies `new` semantics in JS) with the necessary static properties
// attached. TypeScript sees a newable function via the cast.
// ---------------------------------------------------------------------------

interface NotificationLike {
  new (title: string, opts?: NotificationOptions): void;
  permission: NotificationPermission;
  requestPermission: () => Promise<NotificationPermission>;
}

/** Build a Notification global stub for a given permission level. */
function makeNotificationStub(
  permission: NotificationPermission,
  requestPermissionImpl?: () => Promise<NotificationPermission>,
): NotificationLike {
  // eslint-disable-next-line @typescript-eslint/no-empty-function
  const ctor = function NotificationStub() {} as unknown as NotificationLike;
  ctor.permission = permission;
  ctor.requestPermission = requestPermissionImpl ?? (() => Promise.resolve(permission));
  return ctor;
}

/** Build a Notification stub that records constructor calls. */
function makeTrackedNotificationStub(permission: NotificationPermission): {
  stub: NotificationLike;
  calls: [string, NotificationOptions | undefined][];
} {
  const calls: [string, NotificationOptions | undefined][] = [];
  const stub = function NotificationTracked(title: string, opts?: NotificationOptions) {
    calls.push([title, opts]);
  } as unknown as NotificationLike;
  stub.permission = permission;
  stub.requestPermission = () => Promise.resolve(permission);
  return { stub, calls };
}

// ---------------------------------------------------------------------------
// Shared payload fixture
// ---------------------------------------------------------------------------

const PAYLOAD = {
  reminderId: "rem-1",
  title: "Call dentist",
  dueAt: "2030-01-15T09:00:00Z",
  firedAt: "2030-01-15T09:00:01Z",
};

// ---------------------------------------------------------------------------
// AC1: toast.info() is always called on reminder.fired
// ---------------------------------------------------------------------------

describe("notifyReminderFired — toast (AC1)", () => {
  beforeEach(() => {
    toastInfoMock.mockClear();
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("calls toast.info with the reminder title every time", async () => {
    vi.stubGlobal("Notification", makeNotificationStub("granted"));
    const { notifyReminderFired } = await freshModule();

    notifyReminderFired(PAYLOAD);

    expect(toastInfoMock).toHaveBeenCalledOnce();
    // The toast message must contain the reminder title.
    const firstCall = toastInfoMock.mock.calls[0] as [string, ...unknown[]] | undefined;
    expect(firstCall?.[0]).toContain("Call dentist");
  });

  it("includes a human-formatted dueAt (not a raw ISO string) in the toast message", async () => {
    vi.stubGlobal("Notification", makeNotificationStub("granted"));
    const { notifyReminderFired } = await freshModule();

    notifyReminderFired(PAYLOAD);

    expect(toastInfoMock).toHaveBeenCalledOnce();
    const firstCall = toastInfoMock.mock.calls[0] as [string, ...unknown[]] | undefined;
    const toastMessage = firstCall?.[0] ?? "";
    // The raw ISO format must not appear verbatim in the toast message.
    expect(toastMessage).not.toMatch(/\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z/);
    // The message must still be non-empty.
    expect(toastMessage.length).toBeGreaterThan(0);
  });

  it("calls toast.info on every invocation, not just the first", async () => {
    vi.stubGlobal("Notification", makeNotificationStub("denied"));
    const { notifyReminderFired } = await freshModule();

    notifyReminderFired(PAYLOAD);
    notifyReminderFired({ ...PAYLOAD, reminderId: "rem-2" });
    notifyReminderFired({ ...PAYLOAD, reminderId: "rem-3" });

    expect(toastInfoMock).toHaveBeenCalledTimes(3);
  });
});

// ---------------------------------------------------------------------------
// AC2: OS Notification is raised when permission is "granted"
// ---------------------------------------------------------------------------

describe("notifyReminderFired — OS notification (AC2)", () => {
  beforeEach(() => {
    toastInfoMock.mockClear();
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("constructs a new Notification when permission is 'granted'", async () => {
    const { stub, calls } = makeTrackedNotificationStub("granted");
    vi.stubGlobal("Notification", stub);

    const { notifyReminderFired } = await freshModule();
    notifyReminderFired(PAYLOAD);

    expect(calls).toHaveLength(1);
    // Title argument to the Notification constructor must contain the reminder title.
    expect(calls[0]?.[0]).toContain("Call dentist");
  });

  it("passes a human-formatted dueAt (not a raw ISO string) to the OS notification body", async () => {
    const { stub, calls } = makeTrackedNotificationStub("granted");
    vi.stubGlobal("Notification", stub);

    const { notifyReminderFired } = await freshModule();
    notifyReminderFired(PAYLOAD);

    expect(calls).toHaveLength(1);
    const body = calls[0]?.[1]?.body ?? "";
    // The raw ISO format contains a "T" time-separator and a "Z" suffix. After formatting via Intl.DateTimeFormat
    // those are gone.
    expect(body).not.toMatch(/\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z/);
    // The body must be non-empty (the message function produced something).
    expect(body.length).toBeGreaterThan(0);
  });

  it("does NOT construct a Notification when permission is 'denied' (AC4)", async () => {
    const { stub, calls } = makeTrackedNotificationStub("denied");
    vi.stubGlobal("Notification", stub);

    const { notifyReminderFired } = await freshModule();
    notifyReminderFired(PAYLOAD);

    expect(calls).toHaveLength(0);
  });

  it("does NOT construct a Notification when permission is 'default' (AC4)", async () => {
    const { stub, calls } = makeTrackedNotificationStub("default");
    vi.stubGlobal("Notification", stub);

    const { notifyReminderFired } = await freshModule();
    notifyReminderFired(PAYLOAD);

    expect(calls).toHaveLength(0);
  });

  it("does NOT construct a Notification when the Notification API is unavailable (AC4)", async () => {
    // Simulate an environment without the Notification API.
    const saved = (globalThis as Record<string, unknown>).Notification;
    delete (globalThis as Record<string, unknown>).Notification;

    try {
      const { notifyReminderFired } = await freshModule();
      // Must not throw even when Notification is missing.
      expect(() => {
        notifyReminderFired(PAYLOAD);
      }).not.toThrow();
    } finally {
      if (saved !== undefined) {
        (globalThis as Record<string, unknown>).Notification = saved;
      }
    }
  });
});

// ---------------------------------------------------------------------------
// AC3: Deferred permission prompt — isPendingPermissionPrompt / resetNotificationPromptState
// ---------------------------------------------------------------------------

describe("notifyReminderFired — deferred permission prompt (AC3)", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("isPendingPermissionPrompt() becomes true after first fired event with default permission", async () => {
    vi.stubGlobal("Notification", makeNotificationStub("default"));
    const { notifyReminderFired, isPendingPermissionPrompt } = await freshModule();

    expect(isPendingPermissionPrompt()).toBe(false);
    notifyReminderFired(PAYLOAD);
    expect(isPendingPermissionPrompt()).toBe(true);
  });

  it("isPendingPermissionPrompt() stays false when permission is already 'granted'", async () => {
    vi.stubGlobal("Notification", makeNotificationStub("granted"));
    const { notifyReminderFired, isPendingPermissionPrompt } = await freshModule();

    notifyReminderFired(PAYLOAD);
    expect(isPendingPermissionPrompt()).toBe(false);
  });

  it("isPendingPermissionPrompt() stays false when permission is already 'denied'", async () => {
    vi.stubGlobal("Notification", makeNotificationStub("denied"));
    const { notifyReminderFired, isPendingPermissionPrompt } = await freshModule();

    notifyReminderFired(PAYLOAD);
    expect(isPendingPermissionPrompt()).toBe(false);
  });

  it("isPendingPermissionPrompt() resets to false after resetNotificationPromptState()", async () => {
    vi.stubGlobal("Notification", makeNotificationStub("default"));
    const { notifyReminderFired, isPendingPermissionPrompt, resetNotificationPromptState } = await freshModule();

    notifyReminderFired(PAYLOAD);
    expect(isPendingPermissionPrompt()).toBe(true);

    resetNotificationPromptState();
    expect(isPendingPermissionPrompt()).toBe(false);
  });

  it("does not raise the prompt again once dismissed in the same session", async () => {
    vi.stubGlobal("Notification", makeNotificationStub("default"));
    const { notifyReminderFired, isPendingPermissionPrompt, dismissNotificationPrompt } = await freshModule();

    notifyReminderFired(PAYLOAD);
    expect(isPendingPermissionPrompt()).toBe(true);

    dismissNotificationPrompt();
    expect(isPendingPermissionPrompt()).toBe(false);

    // Another fired event must NOT re-raise the prompt.
    notifyReminderFired({ ...PAYLOAD, reminderId: "rem-2" });
    expect(isPendingPermissionPrompt()).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// AC3: requestPermission — called when the user accepts the prompt
// ---------------------------------------------------------------------------

describe("notifyReminderFired — requestPermission (AC3)", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("requestOsNotificationPermission() calls Notification.requestPermission()", async () => {
    const requestPermissionMock = vi.fn(() => Promise.resolve("granted" as NotificationPermission));
    vi.stubGlobal("Notification", makeNotificationStub("default", requestPermissionMock));

    const { requestOsNotificationPermission } = await freshModule();

    await requestOsNotificationPermission();

    expect(requestPermissionMock).toHaveBeenCalledOnce();
  });

  it("requestOsNotificationPermission() dismisses the prompt and resolves even without Notification API", async () => {
    const saved = (globalThis as Record<string, unknown>).Notification;
    delete (globalThis as Record<string, unknown>).Notification;

    try {
      const { requestOsNotificationPermission, isPendingPermissionPrompt } = await freshModule();
      await expect(requestOsNotificationPermission()).resolves.not.toThrow();
      expect(isPendingPermissionPrompt()).toBe(false);
    } finally {
      if (saved !== undefined) {
        (globalThis as Record<string, unknown>).Notification = saved;
      }
    }
  });
});
