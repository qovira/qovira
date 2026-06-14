/**
 * Tests for the SW cache-routing decision helper.
 *
 * The helper is a pure function extracted from the service worker so it can be
 * unit-tested without the $service-worker ambient module or a real SW context.
 */

import { describe, expect, it } from "vitest";

import { shouldCache } from "./cache-routing.js";

const ORIGIN = "https://app.example.com";

describe("shouldCache", () => {
  // --- passthrough: non-GET methods ---
  it("returns false for POST requests", () => {
    expect(shouldCache(`${ORIGIN}/api/v1/foo`, "POST", ORIGIN)).toBe(false);
  });

  it("returns false for DELETE requests", () => {
    expect(shouldCache(`${ORIGIN}/api/v1/reminders/1`, "DELETE", ORIGIN)).toBe(false);
  });

  it("returns false for PATCH requests", () => {
    expect(shouldCache(`${ORIGIN}/api/v1/reminders/1`, "PATCH", ORIGIN)).toBe(false);
  });

  // --- passthrough: API paths ---
  it("returns false for /api/ paths (GET)", () => {
    expect(shouldCache(`${ORIGIN}/api/v1/me`, "GET", ORIGIN)).toBe(false);
  });

  it("returns false for /api/v1/conversations (GET)", () => {
    expect(shouldCache(`${ORIGIN}/api/v1/conversations`, "GET", ORIGIN)).toBe(false);
  });

  it("returns false for /api/ with trailing segments (GET)", () => {
    expect(shouldCache(`${ORIGIN}/api/v1/reminders/abc-123`, "GET", ORIGIN)).toBe(false);
  });

  // --- passthrough: SSE event stream ---
  it("returns false for /events (GET)", () => {
    expect(shouldCache(`${ORIGIN}/events`, "GET", ORIGIN)).toBe(false);
  });

  it("returns false for /events with query string (GET)", () => {
    expect(shouldCache(`${ORIGIN}/events?userId=1`, "GET", ORIGIN)).toBe(false);
  });

  // --- cacheable: app shell assets ---
  it("returns true for the root / (GET)", () => {
    expect(shouldCache(`${ORIGIN}/`, "GET", ORIGIN)).toBe(true);
  });

  it("returns true for index.html (GET)", () => {
    expect(shouldCache(`${ORIGIN}/index.html`, "GET", ORIGIN)).toBe(true);
  });

  it("returns true for _app/immutable JS chunks (GET)", () => {
    expect(shouldCache(`${ORIGIN}/_app/immutable/chunks/abc123.js`, "GET", ORIGIN)).toBe(true);
  });

  it("returns true for static assets like manifest (GET)", () => {
    expect(shouldCache(`${ORIGIN}/manifest.webmanifest`, "GET", ORIGIN)).toBe(true);
  });

  it("returns true for icon files (GET)", () => {
    expect(shouldCache(`${ORIGIN}/icon-192.png`, "GET", ORIGIN)).toBe(true);
  });

  // --- edge: cross-origin requests are not cached ---
  it("returns false for cross-origin requests", () => {
    expect(shouldCache("https://cdn.example.com/some-asset.js", "GET", ORIGIN)).toBe(false);
  });
});
