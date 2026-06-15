// Tests for the SSE event router.
// Environment: browser (happy-dom) — included via src/lib/**/*.test.ts pattern.
import { describe, expect, it, vi } from "vitest";

import { routeEvent, type RouterHandlers } from "./router.js";

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function makeHandlers(overrides: Partial<RouterHandlers> = {}): RouterHandlers {
  return {
    onReminderEvent: vi.fn(),
    onMessageDelta: vi.fn(),
    onMessageCompleted: vi.fn(),
    onToolStarted: vi.fn(),
    onToolCompleted: vi.fn(),
    onToolFailed: vi.fn(),
    onConfirmationRequired: vi.fn(),
    onConfirmationExpired: vi.fn(),
    onTurnFailed: vi.fn(),
    ...overrides,
  };
}

// ---------------------------------------------------------------------------
// Unknown event names are ignored safely
// ---------------------------------------------------------------------------

describe("routeEvent() — unknown event names", () => {
  it("ignores an unknown event name without throwing", () => {
    const h = makeHandlers();
    expect(() => {
      routeEvent("unknown.event", "{}", h);
    }).not.toThrow();
    expect(h.onReminderEvent).not.toHaveBeenCalled();
    expect(h.onMessageDelta).not.toHaveBeenCalled();
  });

  it("ignores an empty event name", () => {
    const h = makeHandlers();
    expect(() => {
      routeEvent("", "{}", h);
    }).not.toThrow();
  });

  it("ignores an event with malformed JSON data without throwing", () => {
    const h = makeHandlers();
    expect(() => {
      routeEvent("message.delta", "not-json{{{", h);
    }).not.toThrow();
  });
});

// ---------------------------------------------------------------------------
// reminder.* events → onReminderEvent
// ---------------------------------------------------------------------------

describe("routeEvent() — reminder.* routing", () => {
  it("routes reminder.fired to onReminderEvent", () => {
    const h = makeHandlers();
    const payload = {
      reminderId: "r1",
      title: "Stand up",
      dueAt: "2030-01-01T09:00:00Z",
      firedAt: "2030-01-01T09:00:01Z",
    };
    routeEvent("reminder.fired", JSON.stringify(payload), h);
    expect(h.onReminderEvent).toHaveBeenCalledOnce();
    expect(h.onReminderEvent).toHaveBeenCalledWith("reminder.fired", payload);
  });

  it("routes reminder.created to onReminderEvent", () => {
    const h = makeHandlers();
    routeEvent("reminder.created", "{}", h);
    expect(h.onReminderEvent).toHaveBeenCalledWith("reminder.created", {});
  });

  it("routes reminder.updated to onReminderEvent", () => {
    const h = makeHandlers();
    routeEvent("reminder.updated", "{}", h);
    expect(h.onReminderEvent).toHaveBeenCalledWith("reminder.updated", {});
  });

  it("routes reminder.completed to onReminderEvent", () => {
    const h = makeHandlers();
    routeEvent("reminder.completed", "{}", h);
    expect(h.onReminderEvent).toHaveBeenCalledWith("reminder.completed", {});
  });

  it("routes reminder.deleted to onReminderEvent", () => {
    const h = makeHandlers();
    routeEvent("reminder.deleted", "{}", h);
    expect(h.onReminderEvent).toHaveBeenCalledWith("reminder.deleted", {});
  });

  it("does not call any chat handler for a reminder event", () => {
    const h = makeHandlers();
    routeEvent("reminder.fired", "{}", h);
    expect(h.onMessageDelta).not.toHaveBeenCalled();
    expect(h.onTurnFailed).not.toHaveBeenCalled();
  });
});

// ---------------------------------------------------------------------------
// message.delta → onMessageDelta
// ---------------------------------------------------------------------------

describe("routeEvent() — message.delta", () => {
  it("routes message.delta with conversationId and text", () => {
    const h = makeHandlers();
    const payload = { conversationId: "conv-1", text: "Hello" };
    routeEvent("message.delta", JSON.stringify(payload), h);
    expect(h.onMessageDelta).toHaveBeenCalledWith("conv-1", "Hello");
  });
});

