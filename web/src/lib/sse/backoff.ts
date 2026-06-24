// Exponential backoff with jitter — pure utility, no store or DOM dependencies.
//
// Parameters:
//   initial  : 500ms
//   max      : 30 000ms
//   multiplier: 2×
//   jitter   : ±20% of the computed value (uniform random)

const BACKOFF_MAX_MS = 30_000;
const BACKOFF_MULTIPLIER = 2;
// Jitter factor: random value in [1 - JITTER, 1 + JITTER].
const BACKOFF_JITTER = 0.2;

/**
 * Compute the next backoff duration in milliseconds.
 *
 * Doubles the current value, applies ±20% jitter, then clamps to BACKOFF_MAX_MS. Clamping after jitter guarantees the
 * result never exceeds the documented 30s ceiling. Returns a positive integer.
 */
export function nextBackoff(currentMs: number): number {
  const doubled = currentMs * BACKOFF_MULTIPLIER;
  const jitter = 1 + (Math.random() * 2 - 1) * BACKOFF_JITTER;
  return Math.min(Math.round(doubled * jitter), BACKOFF_MAX_MS);
}

/** Initial backoff value in ms. */
export const BACKOFF_INITIAL_MS = 500;
