/**
 * i18n catalog guard — asserts that the compiled Paraglide message module is
 * present and that every shell message key resolves to a non-empty English
 * string.
 *
 * This test runs in the `node` vitest project (no DOM, no Vite) and imports
 * from the compiled `src/lib/paraglide/messages.js` output directly. If the
 * generated directory is missing, the import fails with a clear error.
 *
 * Purpose: a build-time guard that the catalog is wired and complete for the
 * current v0.1 shell surfaces (rail / layout / login / settings / placeholders).
 * It does NOT snapshot the whole catalog — only the keys that map to current
 * shell surfaces.
 */

import { describe, expect, it } from "vitest";

// Named imports so a missing key causes a TypeScript / module-not-found error.
import {
  nav_aria_label,
  nav_loading,
  nav_account,
  nav_pin,
  nav_unpin,
  nav_switch_to_evening,
  nav_switch_to_daylight,
  nav_chat,
  nav_reminders,
  login_heading,
  login_field_email,
  login_field_password,
  login_submit,
  login_error_unexpected,
  login_error_invalid_credentials,
  login_error_session_verify,
  settings_heading,
  settings_logout,
  home_placeholder,
  reminders_placeholder,
  onboarding_placeholder,
} from "../lib/paraglide/messages.js";

// All shell message functions expected to resolve to non-empty English strings.
// This map covers every key catalogued for the v0.1 shell. Keys for surfaces
// that don't exist yet (chat/reminders chrome) are intentionally absent.
const shellMessages: Record<string, () => string> = {
  nav_aria_label,
  nav_loading,
  nav_account,
  nav_pin,
  nav_unpin,
  nav_switch_to_evening,
  nav_switch_to_daylight,
  nav_chat,
  nav_reminders,
  login_heading,
  login_field_email,
  login_field_password,
  login_submit,
  login_error_unexpected,
  login_error_invalid_credentials,
  login_error_session_verify,
  settings_heading,
  settings_logout,
  home_placeholder,
  reminders_placeholder,
  onboarding_placeholder,
};

describe("i18n catalog (Paraglide, en)", () => {
  it("compiled messages module is importable", () => {
    // If this test file loads at all, the import succeeded.
    expect(typeof nav_chat).toBe("function");
  });

  it.each(Object.entries(shellMessages))('message "%s" resolves to a non-empty string', (key, fn) => {
    const value = fn();
    expect(typeof value, `${key} should return a string`).toBe("string");
    expect(value.length, `${key} should not be empty`).toBeGreaterThan(0);
  });
});