// ---------------------------------------------------------------------------
// message.completed → onMessageCompleted
// ---------------------------------------------------------------------------

describe("routeEvent() — message.completed", () => {
  it("routes message.completed with conversationId, messageId, finishReason", () => {
    const h = makeHandlers();
    const payload = { conversationId: "conv-1", messageId: "msg-1", finishReason: "stop" };
    routeEvent("message.completed", JSON.stringify(payload), h);
    expect(h.onMessageCompleted).toHaveBeenCalledWith("conv-1", "msg-1", "stop");
  });
});

// ---------------------------------------------------------------------------
// tool.started → onToolStarted
// ---------------------------------------------------------------------------

describe("routeEvent() — tool.started", () => {
  it("routes tool.started with full payload", () => {
    const h = makeHandlers();
    const payload = {
      conversationId: "conv-1",
      callId: "call-1",
      name: "list_reminders",
      risk: "read",
      argsSummary: "{}",
    };
    routeEvent("tool.started", JSON.stringify(payload), h);
    expect(h.onToolStarted).toHaveBeenCalledWith(payload);
  });
});

// ---------------------------------------------------------------------------
// tool.completed → onToolCompleted
// ---------------------------------------------------------------------------

describe("routeEvent() — tool.completed", () => {
  it("routes tool.completed with conversationId, callId, result", () => {
    const h = makeHandlers();
    const payload = { conversationId: "conv-1", callId: "call-1", result: { ok: true } };
    routeEvent("tool.completed", JSON.stringify(payload), h);
    expect(h.onToolCompleted).toHaveBeenCalledWith("conv-1", "call-1", { ok: true });
  });
});

// ---------------------------------------------------------------------------
// tool.failed → onToolFailed
// ---------------------------------------------------------------------------

describe("routeEvent() — tool.failed", () => {
  it("routes tool.failed with conversationId, callId, error", () => {
    const h = makeHandlers();
    const payload = { conversationId: "conv-1", callId: "call-1", error: "not found" };
    routeEvent("tool.failed", JSON.stringify(payload), h);
    expect(h.onToolFailed).toHaveBeenCalledWith("conv-1", "call-1", "not found");
  });
});

// ---------------------------------------------------------------------------
// confirmation.required → onConfirmationRequired
// ---------------------------------------------------------------------------

describe("routeEvent() — confirmation.required", () => {
  it("routes confirmation.required with full payload", () => {
    const h = makeHandlers();
    const payload = {
      conversationId: "conv-1",
      callId: "call-1",
      name: "delete_reminder",
      risk: "destructive",
      args: { id: "r1" },
      expiresAt: "2030-01-01T10:00:00Z",
    };
    routeEvent("confirmation.required", JSON.stringify(payload), h);
    expect(h.onConfirmationRequired).toHaveBeenCalledWith(payload);
  });
});

// ---------------------------------------------------------------------------
// confirmation.expired → onConfirmationExpired
// ---------------------------------------------------------------------------

describe("routeEvent() — confirmation.expired", () => {
  it("routes confirmation.expired with conversationId and callId", () => {
    const h = makeHandlers();
    const payload = { conversationId: "conv-1", callId: "call-1" };
    routeEvent("confirmation.expired", JSON.stringify(payload), h);
    expect(h.onConfirmationExpired).toHaveBeenCalledWith("conv-1", "call-1");
  });
});

// ---------------------------------------------------------------------------
// turn.failed → onTurnFailed
// ---------------------------------------------------------------------------

describe("routeEvent() — turn.failed", () => {
  it("routes turn.failed with conversationId and code", () => {
    const h = makeHandlers();
    const payload = { conversationId: "conv-1", code: "infrastructure" };
    routeEvent("turn.failed", JSON.stringify(payload), h);
    expect(h.onTurnFailed).toHaveBeenCalledWith("conv-1", "infrastructure");
  });
});
