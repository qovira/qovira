// Sanitized Markdown pipeline — the single chokepoint for rendering model text.
//
// SECURITY: All assistant and tool text MUST flow through renderSafeMarkdown()
// before being placed into {@html …}. Never use {@html} on raw model output.
//
// Pipeline: Markdown source → marked (HTML string) → DOMPurify (sanitized HTML)
//
// DOMPurify configuration:
//   - ALLOWED_TAGS: strict allowlist of tags that marked actually emits —
//     p, h1–h6, ul, ol, li, pre, code, blockquote, a, img, table, thead,
//     tbody, tr, th, td, strong, em, del, hr, br, span.
//     Anything not on this list (form, input, button, textarea, select, …) is
//     dropped. This is the correct direction — allowlist the safe set rather
//     than FORBID_TAGS over a default-allow base.
//   - ALLOWED_ATTR: strict allowlist of attributes that markdown legitimately
//     needs — href, src, alt, title, colspan, rowspan. The `style` attribute
//     is intentionally absent; inline styles can paint full-viewport overlays
//     (CSP sets style-src 'unsafe-inline' so DOMPurify is the only defense).
//   - Strips dangerous URI schemes (javascript:, data:, vbscript:) via two layers:
//       1. ALLOWED_URI_REGEXP — the primary config-level allowlist; permits only
//          http:, https:, mailto:, tel:, relative paths, and anchor-only fragments.
//       2. uponSanitizeAttribute hook — secondary belt-and-suspenders defense;
//          explicitly rejects the same dangerous schemes on href/src/action attrs.
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
 *
 * Accepts exactly:
 *   - https:, http:, mailto:, tel:    — named safe schemes (no ftp/sms/etc.)
 *   - /path                            — absolute-relative paths (start with /)
 *   - ./path, ../path                  — relative paths (start with ./ or ../)
 *   - #fragment                        — anchor-only fragments (start with #)
 *
 * Rejects everything else, including:
 *   - javascript:, data:, vbscript:    — dangerous execution / data schemes
 *   - java\tscript:, java\nscript:     — whitespace-obfuscated variants
 *   - ' javascript:'                   — leading-space obfuscated variants
 *   - ftp:, sms:, and any other scheme not in the explicit allowlist above
 *
 * This is the PRIMARY config-level URI control passed to DOMPurify's ALLOWED_URI_REGEXP.
 * The uponSanitizeAttribute hook below is secondary defense-in-depth (belt-and-suspenders).
 *
 * Design note: the previous pattern used a loose `[^a-z]|[a-z+.-]+(?:[^a-z+.-:]|$)` tail
 * that delegated obfuscated-scheme rejection entirely to DOMPurify's pre-normalisation step.
 * This anchored pattern rejects obfuscated schemes at the regexp level itself, so the
 * two defenses are genuinely independent rather than one being a fallback of the other.
 */
const ALLOWED_URI_REGEXP = /^(?:(?:https?|mailto|tel):|[/.#]|\.\.\/)/;

/** URI schemes that must never appear in href/src/action attributes. */
const DANGEROUS_SCHEME = /^(?:javascript|data|vbscript)\s*:/i;

/**
 * Tags that marked.js can legitimately emit when rendering standard Markdown.
 * This is a strict allowlist — any tag not in this list is stripped.
 *
 * Tags verified against marked ^18 output for: headings, paragraphs, lists,
 * code blocks, blockquotes, links, images, tables, emphasis, strikethrough, hr, br.
 * `span` is included because marked emits it for some inline constructs.
 *
 * Intentionally excluded: form, input, button, textarea, select, script, style,
 * iframe, object, embed, and all other interactive / embedding elements.
 */
const ALLOWED_TAGS = [
  "p",
  "h1",
  "h2",
  "h3",
  "h4",
  "h5",
  "h6",
  "ul",
  "ol",
  "li",
  "pre",
  "code",
  "blockquote",
  "a",
  "img",
  "table",
  "thead",
  "tbody",
  "tr",
  "th",
  "td",
  "strong",
  "em",
  "del",
  "hr",
  "br",
  "span",
];

/**
 * Attributes that marked.js can legitimately emit for the tags above.
 * This is a strict allowlist — any attribute not listed is stripped.
 *
 * Intentionally excluded: style (can paint full-viewport overlays; CSP has
 * style-src 'unsafe-inline' so the browser would not block it), class, id,
 * data-*, on* event handlers, action, method, and all other non-content attrs.
 */
const ALLOWED_ATTR = ["href", "src", "alt", "title", "colspan", "rowspan"];

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
 * vectors: <script>, inline event handlers, javascript:/data:/vbscript: URIs,
 * credential-phishing form elements, and CSS overlay style attributes.
 *
 * Sanitization operates at three levels:
 *   1. ALLOWED_TAGS (primary tag control): strict allowlist scoped to tags
 *      that marked actually emits. Drops form/input/button/textarea/select and
 *      all other non-markdown elements.
 *   2. ALLOWED_ATTR (primary attribute control): strict allowlist scoped to
 *      attributes that markdown legitimately needs. Drops style (CSS overlay
 *      vector), class, id, data-*, and all event handler attributes.
 *   3. ALLOWED_URI_REGEXP + uponSanitizeAttribute (URI control): two-layer
 *      defense that strips dangerous URI schemes from href/src. Primary layer
 *      is the regexp allowlist; the hook is belt-and-suspenders.
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
      // ALLOWED_TAGS: strict tag allowlist. Only tags that marked legitimately emits
      // for standard Markdown are permitted. Form, input, button, textarea, select,
      // script, style, and all other non-content tags are dropped.
      ALLOWED_TAGS,
      // ALLOWED_ATTR: strict attribute allowlist. The style attribute is intentionally
      // absent — inline styles can paint full-viewport overlays (CSP allows them).
      ALLOWED_ATTR,
      // ALLOWED_URI_REGEXP: primary config-level URI control. Only href/src/action
      // values matching this pattern pass through DOMPurify's attribute check.
      // Dangerous schemes (javascript:, data:, vbscript:) do not match and are stripped.
      ALLOWED_URI_REGEXP,
    });
  } finally {
    // Always remove the hook to avoid accumulation on the DOMPurify singleton.
    DOMPurify.removeHooks(HOOK_NAME);
  }
}
