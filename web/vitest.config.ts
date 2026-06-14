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

// Three test projects, each in the environment its suite needs:
//   node    — pure-Node unit tests (file I/O, no DOM), e.g. boot.test.ts
//   runes   — .svelte.ts rune logic; node env + the Svelte compiler transform
//             so $state/$derived/$effect work (flushSync / $effect.root), per
//             the conventions:writing-svelte skill
//   browser — DOM-dependent tests (document.cookie / fetch), e.g. the Api
//             client; happy-dom env, excluding the rune suites above
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
        resolve: { alias },
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
          name: "browser",
          environment: "happy-dom",
          include: ["src/lib/**/*.test.ts"],
          exclude: ["src/**/*.svelte.test.ts"],
        },
      },
    ],
  },
});
