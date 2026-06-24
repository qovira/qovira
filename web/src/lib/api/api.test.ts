/**
 * Tests for the Api wrapper: CSRF echo, problem+json parsing, and 401 hook.
 *
 * The environment is happy-dom (set via vitest project config) so document.cookie and globalThis.fetch can be
 * set/mocked directly.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// Import once at module level — the module uses `globalThis.fetch` at call time (not at createClient time), so
// vi.stubGlobal works after the import.
import { Api, ProblemError, onUnauthorized } from "./index.js";

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/** Build a minimal problem+json body matching the Qovira Problem shape. */
function makeProblemBody(overrides: Partial<ProblemBody> = {}): ProblemBody {
  return {
    type: "https://qovira.ai/errors/not_found",
    title: "Not found",
    status: 404,
    detail: "Resource not found",
    code: "not_found",
    requestId: "test-req-id",
    ...overrides,
  };
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

/** A Response with Content-Type: application/problem+json and the given body. */
function problemResponse(body: ProblemBody, status = body.status): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });
}

/** A successful 200 Response with a JSON body. */
function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

// ---------------------------------------------------------------------------
// Cookie helpers (document.cookie in happy-dom is functional)
// ---------------------------------------------------------------------------

function setCsrfCookie(value: string): void {
  document.cookie = `qovira_csrf=${value}; path=/`;
}

function clearCsrfCookie(): void {
  document.cookie = `qovira_csrf=; expires=Thu, 01 Jan 1970 00:00:00 GMT; path=/`;
}

// ---------------------------------------------------------------------------
// Suite
// ---------------------------------------------------------------------------

beforeEach(() => {
  // Reset the 401 handler to a no-op before each test.
  onUnauthorized(() => {
    /* no-op */
  });
});

afterEach(() => {
  vi.restoreAllMocks();
  clearCsrfCookie();
});

// -------------------------------------------------------------------------
// AC1 — credentials: "include" on every request
// -------------------------------------------------------------------------
describe("credentials", () => {
  it("sends credentials:include on GET requests", async () => {
    const fetchSpy = vi.fn().mockResolvedValue(jsonResponse({ user: { id: "u1" } }));
    vi.stubGlobal("fetch", fetchSpy);

    await Api.GET("/me", {});

    const request = fetchSpy.mock.calls[0]?.[0] as Request;
    expect(request).toBeDefined();
    expect(request.credentials).toBe("include");
  });
});

// -------------------------------------------------------------------------
// AC2 — CSRF header: present on POST/PATCH/DELETE, absent on GET
// -------------------------------------------------------------------------
describe("CSRF header", () => {
  it("sends CSRF-Token header on POST from qovira_csrf cookie", async () => {
    setCsrfCookie("csrf-token-value-abc");
    const fetchSpy = vi.fn().mockResolvedValue(jsonResponse({ expiresAt: "2030-01-01T00:00:00Z", user: { id: "u1" } }));
    vi.stubGlobal("fetch", fetchSpy);

    await Api.POST("/auth/login", {
      body: { email: "test@example.com", password: "pass123" },
    });

    const request = fetchSpy.mock.calls[0]?.[0] as Request;
    expect(request).toBeDefined();
    expect(request.headers.get("CSRF-Token")).toBe("csrf-token-value-abc");
  });

  it("sends CSRF-Token header on PATCH from qovira_csrf cookie", async () => {
    setCsrfCookie("csrf-patch-value");
    const fetchSpy = vi.fn().mockResolvedValue(jsonResponse({ user: { id: "u1" } }));
    vi.stubGlobal("fetch", fetchSpy);

    await Api.PATCH("/me", {
      body: { displayName: "New Name" },
    });

    const request = fetchSpy.mock.calls[0]?.[0] as Request;
    expect(request).toBeDefined();
    expect(request.headers.get("CSRF-Token")).toBe("csrf-patch-value");
  });

  it("sends CSRF-Token header on DELETE from qovira_csrf cookie", async () => {
    setCsrfCookie("csrf-delete-value");
    const fetchSpy = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchSpy);

    await Api.DELETE("/auth/session", {});

    const request = fetchSpy.mock.calls[0]?.[0] as Request;
    expect(request).toBeDefined();
    expect(request.headers.get("CSRF-Token")).toBe("csrf-delete-value");
  });

  it("does NOT send CSRF-Token header on GET", async () => {
    setCsrfCookie("csrf-should-not-appear");
    const fetchSpy = vi.fn().mockResolvedValue(jsonResponse({ user: { id: "u1" } }));
    vi.stubGlobal("fetch", fetchSpy);

    await Api.GET("/me", {});

    const request = fetchSpy.mock.calls[0]?.[0] as Request;
    expect(request).toBeDefined();
    expect(request.headers.get("CSRF-Token")).toBeNull();
  });

  it("does NOT send CSRF-Token on POST when qovira_csrf cookie is absent", async () => {
    clearCsrfCookie();
    const fetchSpy = vi.fn().mockResolvedValue(jsonResponse({ expiresAt: "2030-01-01T00:00:00Z", user: { id: "u1" } }));
    vi.stubGlobal("fetch", fetchSpy);

    await Api.POST("/auth/login", {
      body: { email: "a@b.com", password: "p" },
    });

    const request = fetchSpy.mock.calls[0]?.[0] as Request;
    expect(request).toBeDefined();
    expect(request.headers.get("CSRF-Token")).toBeNull();
  });
});

