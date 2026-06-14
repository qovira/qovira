// UI-preferences store — module-level singleton, NOT context-provided.
// Survives logout. Persists { railPinned: boolean } to localStorage under
// "qovira-ui". Theme is owned by @qovira/theme/runtime — not duplicated here.

const STORAGE_KEY = "qovira-ui";

interface PersistedPrefs {
  railPinned: boolean;
}

// Module-level $state is valid here: this is genuinely global, non-user data
// (UI preference — persists across logout, same for all sessions on this device).
// SSR note: ssr=false in +layout.ts; this module only runs in the browser.
let railPinned = $state(false);

/** Read the current rail-pinned value. */
export function getRailPinned(): boolean {
  return railPinned;
}

/** Set rail-pinned and persist to localStorage. */
export function setRailPinned(value: boolean): void {
  railPinned = value;
  persist();
}

/**
 * Load persisted preferences from localStorage. Call once from the root layout
 * inside a `$effect` (localStorage access is side-effectful / browser-only).
 * Silently ignores missing or malformed data.
 */
export function initPrefs(): void {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (raw === null) return;
    const parsed: unknown = JSON.parse(raw) as unknown;
    if (typeof parsed !== "object" || parsed === null) return;
    const prefs = parsed as Record<string, unknown>;
    if (typeof prefs.railPinned === "boolean") {
      railPinned = prefs.railPinned;
    }
  } catch {
    // Malformed JSON or localStorage unavailable — keep defaults.
  }
}

function persist(): void {
  try {
    const prefs: PersistedPrefs = { railPinned };
    localStorage.setItem(STORAGE_KEY, JSON.stringify(prefs));
  } catch {
    // localStorage unavailable (private browsing, quota) — no-op.
  }
}
