// Typed SSE event router.
//
// Dispatches incoming SSE events by name to the correct handler. Unknown event
// names are ignored safely. JSON parse errors are caught and suppressed — a
// malformed server event must not bring down the connection.
//
// The router is pure: it takes an event name, raw data string, and a handler
// bag. The caller (the SSE client) supplies the handler bag, which is wired
// to the reminders and conversation stores.
//
// Event taxonomy (from harness.go payload structs + reminders/reminders.go):
//   reminder.*           → onReminderEvent  (reminder store)
//   message.delta        → onMessageDelta   (conversation store — streaming text)
//   message.completed    → onMessageCompleted
//   tool.started         → onToolStarted
//   tool.completed       → onToolCompleted
//   tool.failed          → onToolFailed
//   confirmation.required → onConfirmationRequired
//   confirmation.expired  → onConfirmationExpired
//   turn.failed          → onTurnFailed

// ---------------------------------------------------------------------------
// Payload shapes (mirror the Go structs in harness.go / reminders.go)
// ---------------------------------------------------------------------------

export interface DeltaPayload {
  conversationId: string;
  text: string;
}

export interface CompletedPayload {
  conversationId: string;
  messageId: string;
  finishReason: string;
}

export interface ToolStartedPayload {
  conversationId: string;
  callId: string;
  name: string;
  risk: string;
  argsSummary: string;
}

export interface ToolCompletedPayload {
  conversationId: string;
  callId: string;
  result: unknown;
}

export interface ToolFailedPayload {
  conversationId: string;
  callId: string;
  error: string;
}

export interface ConfirmationRequiredPayload {
  conversationId: string;
  callId: string;
  name: string;
  risk: string;
  args: unknown;
  expiresAt: string;
}

export interface ConfirmationExpiredPayload {
  conversationId: string;
  callId: string;
}

export interface TurnFailedPayload {
  conversationId: string;
  code: string;
}

// ---------------------------------------------------------------------------
// Handler bag
// ---------------------------------------------------------------------------

/** All handlers the router dispatches to. Each is called at most once per event. */
export interface RouterHandlers {
  /** Called for any event whose name starts with "reminder." */
  onReminderEvent: (eventName: string, payload: unknown) => void;
  /** Called for message.delta — append text to the in-flight assistant message. */
  onMessageDelta: (conversationId: string, text: string) => void;
  /** Called for message.completed — the streaming assistant turn is done. */
  onMessageCompleted: (conversationId: string, messageId: string, finishReason: string) => void;
  /** Called for tool.started — a tool call is about to be executed. */
  onToolStarted: (payload: ToolStartedPayload) => void;
  /** Called for tool.completed — a tool call finished successfully. */
  onToolCompleted: (conversationId: string, callId: string, result: unknown) => void;
  /** Called for tool.failed — a tool call returned a model-visible error. */
  onToolFailed: (conversationId: string, callId: string, error: string) => void;
  /** Called for confirmation.required — the UI should present an approve/deny prompt. */
  onConfirmationRequired: (payload: ConfirmationRequiredPayload) => void;
  /** Called for confirmation.expired — the pending confirmation lapsed. */
  onConfirmationExpired: (conversationId: string, callId: string) => void;
  /** Called for turn.failed — the AI turn aborted with an infrastructure error. */
  onTurnFailed: (conversationId: string, code: string) => void;
}

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------

/**
 * Route one SSE event to the correct handler.
 *
 * @param eventName - The SSE "event:" field value (e.g. "message.delta").
 * @param rawData   - The raw "data:" field value (unparsed JSON string).
 * @param handlers  - The handler bag wired to stores.
 *
 * Unknown event names are silently ignored. Malformed JSON data is caught and
 * suppressed — the handler is NOT called if the payload cannot be parsed.
 */
export function routeEvent(eventName: string, rawData: string, handlers: RouterHandlers): void {
  // reminder.* — route everything with this prefix to the reminder handler.
  if (eventName.startsWith("reminder.")) {
    const payload = tryParse(rawData);
    if (payload !== undefined) {
      handlers.onReminderEvent(eventName, payload);
    }
    return;
  }

  switch (eventName) {
    case "message.delta": {
      const p = tryParseObject(rawData);
      if (p !== undefined && typeof p.conversationId === "string" && typeof p.text === "string") {
        handlers.onMessageDelta(p.conversationId, p.text);
      }
      break;
    }

    case "message.completed": {
      const p = tryParseObject(rawData);
      if (
        p !== undefined &&
        typeof p.conversationId === "string" &&
        typeof p.messageId === "string" &&
        typeof p.finishReason === "string"
      ) {
        handlers.onMessageCompleted(p.conversationId, p.messageId, p.finishReason);
      }
      break;
    }

    case "tool.started": {
      const p = tryParseObject(rawData);
      if (
        p !== undefined &&
        typeof p.conversationId === "string" &&
        typeof p.callId === "string" &&
        typeof p.name === "string" &&
        typeof p.risk === "string" &&
        typeof p.argsSummary === "string"
      ) {
        handlers.onToolStarted({
          conversationId: p.conversationId,
          callId: p.callId,
          name: p.name,
          risk: p.risk,
          argsSummary: p.argsSummary,
        });
      }
      break;
    }

    case "tool.completed": {
      const p = tryParseObject(rawData);
      if (p !== undefined && typeof p.conversationId === "string" && typeof p.callId === "string") {
        handlers.onToolCompleted(p.conversationId, p.callId, p.result);
      }
      break;
    }

    case "tool.failed": {
      const p = tryParseObject(rawData);
      if (
        p !== undefined &&
        typeof p.conversationId === "string" &&
        typeof p.callId === "string" &&
        typeof p.error === "string"
      ) {
        handlers.onToolFailed(p.conversationId, p.callId, p.error);
      }
      break;
    }

    case "confirmation.required": {
      const p = tryParseObject(rawData);
      if (
        p !== undefined &&
        typeof p.conversationId === "string" &&
        typeof p.callId === "string" &&
        typeof p.name === "string" &&
        typeof p.risk === "string" &&
        typeof p.expiresAt === "string"
      ) {
        handlers.onConfirmationRequired({
          conversationId: p.conversationId,
          callId: p.callId,
          name: p.name,
          risk: p.risk,
          args: p.args,
          expiresAt: p.expiresAt,
        });
      }
      break;
    }

    case "confirmation.expired": {
      const p = tryParseObject(rawData);
      if (p !== undefined && typeof p.conversationId === "string" && typeof p.callId === "string") {
        handlers.onConfirmationExpired(p.conversationId, p.callId);
      }
      break;
    }

    case "turn.failed": {
      const p = tryParseObject(rawData);
      if (p !== undefined && typeof p.conversationId === "string" && typeof p.code === "string") {
        handlers.onTurnFailed(p.conversationId, p.code);
      }
      break;
    }

    // All other event names are silently ignored.
  }
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

/** Parse rawData as JSON, returning undefined on any parse error. */
function tryParse(rawData: string): unknown {
  try {
    return JSON.parse(rawData) as unknown;
  } catch {
    return undefined;
  }
}

/** Parse rawData as a JSON object, returning undefined on parse error or non-object. */
function tryParseObject(rawData: string): Record<string, unknown> | undefined {
  const v = tryParse(rawData);
  if (typeof v !== "object" || v === null) {
    return undefined;
  }
  return v as Record<string, unknown>;
}
