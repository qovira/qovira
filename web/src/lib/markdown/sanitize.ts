// Sanitized Markdown pipeline — the single chokepoint for rendering model text.
//
// SECURITY: All assistant and tool text MUST flow through renderSafeMarkdown()
// before being placed into {@html …}. Never use {@html} on raw model output.
//
// Pipeline: Markdown source → marked (HTML string) → DOMPurify (sanitized HTML)
//
// DOMPurify configuration:
//   - Strips all <script> elements and event-handler attributes (onerror, onclick, …)
//   - Strips dangerous URI schemes (javascript:, data:, vbscript:) via two layers:
//       1. ALLOWED_URI_REGEXP — the primary config-level allowlist; permits only
//          http:, https:, mailto:, tel:, relative paths, and anchor-only fragments.
//       2. uponSanitizeAttribute hook — secondary belt-and-suspenders defense;
//          explicitly rejects the same dangerous schemes on href/src/action attrs.
//   - Allows the standard set of "safe" HTML tags (paragraphs, headings, lists,
//     code, blockquotes, links, images, tables, …) that marked can produce
//
// Security boundary: the ALLOWED_URI_REGEXP allowlist is the primary URI control.
// It operates through DOMPurify's native attribute-check path, which requires a
// faithful DOM (real browser or jsdom). The uponSanitizeAttribute hook is
// belt-and-suspenders; it fires after DOMPurify parses each attribute but is NOT
// guaranteed to fire for every attribute in every test environment — happy-dom has
// a known fidelity bug where the hook may not fire for block-context anchors.
// Use the jsdom project (*.jsdom.test.ts) to test XSS cases, not happy-dom.
//
// CSP note: the rendered HTML is injected via {@html} inside a Svelte component.
// The server sends `Content-Security-Policy: default-src 'self'; frame-ancestors 'none'`.
// The default-src 'self' directive covers script-src and blocks any surviving
// <script> from executing — DOMPurify is the primary defense and CSP is an outer
// belt-and-suspenders layer.
//
// Usage:
//   import { renderSafeMarkdown } from "$lib/markdown/sanitize.js";
//   const html = renderSafeMarkdown(message.content);
//   // then: {@html html}

import { marked } from "marked";
import DOMPurify from "dompurify";

// marked is synchronous by default; keep it that way — all options used here
// return strings, not Promises. The `async: false` default is the safe path.
marked.setOptions({ async: false });

/**
 * Allowlist of URI schemes and forms that are safe to include in href/src/action attrs.
 * Permits: http:, https:, mailto:, tel:, relative paths (/foo, ../bar), and anchor
 * fragments (#section). Blocks everything else — javascript:, data:, vbscript:, etc.
 *
 * This is the PRIMARY config-level URI control passed to DOMPurify's ALLOWED_URI_REGEXP.
 * The uponSanitizeAttribute hook below is secondary defense-in-depth.
 */
const ALLOWED_URI_REGEXP = /^(?:(?:https?|mailto|tel):|[^a-z]|[a-z+.-]+(?:[^a-z+.-:]|$))/i;

/** URI schemes that must never appear in href/src/action attributes. */
const DANGEROUS_SCHEME = /^(?:javascript|data|vbscript)\s*:/i;

/**
 * DOMPurify hook name used to strip dangerous URI schemes.
 * Registered before each sanitize call and removed immediately after so the
 * hook does not accumulate on the global DOMPurify instance.
 */
const HOOK_NAME = "uponSanitizeAttribute" as const;

/**
 * Convert Markdown text to sanitized HTML.
 *
 * Safe to pass to Svelte's {@html} directive. Strips all script injection
 * vectors: <script>, inline event handlers, javascript:/data:/vbscript: URIs.
 *
 * URI sanitization operates at two levels:
 *   1. ALLOWED_URI_REGEXP (primary): DOMPurify config-level allowlist that permits
 *      only safe schemes (http, https, mailto, tel) and relative/anchor URLs.
 *      Requires a faithful DOM — effective in real browsers and jsdom.
 *   2. uponSanitizeAttribute hook (secondary, belt-and-suspenders): unconditionally
 *      rejects dangerous URI schemes on href/src/action attrs. Note: this hook
 *      is NOT a cross-environment guarantee — happy-dom has a known fidelity bug
 *      where it may not fire for block-context anchors. Test XSS cases in jsdom
 *      (*.jsdom.test.ts), not happy-dom.
 *
 * @param markdown - Raw model-produced Markdown. May contain adversarial HTML.
 * @returns        - Sanitized HTML string, safe for {@html}.
 */
export function renderSafeMarkdown(markdown: string): string {
  // marked.parse with async:false returns a string synchronously.
  const raw = marked.parse(markdown) as string;

  // Register a per-call hook that removes dangerous URI schemes as secondary defense.
  // The primary control is ALLOWED_URI_REGEXP in the DOMPurify config below.
  DOMPurify.addHook(HOOK_NAME, (_node, data) => {
    const name = data.attrName;
    if (name === "href" || name === "src" || name === "action" || name === "xlink:href") {
      if (DANGEROUS_SCHEME.test(data.attrValue)) {
        data.keepAttr = false;
      }
    }
  });

  try {
    return DOMPurify.sanitize(raw, {
      // ALLOWED_URI_REGEXP is the primary config-level URI control. Only href/src/action
      // values matching this pattern pass through DOMPurify's attribute check. Dangerous
      // schemes (javascript:, data:, vbscript:) do not match and are stripped.
      ALLOWED_URI_REGEXP,
      // Disallow <script> and <style> tags explicitly (defense-in-depth).
      FORBID_TAGS: ["script", "style"],
    });
  } finally {
    // Always remove the hook to avoid accumulation on the DOMPurify singleton.
    DOMPurify.removeHooks(HOOK_NAME);
  }
}
