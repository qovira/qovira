import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { boot } from "@qovira/theme/boot";
import { describe, expect, it } from "vitest";

// The pre-paint theme boot snippet is inlined verbatim in app.html (a dynamic
// import runs too late and reintroduces the flash). @qovira/theme is the source
// of truth, so this guards against the inlined copy drifting from the upstream
// `boot` export — collapsing whitespace so Prettier's indentation in app.html
// is irrelevant while any change to a key, value, or the decision tree fails.

const appHtml = readFileSync(fileURLToPath(new URL("../app.html", import.meta.url)), "utf8");
const collapse = (s: string): string => s.replace(/\s+/g, "");

describe("pre-paint theme boot snippet", () => {
  it("inlines the @qovira/theme boot export verbatim in app.html", () => {
    expect(collapse(appHtml)).toContain(collapse(boot));
  });

  it("runs before any stylesheet so first paint has no theme flash", () => {
    const scriptIdx = appHtml.indexOf('localStorage.getItem("qovira-theme")');
    const headPlaceholderIdx = appHtml.indexOf("%sveltekit.head%");
    expect(scriptIdx).toBeGreaterThan(-1);
    // SvelteKit emits the app's stylesheet links at %sveltekit.head%; the boot
    // script must precede it (and there must be no earlier stylesheet link).
    expect(scriptIdx).toBeLessThan(headPlaceholderIdx);
    expect(appHtml.slice(0, scriptIdx)).not.toContain('rel="stylesheet"');
  });
});
