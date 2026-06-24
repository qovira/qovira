// Tests for the SSE frame parser.
// Environment: browser (happy-dom) — included via src/lib/**/*.test.ts pattern.
import { describe, expect, it } from "vitest";

import { parseFrames, type SseFrame } from "./parser.js";

// ---------------------------------------------------------------------------
// parseFrames — parse raw SSE text into typed frames
// ---------------------------------------------------------------------------

describe("parseFrames()", () => {
  it("returns an empty array for an empty string", () => {
    expect(parseFrames("")).toEqual([]);
  });

  it("returns an empty array for a string with only newlines", () => {
    expect(parseFrames("\n\n\n")).toEqual([]);
  });

  it("ignores comment lines starting with ':'", () => {
    const text = ": ping keepalive\n\n";
    expect(parseFrames(text)).toEqual([]);
  });

  it("parses a complete SSE frame with event, id, and data", () => {
    const text = 'event: message.delta\nid: 1\ndata: {"text":"hello"}\n\n';
    const frames = parseFrames(text);
    expect(frames).toHaveLength(1);
    const frame = frames[0];
    expect(frame).toBeDefined();
    if (frame === undefined) {
      return;
    }
    expect(frame.event).toBe("message.delta");
    expect(frame.id).toBe("1");
    expect(frame.data).toBe('{"text":"hello"}');
  });

  it("parses a frame with only data (no event or id)", () => {
    const text = "data: hello\n\n";
    const frames = parseFrames(text);
    expect(frames).toHaveLength(1);
    expect(frames[0]?.event).toBeUndefined();
    expect(frames[0]?.id).toBeUndefined();
    expect(frames[0]?.data).toBe("hello");
  });

  it("parses multiple frames separated by blank lines", () => {
    const text =
      'event: message.delta\nid: 1\ndata: {"a":1}\n\n' + 'event: message.completed\nid: 2\ndata: {"b":2}\n\n';
    const frames = parseFrames(text);
    expect(frames).toHaveLength(2);
    expect(frames[0]?.event).toBe("message.delta");
    expect(frames[1]?.event).toBe("message.completed");
  });

  it("ignores a ping-only frame with no data", () => {
    // A keepalive may arrive as ': ping\n\n' — comment lines produce no frame.
    const text = ': ping\n\nevent: message.delta\nid: 1\ndata: {"x":1}\n\n';
    const frames = parseFrames(text);
    expect(frames).toHaveLength(1);
    expect(frames[0]?.event).toBe("message.delta");
  });

  it("handles frames without a trailing blank line (incomplete buffer)", () => {
    // A partial frame at the end of a chunk — must be ignored (caller buffers).
    const text = 'event: message.delta\nid: 1\ndata: {"x":1}';
    // No terminating blank line → no complete frame
    const frames = parseFrames(text);
    expect(frames).toHaveLength(0);
  });

  it("returns only complete frames from a mixed buffer", () => {
    const text = 'event: tool.started\nid: 1\ndata: {"a":1}\n\nevent: incomplete';
    const frames = parseFrames(text);
    expect(frames).toHaveLength(1);
    expect(frames[0]?.event).toBe("tool.started");
  });

  it("handles CRLF line endings", () => {
    const text = 'event: turn.failed\r\nid: 5\r\ndata: {"code":"infra"}\r\n\r\n';
    const frames = parseFrames(text);
    expect(frames).toHaveLength(1);
    expect(frames[0]?.event).toBe("turn.failed");
    expect(frames[0]?.data).toBe('{"code":"infra"}');
  });

  it("handles a frame where field value has a leading space stripped", () => {
    // SSE spec: "field: value" — a single space after the colon is stripped.
    const text = 'event: reminder.fired\ndata: {"x":1}\n\n';
    const frames = parseFrames(text);
    expect(frames[0]?.event).toBe("reminder.fired");
  });

  it("handles a frame with no space after colon", () => {
    // "event:name" — no space — value is "name"
    const text = 'event:tool.failed\ndata:{"y":2}\n\n';
    const frames = parseFrames(text);
    expect(frames[0]?.event).toBe("tool.failed");
    expect(frames[0]?.data).toBe('{"y":2}');
  });
});

// Satisfy the SseFrame type import so the compiler doesn't prune it. We verify the shape here to confirm the exported
// interface is correct.
describe("SseFrame type shape", () => {
  it("is assignable from a partial object", () => {
    const frame: SseFrame = { data: "x" };
    expect(frame.data).toBe("x");
    expect(frame.event).toBeUndefined();
    expect(frame.id).toBeUndefined();
  });
});
