// SPA mode: no SSR, no prerendering. The Go binary serves index.html as the
// fallback for all routes; client-side routing handles navigation.
export const ssr = false;
export const prerender = false;
