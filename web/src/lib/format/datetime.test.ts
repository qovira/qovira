// Tests for the shared formatDueAt datetime helper.
// Environment: browser (happy-dom) — included via src/lib/**/*.test.ts pattern.
import { describe, expect, it } from "vitest";

import { formatDueAt } from "./datetime.js";

// ---------------------------------------------------------------------------
// formatDueAt
// ---------------------------------------------------------------------------

describe("formatDueAt()", () => {
  it("returns an empty string for null", () => {
    expect(formatDueAt(null)).toBe("");
  });

  it("returns an empty string for undefined", () => {
    expect(formatDueAt(undefined)).toBe("");
  });

  it("returns an empty string for an empty string", () => {
    expect(formatDueAt("")).toBe("");
  });

  it("returns a non-empty formatted string for a valid ISO 8601 date", () => {
    const result = formatDueAt("2030-01-15T09:00:00Z");
    expect(result).not.toBe("");
    // Must not return the raw ISO string.
    expect(result).not.toBe("2030-01-15T09:00:00Z");
    // Must not contain the ISO 'T' separator or 'Z' suffix.
    expect(result).not.toMatch(/\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z/);
  });

  it("falls back to the raw string for an unparseable value", () => {
    const bad = "not-a-date";
    const result = formatDueAt(bad);
    expect(result).toBe(bad);
  });

  it.each([
    ["garbage"],
    ["2030-01-15T09:00:00Z"],
    ["2030-13-99T99:99:99Z"], // invalid date fields
    [""],
    ["null"],
    ["undefined"],
  ])("never throws for string input %s", (input) => {
    expect(() => formatDueAt(input)).not.toThrow();
  });
});
