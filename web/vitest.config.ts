import path from "node:path";
import { fileURLToPath } from "node:url";

import { svelte } from "@sveltejs/vite-plugin-svelte";
import { playwright } from "@vitest/browser-playwright";
import { storybookTest } from "@storybook/addon-vitest/vitest-plugin";
import { defineConfig, mergeConfig } from "vitest/config";
import viteConfig from "./vite.config";

const dirname = path.dirname(fileURLToPath(import.meta.url));

// SvelteKit provides the `$lib` alias and the `$app/*` virtual modules at build
// time via its Vite plugin, which is not loaded under vitest. Mirror `$lib` so
// test runtime resolves it the same way the app build does, and alias
// `$app/navigation` to a stub so it is resolvable; suites that assert on
// navigation override the stub with `vi.mock("$app/navigation", …)`.
const alias = {
  $lib: fileURLToPath(new URL("./src/lib", import.meta.url)),
  "$app/navigation": fileURLToPath(new URL("./src/tests/stubs/app-navigation.ts", import.meta.url)),
};

// Five test projects:
//   node      — pure-Node unit tests (file I/O, no DOM), e.g. boot.test.ts
//   runes     — .svelte.ts rune logic; node env + the Svelte compiler transform
//               so $state/$derived/$effect work (flushSync / $effect.root), per
//               the conventions:writing-svelte skill
//   jsdom     — tests that require a full DOM with correct attribute sanitization
//               (DOMPurify XSS tests); jsdom passes DOMPurify's isSupported check
//               and correctly handles href attribute sanitization in block context
//   browser   — DOM-dependent tests (document.cookie / fetch), e.g. the Api
//               client; happy-dom env, excluding the rune suites and jsdom suites
//   storybook — story-as-tests via @storybook/addon-vitest; Vitest Browser Mode
//               with Playwright/chromium; runs every *.stories.svelte / *.stories.ts
export default mergeConfig(
  viteConfig,
  defineConfig({
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
        // Storybook story-as-tests — Vitest Browser Mode, Playwright/chromium.
        // @storybook/addon-vitest transforms every *.stories.svelte / *.stories.ts
        // into a test that renders the story and runs its play() function.
        // CI: pnpm exec playwright install --with-deps chromium before this runs.
        {
          extends: true,
          plugins: [storybookTest({ configDir: path.join(dirname, ".storybook") })],
          test: {
            name: "storybook",
            setupFiles: ["./.storybook/vitest.setup.ts"],
            browser: {
              enabled: true,
              headless: true,
              provider: playwright(),
              instances: [{ browser: "chromium" }],
            },
          },
        },
      ],
    },
  }),
);
