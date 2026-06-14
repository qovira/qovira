import { svelte } from "@sveltejs/vite-plugin-svelte";
import { defineConfig } from "vitest/config";

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
        test: {
          name: "node",
          environment: "node",
          include: ["src/tests/**/*.test.ts"],
        },
      },
      {
        plugins: [svelte()],
        test: {
          name: "runes",
          environment: "node",
          include: ["src/**/*.svelte.test.ts"],
        },
      },
      {
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
