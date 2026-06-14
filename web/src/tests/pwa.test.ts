/**
 * PWA static-asset checks: manifest fields and icon presence.
 *
 * These run in the `node` project (file I/O, no DOM).
 */

import { existsSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

const STATIC_DIR = fileURLToPath(new URL("../../static", import.meta.url));

// ---------------------------------------------------------------------------
// Manifest
// ---------------------------------------------------------------------------

describe("web manifest", () => {
  const raw = readFileSync(`${STATIC_DIR}/manifest.webmanifest`, "utf8");
  const manifest = JSON.parse(raw) as Record<string, unknown>;

  it("is valid JSON with a name field", () => {
    expect(typeof manifest.name).toBe("string");
    expect((manifest.name as string).length).toBeGreaterThan(0);
  });

  it("has a short_name", () => {
    expect(typeof manifest.short_name).toBe("string");
    expect((manifest.short_name as string).length).toBeGreaterThan(0);
  });

  it("has display: standalone", () => {
    expect(manifest.display).toBe("standalone");
  });

  it("has a theme_color", () => {
    expect(typeof manifest.theme_color).toBe("string");
    expect(manifest.theme_color as string).toMatch(/^#[0-9a-fA-F]{6}$/);
  });

  it("has a background_color", () => {
    expect(typeof manifest.background_color).toBe("string");
    expect(manifest.background_color as string).toMatch(/^#[0-9a-fA-F]{6}$/);
  });

  it("has a start_url", () => {
    expect(typeof manifest.start_url).toBe("string");
  });

  it("has icons array with at least two entries", () => {
    expect(Array.isArray(manifest.icons)).toBe(true);
    expect((manifest.icons as unknown[]).length).toBeGreaterThanOrEqual(2);
  });

  it("has a 192x192 icon entry", () => {
    const icons = manifest.icons as { src: string; sizes: string; type: string; purpose?: string }[];
    const icon192 = icons.find((ic) => ic.sizes === "192x192");
    expect(icon192).toBeDefined();
    expect(icon192?.src).toBeTruthy();
    expect(icon192?.type).toBe("image/png");
  });

  it("has a 512x512 icon entry", () => {
    const icons = manifest.icons as { src: string; sizes: string; type: string; purpose?: string }[];
    const icon512 = icons.find((ic) => ic.sizes === "512x512");
    expect(icon512).toBeDefined();
    expect(icon512?.src).toBeTruthy();
    expect(icon512?.type).toBe("image/png");
  });
});

// ---------------------------------------------------------------------------
// Icon files
// ---------------------------------------------------------------------------

describe("icon files", () => {
  it("icon-192.png exists in static/", () => {
    expect(existsSync(`${STATIC_DIR}/icon-192.png`)).toBe(true);
  });

  it("icon-512.png exists in static/", () => {
    expect(existsSync(`${STATIC_DIR}/icon-512.png`)).toBe(true);
  });

  it("icon-192.png starts with a PNG signature", () => {
    const buf = readFileSync(`${STATIC_DIR}/icon-192.png`);
    // PNG magic bytes: 89 50 4e 47 0d 0a 1a 0a
    expect(buf[0]).toBe(0x89);
    expect(buf[1]).toBe(0x50); // P
    expect(buf[2]).toBe(0x4e); // N
    expect(buf[3]).toBe(0x47); // G
  });

  it("icon-512.png starts with a PNG signature", () => {
    const buf = readFileSync(`${STATIC_DIR}/icon-512.png`);
    expect(buf[0]).toBe(0x89);
    expect(buf[1]).toBe(0x50);
    expect(buf[2]).toBe(0x4e);
    expect(buf[3]).toBe(0x47);
  });
});

// ---------------------------------------------------------------------------
// service-worker.ts — navigation fallback is explicitly precached
// ---------------------------------------------------------------------------

describe("service worker", () => {
  // The $service-worker `build` array contains only hashed _app/* assets;
  // index.html is NOT included by SvelteKit. The SW must add it explicitly so
  // a cold start (no network) can serve the shell document for any navigation.
  const swSrc = readFileSync(fileURLToPath(new URL("../service-worker.ts", import.meta.url)), "utf8");

  it('explicitly precaches "/index.html" as the navigation fallback', () => {
    expect(swSrc).toContain('"/index.html"');
  });

  it('handles navigation requests (mode === "navigate") with fallback to the cached shell', () => {
    // The fetch handler must branch on request.mode === "navigate" so that deep
    // links (e.g. /reminders) resolve offline via the precached shell document.
    expect(swSrc).toContain('"navigate"');
    expect(swSrc).toContain("NAV_FALLBACK");
  });

  it("does NOT import vite-plugin-pwa or any PWA library", () => {
    expect(swSrc).not.toContain("vite-plugin-pwa");
    expect(swSrc).not.toContain("workbox");
  });
});

// ---------------------------------------------------------------------------
// app.html — manifest link and theme-color meta
// ---------------------------------------------------------------------------

describe("app.html PWA head tags", () => {
  const appHtml = readFileSync(fileURLToPath(new URL("../app.html", import.meta.url)), "utf8");

  it('has <link rel="manifest" href="/manifest.webmanifest">', () => {
    expect(appHtml).toContain('rel="manifest"');
    expect(appHtml).toContain('href="/manifest.webmanifest"');
  });

  it('has <meta name="theme-color" content="…">', () => {
    expect(appHtml).toContain('name="theme-color"');
    expect(appHtml).toMatch(/content="#[0-9a-fA-F]{6}"/);
  });

  it("preserves the inline theme-boot script", () => {
    expect(appHtml).toContain('localStorage.getItem("qovira-theme")');
  });
});