// -------------------------------------------------------------------------
// AC3 — problem+json → ProblemError
// -------------------------------------------------------------------------
describe("problem+json parsing", () => {
  it("surfaces a 404 problem+json as a ProblemError with all fields", async () => {
    const body = makeProblemBody({
      type: "https://qovira.ai/errors/reminder_not_found",
      title: "Not found",
      status: 404,
      detail: "The reminder was not found.",
      code: "reminder_not_found",
      requestId: "req-abc-123",
    });
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(problemResponse(body)));

    const result = await Api.GET("/reminders/{id}", {
      params: { path: { id: "missing-id" } },
    });

    expect(result.error).toBeInstanceOf(ProblemError);
    const err = result.error as ProblemError;
    expect(err.code).toBe("reminder_not_found");
    expect(err.status).toBe(404);
    expect(err.detail).toBe("The reminder was not found.");
    expect(err.requestId).toBe("req-abc-123");
    expect(err.type).toBe("https://qovira.ai/errors/reminder_not_found");
    expect(err.title).toBe("Not found");
    expect(err.message).toContain("reminder_not_found");
  });

  it("includes errors[] on a 422 validation error", async () => {
    const body = makeProblemBody({
      type: "https://qovira.ai/errors/validation_error",
      title: "Request validation failed",
      status: 422,
      detail: "Validation failed",
      code: "validation_error",
      requestId: "req-422",
      errors: [
        { pointer: "/email", detail: "must be a valid email" },
        { pointer: "/password", detail: "must be at least 8 characters" },
      ],
    });
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(problemResponse(body, 422)));

    const result = await Api.POST("/auth/login", {
      body: { email: "bad", password: "x" },
    });

    expect(result.error).toBeInstanceOf(ProblemError);
    const err = result.error as ProblemError;
    expect(err.status).toBe(422);
    expect(err.errors).toHaveLength(2);
    expect(err.errors?.[0]).toEqual({ pointer: "/email", detail: "must be a valid email" });
  });

  it("is an instance of Error and carries the code in the message", async () => {
    const body = makeProblemBody({
      code: "internal_error",
      status: 500,
      type: "https://qovira.ai/errors/internal_error",
      title: "Internal server error",
      detail: "An unexpected error occurred.",
      requestId: "req-500",
    });
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(problemResponse(body)));

    const result = await Api.GET("/me", {});

    const err = result.error as ProblemError;
    expect(err).toBeInstanceOf(Error);
    expect(err.name).toBe("ProblemError");
    expect(err.message).toContain("internal_error");
  });

  it("returns { data: undefined } without ProblemError for non-problem non-2xx", async () => {
    // A non-2xx application/json (no problem+json content type)
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ message: "some error" }), {
          status: 500,
          headers: { "Content-Type": "application/json" },
        }),
      ),
    );

    const result = await Api.GET("/me", {});
    // openapi-fetch puts the raw body as `error` for non-2xx non-problem responses
    expect(result.data).toBeUndefined();
    // The raw error (not a ProblemError) is in result.error
    expect(result.error).not.toBeInstanceOf(ProblemError);
  });

  it("does NOT produce a ProblemError when Content-Type is problem+json but body is a valid JSON non-problem shape", async () => {
    // A response with Content-Type: application/problem+json but a body that does NOT satisfy isProblemShape (missing
    // required fields). The middleware must fall through to openapi-fetch's default handling rather than crashing or
    // producing a malformed ProblemError.
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ message: "oops", code: 500 }), {
          status: 500,
          headers: { "Content-Type": "application/problem+json" },
        }),
      ),
    );

    const result = await Api.GET("/me", {});
    expect(result.data).toBeUndefined();
    // The malformed problem+json body does not become a ProblemError — it falls through and openapi-fetch returns it
    // as the generic `error` field.
    expect(result.error).not.toBeInstanceOf(ProblemError);
  });

  it("does NOT produce a ProblemError when Content-Type is problem+json but body is empty / non-JSON", async () => {
    // A null body with problem+json Content-Type. The qovira middleware detects the problem+json Content-Type and
    // calls response.clone().json() to inspect the body. In this runtime (happy-dom) that throws a SyntaxError on a
    // null body. Since wrap() only catches ProblemError, the SyntaxError propagates. The key invariant: the thrown
    // error must NOT be a ProblemError (the middleware must not wrap a parse failure as if it were a valid problem
    // body).
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(null, {
          status: 500,
          headers: { "Content-Type": "application/problem+json" },
        }),
      ),
    );

    await expect(Api.GET("/me", {})).rejects.not.toBeInstanceOf(ProblemError);
  });
});

