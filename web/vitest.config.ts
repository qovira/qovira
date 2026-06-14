import { defineConfig } from "vitest/config";

// Unit tests run in node environment. Component tests would use
// vitest-browser-svelte (Browser Mode), but this scaffold only has unit tests.
export default defineConfig({
  test: {
    environment: "node",
    include: ["src/**/*.test.ts", "src/**/*.svelte.test.ts"],
  },
});
