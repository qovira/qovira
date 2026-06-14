/**
 * shouldCache — pure cache-routing decision for the app-shell service worker.
 *
 * Extracted from service-worker.ts so it can be unit-tested without the
 * $service-worker ambient module or a real service-worker execution context.
 *
 * @param url    The full URL of the request.
 * @param method The HTTP method (GET, POST, …).
 * @param selfOrigin The origin of the service worker scope (e.g. "https://app.example.com").
 *   Defaults to `self.location.origin` in service-worker context; callers may
 *   pass it explicitly for testing.
 *
 * Rules (in priority order):
 *   1. Non-GET/HEAD methods → never cache (POST, PATCH, DELETE, etc.)
 *   2. Cross-origin requests → never cache (we only serve same-origin)
 *   3. /api/ paths → passthrough to network (real-time data, no offline replica)
 *   4. /events path → passthrough to network (SSE stream)
 *   5. Everything else (same-origin GET/HEAD) → cache-first (app shell assets)
 */
export function shouldCache(url: string, method: string, selfOrigin?: string): boolean {
  // Rule 1 — only cache safe read-only methods.
  const upperMethod = method.toUpperCase();
  if (upperMethod !== "GET" && upperMethod !== "HEAD") return false;

  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    // Unparseable URL — don't cache.
    return false;
  }

  // Rule 2 — cross-origin: requests whose origin differs from the SW scope
  // are never cached. If no selfOrigin is provided, default to the request's
  // own origin so purely local paths always pass (backwards-safe fallback
  // for environments without `self.location`).
  const origin = selfOrigin ?? parsed.origin;
  if (parsed.origin !== origin) return false;

  // Rule 3 — API paths carry live data with no offline replica.
  if (parsed.pathname.startsWith("/api/")) return false;

  // Rule 4 — SSE event stream must always go to the network.
  if (parsed.pathname === "/events" || parsed.pathname.startsWith("/events/")) return false;

  // Rule 5 — cacheable app-shell asset.
  return true;
}
