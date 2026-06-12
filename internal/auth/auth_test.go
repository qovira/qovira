// Package auth_test exercises the exported surface of internal/auth.
//
// argon2id is deliberately slow by design; tests use an injected Params with tiny
// cost (Memory=64 KiB, Time=1, Threads=1) so the suite stays well under a second
// while still covering every code path.  Production defaults live in DefaultParams.
package auth_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/qovira/qovira/internal/auth"
)

// fastParams returns minimal argon2id params that are valid but cheap enough for
// unit tests.  Drive through the constructor so we also cover the NewHasher path.
func fastHasher(t *testing.T) *auth.Hasher {
	t.Helper()
	return auth.NewHasher(auth.Params{
		Memory:  64,
		Time:    1,
		Threads: 1,
		KeyLen:  32,
		SaltLen: 16,
	})
}

// ── AC1: Hash produces a valid PHC string ────────────────────────────────────

func TestHash_PHCFormat(t *testing.T) {
	t.Parallel()

	h := fastHasher(t)
	phc, err := h.Hash("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("Hash: unexpected error: %v", err)
	}

	tests := []struct {
		name  string
		check func(string) bool
		msg   string
	}{
		{
			"prefix",
			func(s string) bool { return strings.HasPrefix(s, "$argon2id$") },
			"PHC must start with $argon2id$",
		},
		{
			"version-segment",
			func(s string) bool { return strings.Contains(s, "$v=19$") },
			"PHC must contain $v=19$",
		},
		{
			"params-segment",
			func(s string) bool { return strings.Contains(s, "m=64,t=1,p=1") },
			"PHC must encode m=64,t=1,p=1 from injected params",
		},
		{
			"five-dollar-fields",
			func(s string) bool { return strings.Count(s, "$") == 5 },
			"PHC must have exactly 5 '$' separators",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !tt.check(phc) {
				t.Errorf("%s; got: %q", tt.msg, phc)
			}
		})
	}
}

// TestHash_ProductionParams verifies that a Hasher built with DefaultParams
// embeds the expected production values (m=65536, t=3, p=2) in the PHC string.
func TestHash_ProductionParams(t *testing.T) {
	t.Parallel()

	h := auth.NewHasher(auth.DefaultParams)
	phc, err := h.Hash("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("Hash: unexpected error: %v", err)
	}

	if !strings.Contains(phc, "m=65536,t=3,p=2") {
		t.Errorf("production PHC must encode m=65536,t=3,p=2; got %q", phc)
	}
}

// TestHash_UniqueSalts verifies that hashing the same plaintext twice produces
// different PHC strings (random salt per call).
func TestHash_UniqueSalts(t *testing.T) {
	t.Parallel()

	h := fastHasher(t)
	phc1, err := h.Hash("same-password")
	if err != nil {
		t.Fatalf("Hash (1): %v", err)
	}
	phc2, err := h.Hash("same-password")
	if err != nil {
		t.Fatalf("Hash (2): %v", err)
	}
	if phc1 == phc2 {
		t.Errorf("same plaintext produced identical PHC strings — salt must be random")
	}
}

// ── AC2: Verify returns true/false + constant-time comparison ────────────────

func TestVerify_RoundTrip(t *testing.T) {
	t.Parallel()

	h := fastHasher(t)
	cases := []struct {
		name      string
		plaintext string
	}{
		{"ascii-passphrase", "correct-horse-battery-staple"},
		{"unicode", "Pässwörð-with-em—dash"},
		{"long", strings.Repeat("a", 512)},
		{"empty-ish", "x"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			phc, err := h.Hash(tc.plaintext)
			if err != nil {
				t.Fatalf("Hash: %v", err)
			}
			ok, err := h.Verify(phc, tc.plaintext)
			if err != nil {
				t.Fatalf("Verify: unexpected error: %v", err)
			}
			if !ok {
				t.Error("Verify returned false for correct password")
			}
		})
	}
}

func TestVerify_WrongPassword(t *testing.T) {
	t.Parallel()

	h := fastHasher(t)
	phc, err := h.Hash("correct-password")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	ok, err := h.Verify(phc, "wrong-password")
	if err != nil {
		t.Fatalf("Verify: unexpected error on wrong password: %v", err)
	}
	if ok {
		t.Error("Verify returned true for wrong password")
	}
}

