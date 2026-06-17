/**
 * Journey 3 — Destructive → confirm → approve → really deleted.
 *
 * Two user messages in one conversation:
 *   Msg 1 "remind me about the dentist tomorrow" → creates a "Dentist" reminder.
 *   Msg 2 "delete the dentist reminder" → emits delete_reminder (RiskDestructive)
 *          → confirmation card renders → click Approve → reminder gone from /reminders.
 */

import { test, expect } from "@playwright/test";
import { AUTH_STATE_FILE } from "./constants.js";
import { gotoConnected } from "./helpers.js";

test.use({ storageState: AUTH_STATE_FILE });

test("destructive tool → confirmation card → approve → reminder deleted", async ({ page }) => {
  // SSE open before sending — both turns (create, then delete) stream live.
  await gotoConnected(page, "/");

  const composer = page.getByRole("textbox", { name: "Message…" });

  // ── Msg 1: create the dentist reminder ───────────────────────────────────
  await composer.fill("remind me about the dentist tomorrow");
  await page.getByRole("button", { name: "Send" }).click();

  // Wait for the narration text from round 1 to confirm the turn is complete.
  await expect(page.getByText("Your dentist reminder is set.")).toBeVisible({ timeout: 15_000 });

  // ── Msg 2: request deletion ───────────────────────────────────────────────
  await composer.fill("delete the dentist reminder");
  await page.getByRole("button", { name: "Send" }).click();

  // ── Confirmation card appears ─────────────────────────────────────────────
  // delete_reminder is RiskDestructive → the harness suspends the turn and
  // emits a confirmation.required event → ConfirmationCard renders with
  // Approve / Deny buttons.
  const approveBtn = page.getByRole("button", { name: "Approve" });
  await expect(approveBtn).toBeVisible({ timeout: 15_000 });
  await expect(page.getByRole("button", { name: "Deny" })).toBeVisible();

  // ── Click Approve ─────────────────────────────────────────────────────────
  await approveBtn.click();

  // ── Navigate to reminders and assert "Dentist" is gone ───────────────────
  // Wait for the post-approval narration to arrive (round 1 of the delete rule).
  await expect(page.getByText("deleted the dentist reminder")).toBeVisible({ timeout: 15_000 });

  await page.goto("/reminders");

  // Load completed section in case it ended up there — first ensure active
  // buckets do NOT show "Dentist".
  // The item was deleted (not completed), so it should not appear anywhere.
  await expect(page.getByText("Dentist")).not.toBeVisible({ timeout: 5_000 });
});
