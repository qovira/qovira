import { defineConfig } from "vitest/config";
import { sveltekit } from "@sveltejs/kit/vite";

export default defineConfig({
  plugins: [sveltekit()],
  test: {
    environment: "node",
    globals: false,
    clearMocks: true,
    restoreMocks: true,
    unstubEnvs: true,
    unstubGlobals: true,
    include: ["src/**/*.test.ts", "src/**/*.svelte.test.ts"],
    coverage: {
      provider: "v8",
      reporter: ["text", "html", "lcov"],
      include: ["src/**"],
      exclude: ["**/*.test.ts", "**/*.svelte.test.ts", "src/**/*.d.ts", "src/app.html"],
      // Thresholds are intentionally omitted for this scaffold — the app
      // has one blank route and one harness test. They will be set once
      // real source and component tests land in a later unit.
    },
  },
});
