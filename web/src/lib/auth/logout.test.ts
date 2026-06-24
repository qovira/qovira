// Tests for the logout flow.
//
// Environment: browser (happy-dom) — openapi-fetch needs a location origin.
// The session store and $app/navigation are mocked; this suite verifies that
// logout issues the DELETE, tears down, resets the store, and navigates — even
// when the session is already revoked.
import { afterEach, describe, expect, it, vi } from "vitest";

const { notifyTearDown, resetSession } = vi.hoisted(() => ({
  notifyTearDown: vi.fn(),
  resetSession: vi.fn(),
}));

vi.mock("$lib/stores/session.svelte.js", () => ({ notifyTearDown, resetSession }));
vi.mock("$app/navigation", () => ({ goto: vi.fn() }));

import { goto } from "$app/navigation";

import { logout } from "./logout.js";

interface ProblemBody {
  type: string;
  title: string;
  status: number;
  detail: string;
  code: string;
  requestId: string;
}

function problemResponse(body: ProblemBody): Response {
  return new Response(JSON.stringify(body), {
    status: body.status,
    headers: { "Content-Type": "application/problem+json" },
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.clearAllMocks();
});

describe("logout", () => {
  it("issues DELETE /auth/session, tears down, resets the store, and navigates to /login", async () => {
    let capturedMethod: string | undefined;
    let capturedUrl: string | undefined;
    const fetchMock = vi.fn<(req: Request) => Promise<Response>>((req) => {
      capturedMethod = req.method;
      capturedUrl = req.url;
      return Promise.resolve(new Response(null, { status: 204 }));
    });
    vi.stubGlobal("fetch", fetchMock);

    await logout();

    expect(capturedMethod).toBe("DELETE");
    expect(capturedUrl).toContain("/auth/session");
    expect(notifyTearDown).toHaveBeenCalledTimes(1);
    expect(resetSession).toHaveBeenCalledTimes(1);
    expect(goto).toHaveBeenCalledWith("/login");
    // Teardown order: tear down SSE before clearing local state.
    const tearDownOrder = notifyTearDown.mock.invocationCallOrder[0] ?? 0;
    const resetOrder = resetSession.mock.invocationCallOrder[0] ?? 0;
    expect(tearDownOrder).toBeLessThan(resetOrder);
  });

  it("completes the client-side teardown even when the session is already revoked (401)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() =>
        Promise.resolve(
          problemResponse({
            type: "https://qovira.ai/errors/unauthorized",
            title: "Unauthorized",
            status: 401,
            detail: "no session",
            code: "unauthorized",
            requestId: "r1",
          }),
        ),
      ),
    );

    await logout();

    expect(notifyTearDown).toHaveBeenCalledTimes(1);
    expect(resetSession).toHaveBeenCalledTimes(1);
    expect(goto).toHaveBeenCalledWith("/login");
  });

  it("completes the client-side teardown (notifyTearDown + resetSession + goto) even when the DELETE network call throws", async () => {
    // Simulate a hard network failure — fetch rejects entirely (offline / DNS failure).
    // Without try/finally, the teardown steps after the await would be skipped,
    // leaving the SSE connection open and the session store in authenticated state.
    vi.stubGlobal(
      "fetch",
      vi.fn(() => Promise.reject(new TypeError("Failed to fetch"))),
    );

    // logout() should not propagate the network throw — it wraps the call in try/finally.
    await logout();

    // All local teardown steps must have run despite the network throw.
    expect(notifyTearDown).toHaveBeenCalledTimes(1);
    expect(resetSession).toHaveBeenCalledTimes(1);
    expect(goto).toHaveBeenCalledWith("/login");
  });

  it("network throw: teardown order is preserved — notifyTearDown fires before resetSession", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => Promise.reject(new TypeError("Network unavailable"))),
    );

    await logout();

    const tearDownOrder = notifyTearDown.mock.invocationCallOrder[0] ?? 0;
    const resetOrder = resetSession.mock.invocationCallOrder[0] ?? 0;
    expect(tearDownOrder).toBeLessThan(resetOrder);
  });
});
