/**
 * Playwright E2E configuration for the Qovira journeys.
 *
 * Design decisions:
 *   - workers: 1 / fullyParallel: false — one seeded admin against one shared
 *     DB; specs must run serially to avoid cross-test reminder interference.
 *   - webServer builds the e2e binary and starts it fresh (wiped data dir) so
 *     admin seeding fires on every run. `make e2e-server` is the entry point.
 *   - Auth: setup project writes storageState; journey specs declare
 *     `dependencies: ['setup']` and load it via storageState.
 *   - Reporter: list locally, blob in CI (for merge-reports).
 *   - Retries: 0 locally, 1 in CI (a single re-run to catch infra flake).
 */

import { defineConfig, devices } from "@playwright/test";
import path from "path";
import { fileURLToPath } from "url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));

const PORT = "3737";
const BASE_URL = `http://localhost:${PORT}`;

// Ephemeral data dir — wiped by the Makefile target before each run.
const E2E_DATA_DIR = "/tmp/qovira-e2e-data";

// Fixture path — absolute so it is correct regardless of cwd.
const FIXTURE_PATH = path.join(__dirname, "e2e", "fixtures", "journeys.json");

export default defineConfig({
  // Test directory — the projects below refine which files each project picks up.
  testDir: "./e2e",

  // Serial: shared DB with one admin account.
  workers: 1,
  fullyParallel: false,

  // Fail fast on `.only` in CI.
  forbidOnly: !!process.env.CI,

  // One retry in CI to absorb infra flake; none locally (investigate via trace).
  retries: process.env.CI ? 1 : 0,

  // Reporter: list locally; blob in CI for merge-reports compatibility.
  reporter: process.env.CI ? "blob" : "list",

  // Global timeout per test: 60 s. The slow-story test takes ~8 × 500 ms + reload
  // overhead; keep headroom.
  timeout: 60_000,

  use: {
    baseURL: BASE_URL,
    // Capture trace on first retry so failures are diagnosable.
    trace: "on-first-retry",
    // Include video on first retry for additional debugging.
    video: "on-first-retry",
  },

  projects: [
    // Auth setup project — logs in once, writes storageState.
    {
      name: "setup",
      testMatch: /auth\.setup\.ts/,
      use: { ...devices["Desktop Chrome"] },
    },

    // Journey specs — all run on Chromium, depend on auth setup.
    {
      name: "chromium",
      testMatch: /journey-.*\.spec\.ts/,
      use: {
        ...devices["Desktop Chrome"],
      },
      dependencies: ["setup"],
    },
  ],

  // webServer: build the e2e binary and start a fresh server for every run.
  // `make e2e-server` wipes the data dir, builds the binary, and exec's it.
  // url: healthz endpoint — Playwright polls until 200 before running specs.
  // reuseExistingServer: allowed locally to speed up iteration; never in CI.
  webServer: {
    // Run from the repo root (one level up from web/).
    cwd: "..",
    command: "make e2e-server",
    url: `${BASE_URL}/healthz`,
    timeout: 120_000,
    reuseExistingServer: !process.env.CI,
    env: {
      QOVIRA_MASTER_KEY: "e2e-test-master-key-32bytes!!!!",
      QOVIRA_ADMIN_EMAIL: "admin@e2e.test",
      QOVIRA_ADMIN_PASSWORD: "AdminPass123!",
      QOVIRA_HTTP_ADDR: `:${PORT}`,
      QOVIRA_DATA_DIR: E2E_DATA_DIR,
      QOVIRA_E2E_SCRIPT_PATH: FIXTURE_PATH,
    },
  },
});
