/**
 * Journey 2 — Message → streaming → tool card → click through.
 *
 * Sends "remind me to buy milk tomorrow at 9am"; asserts:
 *   - Streaming assistant text appears while the turn is in flight.
 *   - A tool chip resolves to a "Reminder: Buy milk" entity card link.
 *   - Clicking the entity card navigates to /reminders with the row present.
 */

import { test, expect } from "@playwright/test";
import { AUTH_STATE_FILE } from "./constants.js";
import { gotoConnected } from "./helpers.js";

test("streaming → tool chip → entity card → reminders view", async ({ page }) => {
  // Land on the chat surface with the SSE connection open before sending, so the turn's streaming deltas and
  // tool events are not raced (see gotoConnected).
  await gotoConnected(page, "/");

  // ── Send message ─────────────────────────────────────────────────────────
  const composer = page.getByRole("textbox", { name: "Message…" });
  await composer.fill("remind me to buy milk tomorrow at 9am");
  await page.getByRole("button", { name: "Send" }).click();

  // ── Streaming text appears ────────────────────────────────────────────────
  // The in-flight assistant bubble renders streaming text (textDelta chunks). Wait for any partial text to
  // appear before asserting the entity card.
  await expect(page.getByText("Sure, I'll")).toBeVisible({ timeout: 15_000 });

  // ── Entity card appears ───────────────────────────────────────────────────
  // After the turn completes the tool chip resolves to an <a> entity card with aria-label "Reminder: Buy milk".
  // The label pattern matches the ToolCallChip component: "{entity_type}: {title}".
  const entityCard = page.getByRole("link", { name: /Reminder.*Buy milk/i });
  await expect(entityCard).toBeVisible({ timeout: 15_000 });

  // ── Click through to /reminders ───────────────────────────────────────────
  await entityCard.click();
  await expect(page).toHaveURL("/reminders");

  // The "Buy milk" row is present in the reminders view.
  await expect(page.getByText("Buy milk")).toBeVisible({ timeout: 10_000 });
});

// Wire storageState so this spec starts authenticated.
test.use({ storageState: AUTH_STATE_FILE });
