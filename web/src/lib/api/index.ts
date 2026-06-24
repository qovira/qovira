/**
 * Api — the single typed HTTP client for the Qovira SPA.
 *
 * Wraps openapi-fetch with:
 *   - credentials: "include" on every request (session cookie rides automatically)
 *   - CSRF-Token header on unsafe methods (POST/PATCH/DELETE) echoed from the
 *     qovira_csrf cookie (double-submit pattern)
 *   - application/problem+json responses parsed into a typed ProblemError
 *   - centralised 401 handling via a registrable onUnauthorized hook
 *
 * Usage:
 *   import { Api, ProblemError, onUnauthorized } from "$lib/api";
 *
 *   // Wire the 401 seam (done once, in the session slice)
 *   onUnauthorized(() => { clearSession(); goto("/login"); });
 *
 *   const { data, error } = await Api.GET("/me", {});
 *   if (error instanceof ProblemError) { … error.code … }
 */

import createClient, { type Client, type Middleware } from "openapi-fetch";

import type { paths } from "./schema.d.ts";

// ---------------------------------------------------------------------------
// ProblemError — typed RFC 9457 error subclass
// ---------------------------------------------------------------------------

export interface FieldError {
  pointer: string;
  detail: string;
}

/** Shape of a raw problem+json body from the server (before wrapping). */
interface RawProblem {
  type: string;
  title: string;
  status: number;
  detail: string;
  code: string;
  requestId: string;
  errors?: FieldError[];
}

/**
 * Typed subclass of Error carrying an RFC 9457 problem+json body.
 *
 * Callers branch on `code` (the stable snake_case slug), not `status`.
 * `errors` is present only on 422 validation responses.
 */
export class ProblemError extends Error {
  override readonly name = "ProblemError";
  readonly type: string;
  readonly title: string;
  readonly status: number;
  readonly detail: string;
  readonly code: string;
  readonly requestId: string;
  readonly errors: FieldError[] | undefined;

  constructor(raw: RawProblem) {
    super(`${raw.code}: ${raw.detail}`);
    this.type = raw.type;
    this.title = raw.title;
    this.status = raw.status;
    this.detail = raw.detail;
    this.code = raw.code;
    this.requestId = raw.requestId;
    this.errors = raw.errors;
  }
}

// ---------------------------------------------------------------------------
// 401 handler seam
// ---------------------------------------------------------------------------

/** The currently registered unauthorised callback. Default is a no-op. */
let unauthorizedHandler: (() => void) | (() => Promise<void>) = () => {
  // no-op until wired by session/SSE slices
};

/**
 * Register the centralised 401 handler. Call this once from the session slice.
 *
 * Only one handler can be active at a time; calling again replaces the previous
 * one. The handler is the sole authority for "force-login": clear session state,
 * tear down SSE, redirect to /login. Session and SSE slices subscribe here;
 * they do NOT call each other.
 */
export function onUnauthorized(cb: (() => void) | (() => Promise<void>)): void {
  unauthorizedHandler = cb;
}

/**
 * Invoke the currently registered unauthorised handler.
 *
 * Call this from any code path that detects a 401 but bypasses the
 * openapi-fetch middleware (e.g. the bare fetch("/events") SSE stream).
 * This is the single seam that ensures every 401 — REST or SSE — triggers
 * the same teardown: notifyTearDown → resetSession → redirect to /login.
 */
export function callUnauthorizedHandler(): void | Promise<void> {
  return unauthorizedHandler();
}

// ---------------------------------------------------------------------------
// Cookie helper
// ---------------------------------------------------------------------------

/** Read a cookie value by name from document.cookie. Returns null if absent. */
function readCookie(name: string): string | null {
  if (typeof document === "undefined") return null;
  const prefix = `${name}=`;
  for (const part of document.cookie.split(";")) {
    const trimmed = part.trimStart();
    if (trimmed.startsWith(prefix)) {
      return trimmed.slice(prefix.length);
    }
  }
  return null;
}

/** Methods that require a CSRF-Token header (cookie-authenticated double-submit). */
const UNSAFE_METHODS = new Set(["POST", "PATCH", "DELETE"]);

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

/**
 * Middleware that:
 *  1. Clones the request with credentials: "include" (same-origin cookie auth)
 *  2. Adds CSRF-Token on POST/PATCH/DELETE from the qovira_csrf cookie
 *  3. On non-2xx application/problem+json response, throws a ProblemError
 *  4. On 401, invokes the registered onUnauthorized handler
 */
