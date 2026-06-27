import { describe, it, expect } from "vitest";

// src/tests/ holds only this standalone harness probe.
// Per house convention, real tests are co-located with their source files
// (e.g. src/lib/foo.ts -> src/lib/foo.test.ts). Those arrive with the
// app shell and feature code in later units.
describe("vitest harness", () => {
  it("runs", () => {
    expect(true).toBe(true);
  });
});
