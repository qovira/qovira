/**
 * Journey 4 — Create/complete/edit live in both surfaces.
 *
 * Sends "remind me to call mom tonight" → create_reminder via chat.
 * Asserts the reminder appears live in /reminders (no reload).
 * Then completes the reminder via the UI (Mark as complete → Done section).
 * Then edits a reminder's title via the Edit sheet and confirms the change.
 */

import { test, expect } from "@playwright/test";
import { AUTH_STATE_FILE } from "./constants.js";
import { gotoConnected } from "./helpers.js";

test.use({ storageState: AUTH_STATE_FILE });

test("chat create → live in reminders → complete → edit", async ({ page }) => {
  // SSE open before sending so the create turn streams live.
  await gotoConnected(page, "/");

  const composer = page.getByRole("textbox", { name: "Message…" });

  // ── Send message ──────────────────────────────────────────────────────────
  await composer.fill("remind me to call mom tonight");
  await page.getByRole("button", { name: "Send" }).click();

  // Wait for the turn to complete (round-1 narration).
  await expect(page.getByText("I've added a reminder to call mom tonight")).toBeVisible({ timeout: 15_000 });

  // ── Navigate to /reminders — live without reload ──────────────────────────
  await page.getByRole("link", { name: "Reminders" }).click();
  await expect(page).toHaveURL("/reminders");

  // "Call mom" must appear in the active buckets (live SSE push).
  const callMomText = page.getByText("Call mom");
  await expect(callMomText).toBeVisible({ timeout: 10_000 });

  // ── Complete the reminder ─────────────────────────────────────────────────
  // ReminderRow renders "Mark as complete" for each active row. Use the
  // check-circle button sibling to the row title. Since there may be multiple
  // active reminders we filter the list item that contains "Call mom".
  const callMomRow = page.locator("li").filter({ hasText: "Call mom" });
  await callMomRow.getByRole("button", { name: "Mark as complete" }).click();

  // After completing, "Call mom" should move out of active buckets.
  // Open the Done section to verify it is there.
  const doneToggle = page.getByRole("button", { name: "Show completed reminders" });
  await expect(doneToggle).toBeVisible();
  await doneToggle.click();
  await expect(page.getByText("Call mom")).toBeVisible({ timeout: 10_000 });

  // ── Edit a reminder's title ───────────────────────────────────────────────
  // Use the Buy milk reminder from journey 2 if present, or add a quick-add
  // reminder to edit. We add via quick-add so this journey is self-contained.
  const quickAddTitle = page.getByRole("textbox", { name: "Title" });
  await quickAddTitle.fill("Call sister");
  // The datetime-local input uses aria-label "Due".
  // Set a due date 1 hour from now so validation passes.
  const oneHourFromNow = new Date(Date.now() + 60 * 60 * 1000);
  const pad = (n: number): string => String(n).padStart(2, "0");
  const dueLocal = `${String(oneHourFromNow.getFullYear())}-${pad(oneHourFromNow.getMonth() + 1)}-${pad(oneHourFromNow.getDate())}T${pad(oneHourFromNow.getHours())}:${pad(oneHourFromNow.getMinutes())}`;
  await page.getByLabel("Due").fill(dueLocal);
  await page.getByRole("button", { name: "Add" }).click();
  await expect(page.getByText("Call sister")).toBeVisible({ timeout: 5_000 });

  // Open the edit sheet via the row body button (aria-label "Edit reminder").
  const callSisterRow = page.locator("li").filter({ hasText: "Call sister" });
  await callSisterRow.getByRole("button", { name: "Edit reminder" }).click();

  // Scope all edit interactions to the slide-over dialog: the quick-add form
  // above also carries a "Title"-labelled input, so an unscoped getByLabel would
  // match two elements once the sheet is open.
  const editSheet = page.getByRole("dialog");
  await expect(editSheet.getByRole("heading", { name: "Edit reminder" })).toBeVisible();

  // Clear the title field and type a new one.
  const titleField = editSheet.getByLabel("Title");
  await titleField.clear();
  await titleField.fill("Call sister updated");

  await editSheet.getByRole("button", { name: "Save" }).click();

  // The slide-over closes and the updated title appears in the reminders list.
  await expect(page.getByText("Call sister updated")).toBeVisible({ timeout: 10_000 });
  // Exact match: "Call sister" is a substring of "Call sister updated", so a
  // default (substring) matcher would still find the renamed row and wrongly fail.
  await expect(page.getByText("Call sister", { exact: true })).not.toBeVisible();
});