const qoviraMiddleware: Middleware = {
  onRequest({ request }) {
    const headers = new Headers(request.headers);

    // 2. CSRF double-submit: add header on unsafe methods from the readable cookie.
    if (UNSAFE_METHODS.has(request.method)) {
      const csrf = readCookie("qovira_csrf");
      if (csrf !== null) {
        headers.set("CSRF-Token", csrf);
      }
    }

    // 1. credentials: "include" + any header mutations are applied by re-cloning.
    return new Request(request, { credentials: "include", headers });
  },

  async onResponse({ response }) {
    if (response.ok) return; // 2xx — nothing to do

    // Detect problem+json by Content-Type.
    const ct = response.headers.get("Content-Type") ?? "";
    const isProblem = ct.includes("application/problem+json");

    if (isProblem) {
      // Clone before reading so the body stream isn't consumed for callers that
      // also need to inspect the response.
      const raw: unknown = await response.clone().json();

      if (isProblemShape(raw)) {
        // 4. Invoke the 401 handler before throwing so the session is torn down
        //    before the caller gets the ProblemError.
        if (response.status === 401) {
          await Promise.resolve(unauthorizedHandler());
        }

        throw new ProblemError(raw);
      }
    }

    // Non-problem non-2xx falls through to openapi-fetch's default error handling
    // (returns { error: parsedBody, response }).
  },
};

// ---------------------------------------------------------------------------
// Type guard
// ---------------------------------------------------------------------------

function isProblemShape(v: unknown): v is RawProblem {
  if (typeof v !== "object" || v === null) return false;
  const p = v as Record<string, unknown>;
  return (
    typeof p.type === "string" &&
    typeof p.title === "string" &&
    typeof p.status === "number" &&
    typeof p.detail === "string" &&
    typeof p.code === "string" &&
    typeof p.requestId === "string"
  );
}

// ---------------------------------------------------------------------------
// Client construction
// ---------------------------------------------------------------------------

/**
 * Internal openapi-fetch client. Not exported — callers always go through the
 * wrapped `Api` (which converts thrown ProblemErrors into { error, response }).
 *
 * The `fetch` wrapper reads `globalThis.fetch` at call time so that tests can
 * stub `fetch` via `vi.stubGlobal` after the module is loaded.
 */
const _client: Client<paths> = createClient<paths>({
  baseUrl: "/api/v1",
  fetch: (req) => globalThis.fetch(req),
});

_client.use(qoviraMiddleware);

// ---------------------------------------------------------------------------
// Wrapped client — converts thrown ProblemErrors to { error, response }
// ---------------------------------------------------------------------------

/**
 * Wrap a single openapi-fetch method call so that a ProblemError thrown from
 * middleware surfaces as `{ error: ProblemError, response: undefined }` rather
 * than as an unhandled rejection, keeping the same call-site shape as a
 * successful `{ data, response }`.
 */
async function wrap<T>(
  fn: () => Promise<T>,
): Promise<T | { data: undefined; error: ProblemError; response: undefined }> {
  try {
    return await fn();
  } catch (e) {
    if (e instanceof ProblemError) {
      return { data: undefined, error: e, response: undefined };
    }
    throw e;
  }
}

// Helper to cast an openapi-fetch method through `wrap` while preserving the
// full generic ClientMethod signature. The cast is safe: at runtime the delegate
// IS the underlying method; the only difference is that thrown ProblemErrors are
// converted to `{ error, response }` instead of propagating. The complex
// overloaded generic `ClientMethod` type cannot be expressed without `as unknown`.
function wrapMethod<M extends (...args: [unknown, ...unknown[]]) => Promise<unknown>>(method: M): M {
  return ((...args: [unknown, ...unknown[]]) => wrap(() => method(...args))) as M;
}

/**
 * The Qovira typed API client.
 *
 * Each method mirrors the openapi-fetch surface but:
 *  - always sends credentials: "include"
 *  - adds CSRF-Token on POST/PATCH/DELETE
 *  - returns problem+json responses as `{ error: ProblemError }`
 *  - triggers the onUnauthorized hook on 401 before returning
 *
 * Do NOT use the internal `_client` directly — always import `Api`.
 */
export const Api: Client<paths> = {
  ..._client,
  GET: wrapMethod(_client.GET as (...args: [unknown, ...unknown[]]) => Promise<unknown>) as Client<paths>["GET"],
  POST: wrapMethod(_client.POST as (...args: [unknown, ...unknown[]]) => Promise<unknown>) as Client<paths>["POST"],
  PATCH: wrapMethod(_client.PATCH as (...args: [unknown, ...unknown[]]) => Promise<unknown>) as Client<paths>["PATCH"],
  DELETE: wrapMethod(
    _client.DELETE as (...args: [unknown, ...unknown[]]) => Promise<unknown>,
  ) as Client<paths>["DELETE"],
  PUT: wrapMethod(_client.PUT as (...args: [unknown, ...unknown[]]) => Promise<unknown>) as Client<paths>["PUT"],
};
