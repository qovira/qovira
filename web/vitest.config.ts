import { svelte } from "@sveltejs/vite-plugin-svelte";
import { defineConfig } from "vitest/config";

// Two test projects:
//   "unit"  — node environment, plain .ts (e.g. boot.test.ts)
//   "runes" — node environment with the Svelte compiler transform so $state /
//             $derived / $effect runes are available in .svelte.ts singletons.
//             Uses flushSync / $effect.root for rune-logic tests per the
//             conventions:writing-svelte skill.
export default defineConfig({
  test: {
    projects: [
      {
        test: {
          name: "unit",
          environment: "node",
          include: ["src/**/*.test.ts"],
          exclude: ["src/**/*.svelte.test.ts"],
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
    ],
  },
});
