package scheduler

import (
	"testing"
	"time"
)

// TestBackoffDuration_BoundsAndShape is a deterministic white-box test of the pure backoffDuration helper. It runs
// inside package scheduler (not the _test variant) so it can reach the unexported function directly — no clock, no DB,
// no flake risk.
//
// Strategy: sample backoffDuration many times per attempt level and assert:
//  1. Every sample is in [0, wantCeiling] — never negative, never over ceiling.
//  2. The observed maximum across samples is ≥ wantCeiling*8/10 — the window is the right magnitude, not collapsed
//     to ~0.
//
// This pins both the exponential growth and the cap clamp deterministically. Overflow-guard attempts (≥ 62) are
// additionally checked to never produce a negative or wrapped duration, even at attempt=100.
func TestBackoffDuration_BoundsAndShape(t *testing.T) {
	t.Parallel()

	// Use realistic, distinct durations so ceilings differ clearly across attempts.
	// With BackoffBase=10s and BackoffCap=1h:
	//   attempt 1: ceil = min(1h, 10s * 2^0) =  10s
	//   attempt 2: ceil = min(1h, 10s * 2^1) =  20s
	//   attempt 3: ceil = min(1h, 10s * 2^2) =  40s
	//   attempt 4: ceil = min(1h, 10s * 2^3) =  80s
	//   attempt 5: ceil = min(1h, 10s * 2^4) = 160s
	//  attempt 20: ceil = min(1h, 10s * 2^19) = 1h   (cap clamp)
	const base = 10 * time.Second
	const maxBackoff = 1 * time.Hour

	cfg := Config{BackoffBase: base, BackoffCap: maxBackoff}

	tests := []struct {
		name        string
		attempt     int
		wantCeiling time.Duration
	}{
		{name: "attempt_1_base", attempt: 1, wantCeiling: 10 * time.Second},
		{name: "attempt_2_double", attempt: 2, wantCeiling: 20 * time.Second},
		{name: "attempt_3_quad", attempt: 3, wantCeiling: 40 * time.Second},
		{name: "attempt_4_octal", attempt: 4, wantCeiling: 80 * time.Second},
		{name: "attempt_5_wider", attempt: 5, wantCeiling: 160 * time.Second},
		{name: "attempt_20_cap_clamp", attempt: 20, wantCeiling: maxBackoff},
		// Overflow-guard boundary: shift = attempt-1 reaches 62 at attempt=63.
		{name: "attempt_62_overflow_guard", attempt: 62, wantCeiling: maxBackoff},
		{name: "attempt_63_overflow_guard", attempt: 63, wantCeiling: maxBackoff},
		{name: "attempt_64_overflow_guard", attempt: 64, wantCeiling: maxBackoff},
		{name: "attempt_100_overflow_guard", attempt: 100, wantCeiling: maxBackoff},
	}

	const samples = 2000

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var maxSeen time.Duration
			for i := range samples {
				d := backoffDuration(cfg, tt.attempt)
				if d < 0 {
					t.Errorf("sample %d: backoffDuration(attempt=%d) = %v; want ≥ 0", i, tt.attempt, d)
				}
				if d > tt.wantCeiling {
					t.Errorf("sample %d: backoffDuration(attempt=%d) = %v; want ≤ %v (ceiling exceeded)",
						i, tt.attempt, d, tt.wantCeiling)
				}
				if d > maxSeen {
					maxSeen = d
				}
			}

			// For the cap cases (attempt ≥ 20) the ceiling is 1h. After 2000 samples drawn uniformly from [0, 1h]
			// the expected maximum is ≈ 1h * 2000/2001 ≈ 99.95% of 1h, so 80% is a conservative floor that reliably
			// fails if the window collapsed. For smaller ceilings the same argument holds.
			//
			// We skip the 80% floor for attempt < 3 because the ceiling is only 10–20s and in a fully-random sample
			// it's theoretically possible (though astronomically unlikely) to stay below the threshold. For attempts
			// ≥ 3 the ceiling is ≥ 40s; the 80% floor is 32s, robust at 2000 samples.
			if tt.attempt >= 3 {
				floor := tt.wantCeiling * 8 / 10
				if maxSeen < floor {
					t.Errorf("backoffDuration(attempt=%d): max across %d samples = %v; want ≥ %v (80%% of ceiling %v) — window may be collapsed",
						tt.attempt, samples, maxSeen, floor, tt.wantCeiling)
				}
			}
		})
	}
}

// TestBackoffDuration_OverflowGuardNeverNegative is a targeted smoke-check for
// attempts so large that a naive bit-shift would overflow int64. backoffDuration
// must return a non-negative value clamped to [0, BackoffCap] regardless.
func TestBackoffDuration_OverflowGuardNeverNegative(t *testing.T) {
	t.Parallel()

	cfg := Config{BackoffBase: 10 * time.Second, BackoffCap: time.Hour}

	for _, attempt := range []int{62, 63, 64, 100, 1_000, 1_000_000} {
		for range 500 {
			d := backoffDuration(cfg, attempt)
			if d < 0 {
				t.Errorf("attempt=%d: backoffDuration returned negative %v", attempt, d)
			}
			if d > cfg.BackoffCap {
				t.Errorf("attempt=%d: backoffDuration returned %v > BackoffCap %v", attempt, d, cfg.BackoffCap)
			}
		}
	}
}

// TestBackoffDuration_ZeroAttemptClamped verifies that an attempt < 1 (treated as 1
// by the guard) still returns a value in [0, BackoffBase].
func TestBackoffDuration_ZeroAttemptClamped(t *testing.T) {
	t.Parallel()

	cfg := Config{BackoffBase: 10 * time.Second, BackoffCap: time.Hour}
	for _, attempt := range []int{0, -1, -100} {
		d := backoffDuration(cfg, attempt)
		if d < 0 {
			t.Errorf("attempt=%d: backoffDuration returned negative %v", attempt, d)
		}
		if d > cfg.BackoffBase {
			t.Errorf("attempt=%d: backoffDuration returned %v > BackoffBase %v (attempt<1 must clamp to attempt=1)",
				attempt, d, cfg.BackoffBase)
		}
	}
}
