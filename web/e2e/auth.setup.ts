/**
 * Auth setup project — logs in once via the API and writes storageState so every
 * journey spec starts already authenticated. Runs as a Playwright "setup" project
 * (dependencies: ['setup']) so it executes before the journey specs.
 *
 * Using the API directly (POST /api/v1/auth/login) is preferred over the UI login
 * form: it is faster, not a behaviour under test, and sets both cookies correctly.
 * The storageState capture picks up both __Host-qovira_session (HttpOnly) and
 * qovira_csrf because Playwright's APIRequestContext stores all cookies including
 * HttpOnly ones.
 */

import { test as setup } from "@playwright/test";
import { AUTH_STATE_FILE } from "./constants.js";

setup("authenticate as admin", async ({ request }) => {
  // POST /api/v1/auth/login — no CSRF required on login.
  const resp = await request.post("/api/v1/auth/login", {
    data: {
      email: process.env.QOVIRA_ADMIN_EMAIL ?? "admin@e2e.test",
      password: process.env.QOVIRA_ADMIN_PASSWORD ?? "AdminPass123!",
    },
    headers: { "Content-Type": "application/json" },
  });

  if (!resp.ok()) {
    throw new Error(`Login failed: ${String(resp.status())} ${await resp.text()}`);
  }

  // Write the storage state (cookies) so journey specs can start authenticated.
  // request.storageState() includes HttpOnly cookies.
  await request.storageState({ path: AUTH_STATE_FILE });
});
