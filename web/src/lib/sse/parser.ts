// SSE wire-frame parser.
//
// Parses raw SSE text (as accumulated in a read buffer) into typed SseFrame objects. Each frame is terminated by a
// blank line ("\n\n" or "\r\n\r\n"). Incomplete frames at the end of a chunk are NOT returned — the caller must buffer
// across reads and pass the remainder back on the next call.
//
// Wire format per the SSE spec (HTML Living Standard, §9.2.6):
//   event: <name>   -- optional; absent means "message"
//   id: <string>    -- optional; the last-event-id value
//   data: <payload> -- the event data; may appear multiple times (joined with \n)
//   : <comment>     -- ignored (keepalive pings arrive as ": ping\n\n")
//
// A single leading space after the colon is stripped per the spec.

/** A parsed SSE frame. All fields are optional; only `data` is typically required. */
export interface SseFrame {
  /** The event name from "event: <name>" lines. Absent for unnamed events. */
  event?: string;
  /** The last-event-id from "id: <value>" lines. */
  id?: string;
  /** The raw data payload, joining multiple "data:" lines with "\n". */
  data?: string;
}

/**
 * Parse all complete SSE frames from a raw text buffer.
 *
 * Returns only frames that have at least a data field (pure comment frames like ": ping\n\n" produce no output).
 * Incomplete frames (no trailing blank line) are silently dropped — the caller is responsible for buffering the
 * remainder across stream reads.
 */
export function parseFrames(text: string): SseFrame[] {
  // Normalise CRLF → LF, then split on the blank-line frame boundary.
  const normalised = text.replace(/\r\n/g, "\n").replace(/\r/g, "\n");
  // A frame is terminated by "\n\n". Split on that separator. If the text ends with "\n\n" the last element is ""; if
  // the text does NOT end with "\n\n" the last element is an incomplete frame and must be dropped.
  const rawFrames = normalised.split("\n\n");
  // Drop the last element: it is either "" (the trailing blank line after a complete frame — no content) or an
  // incomplete frame with no blank terminator.
  const completeFrames = rawFrames.slice(0, -1);

  const result: SseFrame[] = [];

  for (const rawFrame of completeFrames) {
    if (rawFrame.trim() === "") {
      continue;
    }

    const frame: SseFrame = {};
    const dataParts: string[] = [];

    for (const line of rawFrame.split("\n")) {
      if (line.startsWith(":")) {
        // Comment line — ignore (keepalive pings arrive as ": ping").
        continue;
      }

      const colonIdx = line.indexOf(":");
      if (colonIdx === -1) {
        // Field with no value — ignore (bare field name, not used for our events).
        continue;
      }

      const field = line.slice(0, colonIdx);
      // Strip exactly one leading space after the colon per the SSE spec.
      const rawValue = line.slice(colonIdx + 1);
      const value = rawValue.startsWith(" ") ? rawValue.slice(1) : rawValue;

      switch (field) {
        case "event":
          frame.event = value;
          break;
        case "id":
          frame.id = value;
          break;
        case "data":
          dataParts.push(value);
          break;
        // All other field names are ignored per the spec.
      }
    }

    if (dataParts.length > 0) {
      frame.data = dataParts.join("\n");
      result.push(frame);
    }
    // A frame with no data lines (e.g. a pure comment block) is dropped.
  }

  return result;
}
