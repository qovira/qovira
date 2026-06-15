import { fileURLToPath } from "node:url";

import { svelte } from "@sveltejs/vite-plugin-svelte";
import { defineConfig } from "vitest/config";

// SvelteKit provides the `$lib` alias and the `$app/*` virtual modules at build
// time via its Vite plugin, which is not loaded under vitest. Mirror `$lib` so
// test runtime resolves it the same way the app build does, and alias
// `$app/navigation` to a stub so it is resolvable; suites that assert on
// navigation override the stub with `vi.mock("$app/navigation", …)`.
const alias = {
  $lib: fileURLToPath(new URL("./src/lib", import.meta.url)),
  "$app/navigation": fileURLToPath(new URL("./src/tests/stubs/app-navigation.ts", import.meta.url)),
};

// Four test projects, each in the environment its suite needs:
//   node    — pure-Node unit tests (file I/O, no DOM), e.g. boot.test.ts
//   runes   — .svelte.ts rune logic; node env + the Svelte compiler transform
//             so $state/$derived/$effect work (flushSync / $effect.root), per
//             the conventions:writing-svelte skill
//   jsdom   — tests that require a full DOM with correct attribute sanitization
//             (DOMPurify XSS tests); jsdom passes DOMPurify's isSupported check
//             and correctly handles href attribute sanitization in block context
//   browser — DOM-dependent tests (document.cookie / fetch), e.g. the Api
//             client; happy-dom env, excluding the rune suites and jsdom suites
export default defineConfig({
  test: {
    projects: [
      {
        resolve: { alias },
        test: {
          name: "node",
          environment: "node",
          include: ["src/tests/**/*.test.ts"],
        },
      },
      {
        // conditions adds "svelte" so @qovira/ui (svelte-condition-only exports)
        // is resolvable from .svelte.test.ts suites that mock or import UI packages.
        resolve: { alias, conditions: ["svelte", "import", "default"] },
        plugins: [svelte()],
        test: {
          name: "runes",
          environment: "node",
          include: ["src/**/*.svelte.test.ts"],
        },
      },
      {
        resolve: { alias },
        test: {
          name: "jsdom",
          environment: "jsdom",
          include: ["src/lib/**/*.jsdom.test.ts"],
        },
      },
      {
        // conditions adds "svelte" to the export-condition list so packages
        // that only export under a "svelte" condition (e.g. @qovira/ui) are
        // resolvable in the happy-dom test environment. SvelteKit's Vite plugin
        // adds this condition at build time; here we mirror it for tests.
        resolve: { alias, conditions: ["svelte", "browser", "import", "default"] },
        test: {
          name: "browser",
          environment: "happy-dom",
          include: ["src/lib/**/*.test.ts"],
          exclude: ["src/**/*.svelte.test.ts", "src/lib/**/*.jsdom.test.ts"],
        },
      },
    ],
  },
});
