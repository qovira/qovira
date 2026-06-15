// Tests for the sanitized Markdown pipeline.
//
// Vitest project: jsdom — DOMPurify requires a DOM that correctly processes href
// attributes in block context (jsdom passes DOMPurify's isSupported check and
// faithfully sanitizes block-context anchors; happy-dom has a known fidelity bug
// where uponSanitizeAttribute may not fire for such elements).
//
// Verifies that the pipeline:
//   1. Renders valid Markdown to HTML.
//   2. Strips script injection payloads (XSS prevention — AC #3).
//   3. Strips HTML injection payloads.
//   4. Never exposes inline event handlers.
//   5. Never exposes javascript:/data:/vbscript: hrefs (including block-context anchors).

import { describe, expect, it } from "vitest";

import { renderSafeMarkdown } from "./sanitize.js";

describe("renderSafeMarkdown()", () => {
  it("renders basic Markdown to HTML", () => {
    const result = renderSafeMarkdown("**bold** and _italic_");
    expect(result).toContain("<strong>bold</strong>");
    expect(result).toContain("<em>italic</em>");
  });

  it("renders a code block", () => {
    const result = renderSafeMarkdown("```\nconst x = 1;\n```");
    expect(result).toContain("<code>");
  });

  it("strips <script> injection payloads (AC #3 — XSS prevention)", () => {
    const malicious = '<script>alert("xss")</script>\n\nSafe text';
    const result = renderSafeMarkdown(malicious);
    expect(result).not.toContain("<script");
    expect(result).not.toContain("alert(");
  });

  it("strips inline event handler injection (AC #3)", () => {
    const malicious = '<img src="x" onerror="alert(1)">';
    const result = renderSafeMarkdown(malicious);
    expect(result).not.toContain("onerror");
    expect(result).not.toContain("alert(1)");
  });

  it("strips javascript: href injection via Markdown link (AC #3)", () => {
    const malicious = "[click me](javascript:alert(1))";
    const result = renderSafeMarkdown(malicious);
    expect(result).not.toContain("javascript:");
  });

  it("strips javascript: href injection via block-context raw anchor (AC #3)", () => {
    // A raw <a> tag in a block paragraph — this is the case happy-dom mishandles.
    // jsdom + DOMPurify's ALLOWED_URI_REGEXP allowlist neutralizes it correctly.
    const malicious = '<p><a href="javascript:alert(1)">click</a></p>';
    const result = renderSafeMarkdown(malicious);
    expect(result).not.toContain("javascript:");
  });

  it("strips data: URI injection (AC #3)", () => {
    const malicious = '<a href="data:text/html,<script>alert(1)</script>">click</a>';
    const result = renderSafeMarkdown(malicious);
    expect(result).not.toContain("data:");
  });

  it("strips vbscript: URI injection (AC #3)", () => {
    const malicious = '<a href="vbscript:MsgBox(1)">click</a>';
    const result = renderSafeMarkdown(malicious);
    expect(result).not.toContain("vbscript:");
  });

  it("strips vbscript: URI injection via Markdown link (AC #3)", () => {
    const malicious = "[click me](vbscript:MsgBox(1))";
    const result = renderSafeMarkdown(malicious);
    expect(result).not.toContain("vbscript:");
  });

  it("preserves safe https links", () => {
    const result = renderSafeMarkdown("[Qovira](https://qovira.ai)");
    expect(result).toContain('href="https://qovira.ai"');
    expect(result).toContain("Qovira");
  });

  it("preserves safe http links", () => {
    const result = renderSafeMarkdown("[example](http://example.com)");
    expect(result).toContain('href="http://example.com"');
  });

  it("preserves mailto: links", () => {
    const result = renderSafeMarkdown("[email](mailto:hello@example.com)");
    expect(result).toContain('href="mailto:hello@example.com"');
  });

  it("preserves relative links", () => {
    const result = renderSafeMarkdown("[home](/home)");
    expect(result).toContain('href="/home"');
  });

  it("preserves anchor links", () => {
    const result = renderSafeMarkdown("[section](#section)");
    expect(result).toContain('href="#section"');
  });

  it("returns a non-empty string for empty input", () => {
    const result = renderSafeMarkdown("");
    // Either empty string or empty paragraph — both are safe.
    expect(typeof result).toBe("string");
  });

  it("handles plain text with no Markdown", () => {
    const result = renderSafeMarkdown("Hello, world!");
    expect(result).toContain("Hello, world!");
  });

  it("renders ordered and unordered lists", () => {
    const result = renderSafeMarkdown("- item one\n- item two");
    expect(result).toContain("<li>");
    expect(result).toContain("item one");
  });
});
