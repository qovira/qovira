/**
 * Journey 1 — Login → land home.
 *
 * Navigates to /login as an unauthenticated user, fills in the credentials,
 * submits the form, and asserts landing on the root chat surface (`/`).
 *
 * This is the only journey that exercises the login UI itself; all other
 * journeys reuse the stored auth state and start already authenticated.
 */

import { test, expect } from "@playwright/test";

test("login lands on the chat surface", async ({ page }) => {
  // Start unauthenticated — do NOT use storageState for this spec.
  await page.goto("/login");

  await page.getByLabel("Email").fill("admin@e2e.test");
  await page.getByLabel("Password").fill("AdminPass123!");
  await page.getByRole("button", { name: "Sign in" }).click();

  // The layout guard redirects to / on a successful login.
  await expect(page).toHaveURL("/");

  // The chat surface is present: the composer textarea is visible.
  await expect(page.getByRole("textbox", { name: "Message…" })).toBeVisible();
});
