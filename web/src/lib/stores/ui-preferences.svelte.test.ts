// Tests for the ui-preferences store: railPinned load/persist and state transitions. Rune logic is tested with
// $effect.root + flushSync in the node environment.
import { flushSync } from "svelte";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

// We re-import the module fresh for each group via vi.resetModules in beforeEach. Module-level $state means a single
// singleton per import; to isolate tests we reset state manually via the exported setters rather than re-importing.
import { getRailPinned, initPrefs, setRailPinned } from "./ui-preferences.svelte.js";

// ---------------------------------------------------------------------------
// Minimal localStorage stub (node environment has no DOM/localStorage)
// ---------------------------------------------------------------------------

interface StorageStub {
  store: Record<string, string>;
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
  removeItem(key: string): void;
  clear(): void;
}

function makeStorage(): StorageStub {
  const store: Record<string, string> = {};
  return {
    store,
    getItem: (key: string) => store[key] ?? null,
    setItem: (key: string, value: string) => {
      store[key] = value;
    },
    removeItem: (key: string) => {
      // eslint-disable-next-line @typescript-eslint/no-dynamic-delete
      delete store[key];
    },
    clear: () => {
      for (const k of Object.keys(store)) {
        // eslint-disable-next-line @typescript-eslint/no-dynamic-delete
        delete store[k];
      }
    },
  };
}

let storage: StorageStub;

beforeEach(() => {
  storage = makeStorage();
  // Wire the stub into globalThis so the store's localStorage calls hit it.
  Object.defineProperty(globalThis, "localStorage", {
    value: storage,
    writable: true,
    configurable: true,
  });
});

afterEach(() => {
  // Reset state to defaults after each test so the singleton doesn't bleed.
  setRailPinned(false);
  storage.clear();
});

// ---------------------------------------------------------------------------
// Default state
// ---------------------------------------------------------------------------

describe("ui-preferences store — defaults", () => {
  it("defaults railPinned to false without localStorage", () => {
    expect(getRailPinned()).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// initPrefs — loads from localStorage
// ---------------------------------------------------------------------------

describe("initPrefs()", () => {
  it("loads railPinned=true from localStorage", () => {
    storage.setItem("qovira-ui", JSON.stringify({ railPinned: true }));
    initPrefs();
    flushSync();
    expect(getRailPinned()).toBe(true);
  });

  it("loads railPinned=false from localStorage", () => {
    // Explicitly start in a non-default state to confirm the read overrides it.
    setRailPinned(true);
    storage.setItem("qovira-ui", JSON.stringify({ railPinned: false }));
    initPrefs();
    flushSync();
    expect(getRailPinned()).toBe(false);
  });

  it("ignores missing localStorage key and keeps default", () => {
    // No key set — should remain false.
    initPrefs();
    flushSync();
    expect(getRailPinned()).toBe(false);
  });

  it("ignores malformed JSON and keeps default", () => {
    storage.setItem("qovira-ui", "not-json{{{");
    initPrefs();
    flushSync();
    expect(getRailPinned()).toBe(false);
  });

  it("ignores non-boolean railPinned value and keeps default", () => {
    storage.setItem("qovira-ui", JSON.stringify({ railPinned: "yes" }));
    initPrefs();
    flushSync();
    expect(getRailPinned()).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// setRailPinned — persists to localStorage
// ---------------------------------------------------------------------------

describe("setRailPinned()", () => {
  it("persists true to localStorage under 'qovira-ui'", () => {
    setRailPinned(true);
    const raw = storage.getItem("qovira-ui");
    expect(raw).not.toBeNull();
    if (raw === null) {
      throw new Error("raw should not be null");
    }
    const parsed = JSON.parse(raw) as Record<string, unknown>;
    expect(parsed.railPinned).toBe(true);
  });

  it("persists false to localStorage", () => {
    setRailPinned(true);
    setRailPinned(false);
    const raw = storage.getItem("qovira-ui");
    if (raw === null) {
      throw new Error("raw should not be null");
    }
    const parsed = JSON.parse(raw) as Record<string, unknown>;
    expect(parsed.railPinned).toBe(false);
  });

  it("updates the reactive getter synchronously", () => {
    expect(getRailPinned()).toBe(false);
    setRailPinned(true);
    expect(getRailPinned()).toBe(true);
    setRailPinned(false);
    expect(getRailPinned()).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// Rail state-machine transitions (collapsed / peek / pinned)
// ---------------------------------------------------------------------------
// The state machine lives in the layout component (pointer/focus events). Here we test the store invariant:
// pinned=true always overrides the peek state at the store level, and that setRailPinned(true/false) correctly models
// the pin/unpin transitions the component relies on.

describe("rail state-machine — store layer", () => {
  it("starts in collapsed state (pinned=false)", () => {
    expect(getRailPinned()).toBe(false);
  });

  it("pinned state: setRailPinned(true) moves to pinned", () => {
    setRailPinned(true);
    expect(getRailPinned()).toBe(true);
  });

  it("unpin: setRailPinned(false) returns to collapsed", () => {
    setRailPinned(true);
    setRailPinned(false);
    expect(getRailPinned()).toBe(false);
  });

  it("round-trip: init with pinned=true then unpin", () => {
    storage.setItem("qovira-ui", JSON.stringify({ railPinned: true }));
    initPrefs();
    flushSync();
    expect(getRailPinned()).toBe(true);
    setRailPinned(false);
    expect(getRailPinned()).toBe(false);
  });
});