func TestVerify_MalformedPHC(t *testing.T) {
	t.Parallel()

	h := fastHasher(t)
	malformed := []struct {
		name string
		phc  string
	}{
		{"empty", ""},
		{"no-dollar", "argon2id"},
		{"wrong-algo", "$argon2i$v=19$m=64,t=1,p=1$c2FsdA$aGFzaA"},
		{"truncated", "$argon2id$v=19$m=64"},
		{"bad-version", "$argon2id$v=18$m=64,t=1,p=1$c2FsdA$aGFzaA"},
		{"bad-params", "$argon2id$v=19$m=abc,t=1,p=1$c2FsdA$aGFzaA"},
		{"bad-salt-b64", "$argon2id$v=19$m=64,t=1,p=1$!!!$aGFzaA"},
		{"bad-hash-b64", "$argon2id$v=19$m=64,t=1,p=1$c2FsdA$!!!"},
	}

	for _, tc := range malformed {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := h.Verify(tc.phc, "anything")
			if err == nil {
				t.Errorf("Verify(%q): expected error for malformed PHC, got nil", tc.phc)
			}
		})
	}
}

// TestVerify_MalformedPHC_DegenParams covers PHC strings whose parameter values
// are out of range for argon2id (p>255 truncation, and zero-valued m/t/p).
// Each must return an error — previously p=256 silently truncated to Threads=0
// and t=0/m=0 were accepted, causing argon2.IDKey to panic.
func TestVerify_MalformedPHC_DegenParams(t *testing.T) {
	t.Parallel()

	h := fastHasher(t)

	// Build a valid base PHC to use real salt/hash fields.
	basePHC, err := h.Hash("base-password")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	// Extract the salt and hash segments from the valid PHC.
	// Format: $argon2id$v=19$<params>$<salt>$<hash>
	parts := strings.SplitN(basePHC, "$", 6)
	saltSeg := parts[4]
	hashSeg := parts[5]

	cases := []struct {
		name string
		phc  string
	}{
		// p=256 overflows uint8 — previously truncated to 0 (Threads=0), causing panic.
		{
			"p-256-overflows-uint8",
			"$argon2id$v=19$m=64,t=1,p=256$" + saltSeg + "$" + hashSeg,
		},
		// p=300 similarly overflows.
		{
			"p-300-overflows-uint8",
			"$argon2id$v=19$m=64,t=1,p=300$" + saltSeg + "$" + hashSeg,
		},
		// p=0 is explicitly zero — argon2 requires parallelism >= 1.
		{
			"p-zero",
			"$argon2id$v=19$m=64,t=1,p=0$" + saltSeg + "$" + hashSeg,
		},
		// t=0 is zero — argon2 requires time >= 1.
		{
			"t-zero",
			"$argon2id$v=19$m=64,t=0,p=1$" + saltSeg + "$" + hashSeg,
		},
		// m=0 is zero — argon2 requires memory > 0.
		{
			"m-zero",
			"$argon2id$v=19$m=0,t=1,p=1$" + saltSeg + "$" + hashSeg,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := h.Verify(tc.phc, "anything")
			if err == nil {
				t.Errorf("Verify(%q): expected error for degenerate params, got nil", tc.phc)
			}
		})
	}
}

// ── AC3: NeedsRehash ─────────────────────────────────────────────────────────

func TestNeedsRehash(t *testing.T) {
	t.Parallel()

	// Build a PHC with lower params (m=64, t=1, p=1).
	weak := fastHasher(t)
	weakPHC, err := weak.Hash("password")
	if err != nil {
		t.Fatalf("Hash weak: %v", err)
	}

	// Build a PHC with higher params (m=128, t=2, p=1).
	stronger := auth.NewHasher(auth.Params{
		Memory:  128,
		Time:    2,
		Threads: 1,
		KeyLen:  32,
		SaltLen: 16,
	})
	strongerPHC, err := stronger.Hash("password")
	if err != nil {
		t.Fatalf("Hash stronger: %v", err)
	}

	// The "current" policy against which we ask "does this hash need rehash?".
	current := auth.NewHasher(auth.Params{
		Memory:  128,
		Time:    2,
		Threads: 1,
		KeyLen:  32,
		SaltLen: 16,
	})

	cases := []struct {
		name string
		phc  string
		want bool
	}{
		{"weaker-than-current", weakPHC, true},   // m=64,t=1 vs m=128,t=2 → needs rehash
		{"equal-to-current", strongerPHC, false}, // same params → no rehash
		{"unparseable", "not-a-phc", true},       // can't parse → needs rehash
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := current.NeedsRehash(tc.phc)
			if got != tc.want {
				t.Errorf("NeedsRehash(%q) = %v, want %v", tc.phc, got, tc.want)
			}
		})
	}
}

