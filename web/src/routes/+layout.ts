/**
 * Off — Qovira ships as a client-only SPA embedded in the Go binary and served via adapter-static's
 * index.html fallback, so there is no server runtime to render on.
 */
export const ssr = false;

/** Off — the SPA renders entirely on the client from live API/SSE data; there are no static routes to prerender. */
export const prerender = false;
