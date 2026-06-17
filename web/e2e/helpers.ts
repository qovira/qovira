/**
 * Shared E2E helpers. A non-test file (does not match the projects' testMatch),
 * so journey specs may import from it freely.
 */

import type { Page } from "@playwright/test";

/**
 * Navigate to `path` and resolve only once the persistent SSE connection
 * (`GET /events`) is established.
 *
 * The chat surface streams assistant text deltas and tool-call lifecycle events
 * live over this single connection, opened in the root layout's onMount. Those
 * events are live-only: a message POSTed before the connection is open races the
 * turn and silently drops whatever the server emits in the gap. Waiting for the
 * `/events` response — which resolves on the long-lived stream's headers (status
 * 200), i.e. the moment the server accepts the subscription — closes that race
 * deterministically, with no arbitrary timeout.
 *
 * The wait is armed before goto so a fast connection cannot resolve before we
 * start listening.
 */
export async function gotoConnected(page: Page, path: string): Promise<void> {
  const sseConnected = page.waitForResponse((r) => r.url().includes("/events") && r.status() === 200);
  await page.goto(path);
  await sseConnected;
}