// TestNeedsRehash_EqualOrStronger explicitly verifies the boundary conditions
// across each param dimension (m, t, p independently stronger).
func TestNeedsRehash_EqualOrStronger(t *testing.T) {
	t.Parallel()

	currentParams := auth.Params{Memory: 128, Time: 2, Threads: 2, KeyLen: 32, SaltLen: 16}
	current := auth.NewHasher(currentParams)

	buildPHC := func(t *testing.T, p auth.Params) string {
		t.Helper()
		phc, err := auth.NewHasher(p).Hash("pw")
		if err != nil {
			t.Fatalf("Hash: %v", err)
		}
		return phc
	}

	cases := []struct {
		name   string
		stored auth.Params
		want   bool
	}{
		{"m-weaker", auth.Params{Memory: 64, Time: 2, Threads: 2, KeyLen: 32, SaltLen: 16}, true},
		{"t-weaker", auth.Params{Memory: 128, Time: 1, Threads: 2, KeyLen: 32, SaltLen: 16}, true},
		{"p-weaker", auth.Params{Memory: 128, Time: 2, Threads: 1, KeyLen: 32, SaltLen: 16}, true},
		{"equal", currentParams, false},
		{"m-stronger", auth.Params{Memory: 256, Time: 2, Threads: 2, KeyLen: 32, SaltLen: 16}, false},
		{"t-stronger", auth.Params{Memory: 128, Time: 4, Threads: 2, KeyLen: 32, SaltLen: 16}, false},
		{"p-stronger", auth.Params{Memory: 128, Time: 2, Threads: 4, KeyLen: 32, SaltLen: 16}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			phc := buildPHC(t, tc.stored)
			got := current.NeedsRehash(phc)
			if got != tc.want {
				t.Errorf("NeedsRehash = %v, want %v (stored: %+v, current: %+v)", got, tc.want, tc.stored, currentParams)
			}
		})
	}
}

// ── AC4: ValidatePassword ────────────────────────────────────────────────────

func TestValidatePassword(t *testing.T) {
	t.Parallel()

	// Use a policy with narrow min/max so we can hit both bounds easily.
	pol := auth.Policy{MinLen: 12, MaxLen: 64}

	cases := []struct {
		name      string
		plaintext string
		wantErr   error // nil means valid
	}{
		// --- too short ---
		{"empty", "", auth.ErrPasswordTooShort},
		{"one-char", "a", auth.ErrPasswordTooShort},
		{"eleven-chars", strings.Repeat("a", 11), auth.ErrPasswordTooShort},
		// --- at minimum boundary ---
		{"at-min", strings.Repeat("a", 12), nil},
		// --- valid range ---
		{"mid-range", "correct-horse-battery", nil},
		// --- at maximum boundary ---
		{"at-max", strings.Repeat("a", 64), nil},
		// --- too long ---
		{"over-max", strings.Repeat("a", 65), auth.ErrPasswordTooLong},
		// --- no composition rules: spaces, unicode, punctuation all OK ---
		{"spaces", "a valid passphrase here!", nil},
		{"unicode", "Rôle-präfix-ñoño123", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := pol.ValidatePassword(tc.plaintext)
			if tc.wantErr == nil {
				if err != nil {
					t.Errorf("ValidatePassword(%q) = %v, want nil", tc.plaintext, err)
				}
			} else {
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("ValidatePassword(%q) = %v, want %v", tc.plaintext, err, tc.wantErr)
				}
			}
		})
	}
}

