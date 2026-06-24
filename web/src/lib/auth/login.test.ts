// Tests for the login flow core (performLogin).
//
// Environment: browser (happy-dom) — openapi-fetch needs a location origin to
// build the request URL, and document.cookie for the CSRF read. The session
// store is mocked so this suite exercises performLogin's contract (what it
// calls, what it returns) without the $state runtime; the store has its own
// rune tests.
import { afterEach, describe, expect, it, vi } from "vitest";

const { seedSession, notifySessionReady } = vi.hoisted(() => ({
  seedSession: vi.fn(),
  notifySessionReady: vi.fn(),
}));

vi.mock("$lib/stores/session.svelte.js", () => ({ seedSession, notifySessionReady }));

import { performLogin } from "./login.js";

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

const USER = {
  id: "u1",
  email: "alice@example.com",
  displayName: "Alice",
  role: "user",
  timezone: "UTC",
  locale: "en-US",
  language: "en",
};

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

interface ProblemBody {
  type: string;
  title: string;
  status: number;
  detail: string;
  code: string;
  requestId: string;
  errors?: { pointer: string; detail: string }[];
}

function problemResponse(body: ProblemBody): Response {
  return new Response(JSON.stringify(body), {
    status: body.status,
    headers: { "Content-Type": "application/problem+json" },
  });
}

/** Stub globalThis.fetch, routing by URL substring (first match wins). */
function stubFetch(routes: [string, () => Response][]): void {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: Request | string) => {
      const url = typeof input === "string" ? input : input.url;
      for (const [match, make] of routes) {
        if (url.includes(match)) {
          return Promise.resolve(make());
        }
      }
      return Promise.reject(new Error(`unexpected fetch: ${url}`));
    }),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.clearAllMocks();
});

// ---------------------------------------------------------------------------
// Success
// ---------------------------------------------------------------------------

describe("performLogin — success", () => {
  it("seeds the session with the real login expiry, fires session-ready, returns ok", async () => {
    stubFetch([
      ["/auth/login", () => jsonResponse({ expiresAt: "2030-01-01T00:00:00Z", user: USER })],
      ["/me", () => jsonResponse({ user: USER })],
    ]);

    const result = await performLogin("alice@example.com", "pw");

    expect(result).toEqual({ ok: true });
    // The expiry comes from the login body (the real value), never fabricated.
    expect(seedSession).toHaveBeenCalledWith({ user: USER, expiresAt: "2030-01-01T00:00:00Z" });
    expect(notifySessionReady).toHaveBeenCalledTimes(1);
  });
});

// ---------------------------------------------------------------------------
// Failures — never seed, never signal session-ready
// ---------------------------------------------------------------------------

describe("performLogin — failures", () => {
  it("maps a 401 to the uniform invalid-credentials message", async () => {
    stubFetch([
      [
        "/auth/login",
        () =>
          problemResponse({
            type: "https://qovira.ai/errors/invalid_credentials",
            title: "Unauthorized",
            status: 401,
            detail: "server detail that must not leak",
            code: "invalid_credentials",
            requestId: "r1",
          }),
      ],
    ]);

    const result = await performLogin("alice@example.com", "wrong");

    expect(result).toEqual({ ok: false, message: "Invalid email or password." });
    expect(seedSession).not.toHaveBeenCalled();
    expect(notifySessionReady).not.toHaveBeenCalled();
  });

  it("maps 422 validation errors to field errors keyed by JSON Pointer", async () => {
    stubFetch([
      [
        "/auth/login",
        () =>
          problemResponse({
            type: "https://qovira.ai/errors/validation_error",
            title: "Unprocessable Entity",
            status: 422,
            detail: "Validation failed",
            code: "validation_error",
            requestId: "r2",
            errors: [{ pointer: "/email", detail: "Enter a valid email address." }],
          }),
      ],
    ]);

    const result = await performLogin("not-an-email", "pw");

    expect(result).toEqual({ ok: false, fieldErrors: { email: "Enter a valid email address." } });
    expect(seedSession).not.toHaveBeenCalled();
  });

  it("returns a verify message when login succeeds but the /me probe fails", async () => {
    stubFetch([
      ["/auth/login", () => jsonResponse({ expiresAt: "2030-01-01T00:00:00Z", user: USER })],
      [
        "/me",
        () =>
          problemResponse({
            type: "https://qovira.ai/errors/unauthorized",
            title: "Unauthorized",
            status: 401,
            detail: "no session",
            code: "unauthorized",
            requestId: "r3",
          }),
      ],
    ]);

    const result = await performLogin("alice@example.com", "pw");

    expect(result).toEqual({
      ok: false,
      message: "Login succeeded but the session could not be verified. Please try again.",
    });
    expect(seedSession).not.toHaveBeenCalled();
  });

  it("propagates a raw network failure so the caller can show a generic error", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => Promise.reject(new TypeError("network down"))),
    );

    await expect(performLogin("alice@example.com", "pw")).rejects.toThrow();
    expect(seedSession).not.toHaveBeenCalled();
  });
});