// -------------------------------------------------------------------------
// AC4 — 401 triggers the central handler
// -------------------------------------------------------------------------
describe("401 handler", () => {
  it("invokes the registered onUnauthorized callback on a 401 response", async () => {
    const handler = vi.fn();
    onUnauthorized(handler);

    const body = makeProblemBody({
      type: "https://qovira.ai/errors/unauthenticated",
      title: "Authentication required",
      status: 401,
      detail: "Not authenticated",
      code: "unauthenticated",
      requestId: "req-401",
    });
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(problemResponse(body, 401)));

    await Api.GET("/me", {});

    expect(handler).toHaveBeenCalledOnce();
  });

  it("does NOT invoke the 401 handler on a 200 response", async () => {
    const handler = vi.fn();
    onUnauthorized(handler);

    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse({ user: { id: "u1" } })));

    await Api.GET("/me", {});

    expect(handler).not.toHaveBeenCalled();
  });

  it("does NOT invoke the 401 handler on a 403 response", async () => {
    const handler = vi.fn();
    onUnauthorized(handler);

    const body = makeProblemBody({
      status: 403,
      code: "csrf_failed",
      type: "https://qovira.ai/errors/csrf_failed",
      title: "CSRF validation failed",
      detail: "CSRF token mismatch",
      requestId: "req-403",
    });
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(problemResponse(body, 403)));

    await Api.GET("/me", {});

    expect(handler).not.toHaveBeenCalled();
  });

  it("supports replacing the handler: only the latest registration fires", async () => {
    const handler1 = vi.fn();
    const handler2 = vi.fn();
    onUnauthorized(handler1);
    onUnauthorized(handler2);

    const body = makeProblemBody({
      status: 401,
      code: "unauthenticated",
      type: "https://qovira.ai/errors/unauthenticated",
      title: "Authentication required",
      detail: "Not authenticated",
      requestId: "req-401-b",
    });
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(problemResponse(body, 401)));

    await Api.GET("/me", {});

    expect(handler2).toHaveBeenCalledOnce();
    expect(handler1).not.toHaveBeenCalled();
  });
});
