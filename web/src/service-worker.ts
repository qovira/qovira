/**
 * App-shell precaching service worker.
 *
 * Architecture: online-first. The SW exists solely to:
 *   1. Precache the SvelteKit build artifacts and static files on install so
 *      cold starts load from the cache (fast shell).
 *   2. Serve those cached assets cache-first on fetch.
 *   3. Serve the shell document ("/index.html") as the navigation fallback so
 *      deep links (e.g. /reminders, /settings) load offline once the shell is
 *      cached — SvelteKit's `$service-worker.build` does NOT include index.html
 *      for adapter-static SPAs, so it must be added explicitly.
 *   4. Clean up stale versioned caches on activate.
 *
 * Data paths (/api/, /events) and non-GET requests bypass the cache entirely
 * and go straight to the network — the app has no offline data replica.
 *
 * SvelteKit auto-registers this file; do NOT add manual registration elsewhere.
 */

/// <reference types="@sveltejs/kit" />
/// <reference no-default-lib="true"/>
/// <reference lib="esnext" />
/// <reference lib="webworker" />

import { build, files, version } from "$service-worker";

import { shouldCache } from "$lib/sw/cache-routing.js";

declare const self: ServiceWorkerGlobalScope;

// ---------------------------------------------------------------------------
// Cache naming
// ---------------------------------------------------------------------------

/** Versioned cache name — bumped automatically when the build version changes. */
const CACHE_NAME = `qovira-shell-v${version}`;

/**
 * The navigation fallback document.
 *
 * SvelteKit's adapter-static emits "index.html" as the fallback for any
 * unmatched navigation, but $service-worker.build contains only hashed
 * _app/* assets — the HTML document is absent from all three arrays (build,
 * files, prerendered). Adding it explicitly ensures a cold start with no
 * network can serve the shell for any route.
 */
const NAV_FALLBACK = "/index.html";

/** All assets that make up the app shell, including the navigation fallback. */
const SHELL_ASSETS = [...build, ...files, NAV_FALLBACK];

// ---------------------------------------------------------------------------
// Install — precache the shell
// ---------------------------------------------------------------------------

self.addEventListener("install", (event) => {
  event.waitUntil(
    (async () => {
      const cache = await caches.open(CACHE_NAME);
      await cache.addAll(SHELL_ASSETS);
      // Skip waiting so the new SW activates immediately on pages that have
      // no outstanding fetch to the old shell.
      await self.skipWaiting();
    })(),
  );
});

// ---------------------------------------------------------------------------
// Activate — clean up caches from previous versions
// ---------------------------------------------------------------------------

self.addEventListener("activate", (event) => {
  event.waitUntil(
    (async () => {
      const keys = await caches.keys();
      await Promise.all(keys.filter((key) => key !== CACHE_NAME).map((key) => caches.delete(key)));
      // Take control of all clients without waiting for a page reload.
      await self.clients.claim();
    })(),
  );
});

// ---------------------------------------------------------------------------
// Fetch — cache-first for shell, network-only for data paths
// ---------------------------------------------------------------------------

self.addEventListener("fetch", (event) => {
  const { request } = event;

  // Delegate the routing decision to the pure helper so it remains testable.
  if (!shouldCache(request.url, request.method, self.location.origin)) {
    // Network-only: API calls, SSE stream, and non-GET requests.
    return;
  }

  event.respondWith(
    (async () => {
      // For navigation requests (HTML document fetches for any SPA route),
      // try an exact cache match first; fall back to the precached shell
      // document so deep links work offline without needing their own entry.
      if (request.mode === "navigate") {
        const cached = await caches.match(request);
        if (cached !== undefined) return cached;
        const shell = await caches.match(NAV_FALLBACK);
        if (shell !== undefined) return shell;
        // Network fallback if the shell isn't cached yet (first install race).
        return fetch(request);
      }

      // Non-navigation assets: exact cache-first, then network + runtime cache.
      const cached = await caches.match(request);
      if (cached !== undefined) return cached;

      // Not in cache yet (possible for assets added after install).
      // Fetch from network and cache the response for next time.
      const response = await fetch(request);
      if (response.ok) {
        const cache = await caches.open(CACHE_NAME);
        await cache.put(request, response.clone());
      }
      return response;
    })(),
  );
});
