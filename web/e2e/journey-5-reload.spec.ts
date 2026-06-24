/**
 * Journey 5 — Reload mid-turn loses nothing.
 *
 * Sends "tell me a story slowly" which triggers ~8 textDelta chunks each spaced 500 ms apart. After the first
 * partial text appears, the page is reloaded. After reload the full assistant text must still appear and the
 * user message must be present — no duplication or loss.
 */

import { test, expect } from "@playwright/test";
import { AUTH_STATE_FILE } from "./constants.js";
import { gotoConnected } from "./helpers.js";

test.use({ storageState: AUTH_STATE_FILE });

test("reload mid-turn: user message persists and full assistant text appears", async ({ page }) => {
  // SSE open before sending so the slow turn starts streaming live; the reload below then exercises reconnect +
  // history reconcile without losing the turn.
  await gotoConnected(page, "/");

  const composer = page.getByRole("textbox", { name: "Message…" });

  // ── Send the slow-streaming message ───────────────────────────────────────
  await composer.fill("tell me a story slowly");
  await page.getByRole("button", { name: "Send" }).click();

  // ── Wait for the first partial chunk ─────────────────────────────────────
  // The fixture emits "Once " (500 ms) then further chunks. Wait for "Once" to appear before reloading — this
  // ensures the turn has started.
  await expect(page.getByText("Once")).toBeVisible({ timeout: 10_000 });

  // ── Reload mid-stream ─────────────────────────────────────────────────────
  await page.reload();

  // ── After reload: user message is present ────────────────────────────────
  // The conversation history is loaded from the server (GET /conversations/{id}).
  await expect(page.getByText("tell me a story slowly")).toBeVisible({ timeout: 10_000 });

  // ── Full assistant text eventually appears ────────────────────────────────
  // The turn runs server-side regardless of the client connection. After reload the SSE reconnect + history
  // reconcile deliver the full completed message. The complete text is: "Once upon a time, in a land far away."
  await expect(page.getByText("Once upon a time, in a land far away.")).toBeVisible({ timeout: 20_000 });

  // ── No duplication ────────────────────────────────────────────────────────
  // Expect exactly one occurrence of the user message (no duplicate bubbles).
  await expect(page.getByText("tell me a story slowly")).toHaveCount(1);
});
