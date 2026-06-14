// Test stub for SvelteKit's `$app/navigation` virtual module, which has no
// runtime under vitest. Aliased in vitest.config.ts so imports resolve; suites
// that assert on navigation override it with `vi.mock("$app/navigation", …)`.
export function goto(): Promise<void> {
  return Promise.resolve();
}