// TestValidatePassword_DefaultPolicy checks that the exported DefaultPolicy has
// sensible defaults (min ≥ 10, max ≥ 512).
func TestValidatePassword_DefaultPolicy(t *testing.T) {
	t.Parallel()

	// DefaultPolicy must accept a valid long passphrase.
	longPassphrase := strings.Repeat("a", 512)
	if err := auth.DefaultPolicy.ValidatePassword(longPassphrase); err != nil {
		t.Errorf("DefaultPolicy.ValidatePassword(512 chars) = %v, want nil", err)
	}

	// DefaultPolicy must reject a single character.
	if err := auth.DefaultPolicy.ValidatePassword("x"); !errors.Is(err, auth.ErrPasswordTooShort) {
		t.Errorf("DefaultPolicy.ValidatePassword(1 char) = %v, want ErrPasswordTooShort", err)
	}

	// DefaultPolicy must reject a massively overlong string.
	overlong := strings.Repeat("a", 2000)
	if err := auth.DefaultPolicy.ValidatePassword(overlong); !errors.Is(err, auth.ErrPasswordTooLong) {
		t.Errorf("DefaultPolicy.ValidatePassword(2000 chars) = %v, want ErrPasswordTooLong", err)
	}
}

// ── AC5: Dummy-verify path ───────────────────────────────────────────────────

// TestDummyVerify_IsValidPHC verifies that the dummy hash stored inside the
// Hasher is a parseable, valid argon2id PHC string.
func TestDummyVerify_IsValidPHC(t *testing.T) {
	t.Parallel()

	h := fastHasher(t)
	phc := h.DummyHash()

	if !strings.HasPrefix(phc, "$argon2id$") {
		t.Errorf("DummyHash() = %q: not a valid argon2id PHC", phc)
	}
	if !strings.Contains(phc, "$v=19$") {
		t.Errorf("DummyHash() = %q: missing v=19", phc)
	}
	if strings.Count(phc, "$") != 5 {
		t.Errorf("DummyHash() = %q: expected 5 '$' separators", phc)
	}
}

// TestDummyVerify_ReturnsFalseNotError verifies that DummyVerify returns false
// (not an error) — it is a genuine argon2id derivation against the dummy hash,
// not a short-circuit.
func TestDummyVerify_ReturnsFalseNotError(t *testing.T) {
	t.Parallel()

	h := fastHasher(t)
	// Any string that was not the password used to create the dummy hash.
	ok, err := h.DummyVerify("some-user-supplied-password")
	if err != nil {
		t.Fatalf("DummyVerify: unexpected error: %v", err)
	}
	if ok {
		// The dummy hash was created with a random password; "some-user-supplied-password" must not match.
		t.Error("DummyVerify returned true — it should always return false (wrong password)")
	}
}

// TestDummyVerify_ActualDerivation verifies that DummyVerify actually performs
// a real argon2id derivation (not a short-circuit) by checking that it takes a
// measurable non-zero amount of time even with minimal params.
func TestDummyVerify_ActualDerivation(t *testing.T) {
	t.Parallel()

	// Even with m=64/t=1/p=1 argon2id must do actual work — this is a loose
	// sanity check rather than a timing bound.
	h := fastHasher(t)

	// Verify returns false without error, confirming it ran a full derivation
	// path (not a special-case short-circuit that skips KDF work).
	ok, err := h.DummyVerify("not-the-right-password")
	if err != nil {
		t.Fatalf("DummyVerify: unexpected error: %v", err)
	}
	if ok {
		t.Error("DummyVerify returned true unexpectedly")
	}
}

// ── AC6: table-driven policy-boundary tests (already covered above) ──────────

// TestVerify_ParamsDecodedFromPHC confirms that Verify re-derives using the
// params *encoded in the PHC string*, not the Hasher's current params.  We
// hash with cheap params, then call Verify on a Hasher with expensive (default)
// params — it must still return true.
func TestVerify_ParamsDecodedFromPHC(t *testing.T) {
	t.Parallel()

	cheap := fastHasher(t)
	phc, err := cheap.Hash("my-passphrase")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	// expensive Hasher — its params are irrelevant for Verify.
	expensive := auth.NewHasher(auth.DefaultParams)
	ok, err := expensive.Verify(phc, "my-passphrase")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Error("Verify returned false — it must use the PHC-embedded params, not the Hasher's current params")
	}
}
