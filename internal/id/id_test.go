package id_test

import (
	"sort"
	"sync"
	"testing"

	"github.com/qovira/qovira/internal/id"
)

// crockfordAlphabet is the set of valid characters in a Crockford base32 ULID.
// ULIDs must NOT contain I, L, O, or U.
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// isValidCrockford reports whether every rune in s belongs to the Crockford
// base32 alphabet.
func isValidCrockford(s string) bool {
	for _, c := range s {
		found := false
		for _, v := range crockfordAlphabet {
			if c == v {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// TestNew_Length verifies that every generated id is exactly 26 characters.
func TestNew_Length(t *testing.T) {
	t.Parallel()

	for range 100 {
		got := id.New()
		if len(got) != 26 {
			t.Errorf("id.New() = %q (len %d), want 26-char ULID", got, len(got))
		}
	}
}

// TestNew_CrockfordAlphabet verifies that generated ids only contain valid
// Crockford base32 characters (no I, L, O, or U).
func TestNew_CrockfordAlphabet(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
	}{
		{"sample-1"},
		{"sample-2"},
		{"sample-3"},
		{"sample-4"},
		{"sample-5"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := id.New()
			if len(got) != 26 {
				t.Errorf("id %q: want len 26, got %d", got, len(got))
			}
			if !isValidCrockford(got) {
				t.Errorf("id %q: contains character(s) outside Crockford base32 alphabet", got)
			}
			// Explicitly check the forbidden characters.
			for _, bad := range []rune{'I', 'L', 'O', 'U'} {
				for _, c := range got {
					if c == bad {
						t.Errorf("id %q: contains forbidden Crockford character %c", got, bad)
					}
				}
			}
		})
	}
}

// TestNew_Monotonic generates a large sequence of ids in a tight loop and
// asserts that they are already sorted in lexicographic order (monotonic) with
// no duplicates.  Because the loop is tight, many ids will share the same
// millisecond, exercising the within-millisecond monotonic increment.
func TestNew_Monotonic(t *testing.T) {
	t.Parallel()

	const n = 10_000
	ids := make([]string, n)
	for i := range n {
		ids[i] = id.New()
	}

	// Assert sorted order — the slice must already be in ascending
	// lexicographic order (monotonic generation guarantee).
	for i := 1; i < n; i++ {
		if ids[i] <= ids[i-1] {
			t.Fatalf("ids not monotonically increasing at index %d: %q <= %q", i, ids[i], ids[i-1])
		}
	}

	// Assert no duplicates (strict increase implies uniqueness, but be explicit).
	seen := make(map[string]struct{}, n)
	for _, v := range ids {
		if _, dup := seen[v]; dup {
			t.Fatalf("duplicate id generated: %q", v)
		}
		seen[v] = struct{}{}
	}

	// Double-check with sort.SliceIsSorted to make the failure message clearer
	// in case the monotonic property is violated.
	if !sort.StringsAreSorted(ids) {
		t.Fatal("id sequence is not sorted — monotonic property violated")
	}
}

// TestNew_Concurrent exercises concurrent callers to verify:
//  1. All ids are unique (no collisions).
//  2. Per-goroutine sequences are monotonically increasing.
//  3. There are no data races (enforced by the -race flag in CI/make race).
func TestNew_Concurrent(t *testing.T) {
	t.Parallel()

	const (
		goroutines   = 32
		perGoroutine = 500
	)

	results := make([][]string, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := range goroutines {
		go func() {
			defer wg.Done()
			local := make([]string, perGoroutine)
			for i := range perGoroutine {
				local[i] = id.New()
			}
			results[g] = local
		}()
	}
	wg.Wait()

	// Collect all ids and verify uniqueness across all goroutines.
	all := make(map[string]struct{}, goroutines*perGoroutine)
	for g, slice := range results {
		for _, v := range slice {
			if _, dup := all[v]; dup {
				t.Fatalf("goroutine %d produced duplicate id: %q", g, v)
			}
			all[v] = struct{}{}
		}

		// Per-goroutine slice must be monotonically increasing.
		for i := 1; i < len(slice); i++ {
			if slice[i] <= slice[i-1] {
				t.Fatalf("goroutine %d: ids not monotonically increasing at index %d: %q <= %q",
					g, i, slice[i], slice[i-1])
			}
		}
	}
}

// TestGenerator_New verifies that a Generator constructed via NewGenerator
// produces valid 26-char Crockford ULIDs, enabling injectable generators for
// tests that need a deterministic entropy source.
func TestGenerator_New(t *testing.T) {
	t.Parallel()

	g := id.NewGenerator()
	got := g.New()
	if len(got) != 26 {
		t.Errorf("Generator.New() = %q (len %d), want 26", got, len(got))
	}
	if !isValidCrockford(got) {
		t.Errorf("Generator.New() = %q: contains character(s) outside Crockford base32 alphabet", got)
	}
}
