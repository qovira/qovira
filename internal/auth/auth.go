// Package auth implements the password-credential core for Qovira.
//
// This package is intentionally self-contained: it has no database/sql or net/http imports.
// All parameters are injected through [Params] and [Policy] structs so the caller (an identity
// service constructed in a later slice) can supply production values from its own config.
//
// # Hashing
//
// [Hasher] wraps argon2id ([golang.org/x/crypto/argon2.IDKey]).  [Hash] produces a PHC-formatted
// string; [Verify] decodes the PHC and re-derives in constant time.
//
// # PHC string format
//
//	$argon2id$v=19$m=<memory>,t=<time>,p=<threads>$<base64-salt>$<base64-hash>
//
// Salt and hash are encoded with [base64.RawStdEncoding] (no padding), matching the PHC
// convention.
//
// # Dummy-verify
//
// [Hasher.DummyVerify] performs a real argon2id derivation so the unknown-email login path
// costs the same wall-clock time as the wrong-password path, defeating user-enumeration
// timing oracles.
//
// # Password policy
//
// [Policy] enforces minimum/maximum UTF-8 rune length with no composition rules.
// [ValidatePassword] returns a typed sentinel ([ErrPasswordTooShort] / [ErrPasswordTooLong])
// so a later HTTP slice can map the error to a 422 with a JSON Pointer.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// phcVersion is the argon2 version encoded in PHC strings.  argon2.IDKey always produces
// output for version 19 (0x13).
const phcVersion = 19

// ── Params ───────────────────────────────────────────────────────────────────

// Params holds the argon2id KDF parameters and salt/key lengths.  Treat it as an injected
// value — the identity service that constructs a [Hasher] is responsible for mapping its
// own config here.
//
// The zero value is not valid; use [DefaultParams] or populate every field explicitly.
type Params struct {
	// Memory is the memory cost in KiB.  Must be > 0.  Production default: 65536 (64 MiB).
	Memory uint32

	// Time is the number of iterations (time cost).  Must be > 0.  Production default: 3.
	Time uint32

	// Threads is the parallelism factor.  Must be > 0.  Production default: 2.
	Threads uint8

	// KeyLen is the byte length of the derived key.  Must be > 0.  Production default: 32.
	KeyLen uint32

	// SaltLen is the byte length of the random salt.  Must be > 0.  Production default: 16.
	SaltLen uint32
}

// DefaultParams are the production argon2id parameters recommended by the Qovira design:
//   - Memory: 65536 KiB (64 MiB)
//   - Time:   3 iterations
//   - Threads: 2
//   - KeyLen:  32 bytes
//   - SaltLen: 16 bytes
var DefaultParams = Params{
	Memory:  65536,
	Time:    3,
	Threads: 2,
	KeyLen:  32,
	SaltLen: 16,
}

// ── Hasher ───────────────────────────────────────────────────────────────────

// Hasher wraps argon2id with an injected [Params] set.  Construct it via [NewHasher].
// The zero value is not valid.
type Hasher struct {
	params    Params
	dummyHash string // precomputed PHC for the dummy-verify path
}

// NewHasher constructs a Hasher parameterised by p.  It eagerly computes a dummy PHC
// hash (from a random password + random salt) so the [DummyVerify] path is ready without
// any per-call overhead.
//
// NewHasher panics if the OS entropy source is unavailable when generating the dummy hash;
// that is a fatal system error and not recoverable.
func NewHasher(p Params) *Hasher {
	phc := mustDummyHash(p)
	return &Hasher{params: p, dummyHash: phc}
}

// mustDummyHash creates a genuine argon2id PHC hash from a random 32-byte password so the
// dummy-verify path incurs a full KDF round.  Panics on entropy failure.
func mustDummyHash(p Params) string {
	// Random password so the dummy PHC is unpredictable.
	randomPwd := make([]byte, 32)
	if _, err := rand.Read(randomPwd); err != nil {
		panic("auth: crypto/rand unavailable: " + err.Error())
	}
	phc, err := hashWithParams(p, string(randomPwd))
	if err != nil {
		panic("auth: failed to compute dummy hash: " + err.Error())
	}
	return phc
}

// Hash derives an argon2id hash of plaintext and returns it as a PHC string.
// A fresh random salt is generated for every call.
func (h *Hasher) Hash(plaintext string) (string, error) {
	return hashWithParams(h.params, plaintext)
}

// hashWithParams is the internal helper shared by Hash and NewHasher.
func hashWithParams(p Params, plaintext string) (string, error) {
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate salt: %w", err)
	}

	key := argon2.IDKey([]byte(plaintext), salt, p.Time, p.Memory, p.Threads, p.KeyLen)

	// PHC format uses base64.RawStdEncoding (no padding).
	enc := base64.RawStdEncoding
	saltB64 := enc.EncodeToString(salt)
	keyB64 := enc.EncodeToString(key)

	phc := fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		phcVersion,
		p.Memory,
		p.Time,
		p.Threads,
		saltB64,
		keyB64,
	)
	return phc, nil
}

// Verify decodes the parameters and salt from phc, re-derives the key for plaintext,
// and compares using [crypto/subtle.ConstantTimeCompare].
//
// Returns (false, nil) when the password does not match.
// Returns (false, error) when phc is malformed or unparseable.
func (h *Hasher) Verify(phc, plaintext string) (bool, error) {
	p, salt, storedKey, err := parsePHC(phc)
	if err != nil {
		return false, err
	}

	derived := argon2.IDKey([]byte(plaintext), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	match := subtle.ConstantTimeCompare(derived, storedKey) == 1
	return match, nil
}

// NeedsRehash reports whether the params embedded in phc are weaker than the Hasher's
// current params (i.e. any of Memory, Time, or Threads is strictly less than the current
// value).  Returns true for an unparseable PHC so the caller can rehash on the next login.
func (h *Hasher) NeedsRehash(phc string) bool {
	stored, _, _, err := parsePHC(phc)
	if err != nil {
		// Can't parse → treat as needing rehash.
		return true
	}
	cur := h.params
	// KeyLen and SaltLen are intentionally omitted: they affect output size, not KDF hardness.
	// A change to either requires an explicit migration, not an opportunistic rehash.
	return stored.Memory < cur.Memory || stored.Time < cur.Time || stored.Threads < cur.Threads
}

// DummyHash returns the precomputed dummy PHC string.  Exposed for tests that need to
// assert it is a valid argon2id PHC.
func (h *Hasher) DummyHash() string {
	return h.dummyHash
}

// DummyVerify performs a real argon2id verification against the precomputed dummy PHC for
// an unknown email, so the unknown-email and wrong-password login paths cost the same KDF
// work.  Always returns (false, nil) on a correctly formed dummy hash; an error is
// returned only if the dummy hash is somehow unparseable (which would be a programmer error
// caught in tests).
func (h *Hasher) DummyVerify(plaintext string) (bool, error) {
	return h.Verify(h.dummyHash, plaintext)
}

// ── PHC parsing ──────────────────────────────────────────────────────────────

// parsePHC decodes a PHC string of the form:
//
//	$argon2id$v=19$m=<M>,t=<T>,p=<P>$<b64salt>$<b64hash>
//
// It returns the embedded Params (Memory/Time/Threads; KeyLen is derived from the decoded
// key length; SaltLen is derived from the decoded salt length), the raw salt bytes, and the
// raw derived-key bytes.
func parsePHC(phc string) (p Params, salt, key []byte, err error) {
	// Expected structure after splitting on '$':
	//   [0] ""             (the leading $)
	//   [1] "argon2id"
	//   [2] "v=19"
	//   [3] "m=65536,t=3,p=2"
	//   [4] <b64salt>
	//   [5] <b64hash>
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[0] != "" {
		return p, nil, nil, fmt.Errorf("auth: malformed PHC string: want 6 '$'-separated fields")
	}

	if parts[1] != "argon2id" {
		return p, nil, nil, fmt.Errorf("auth: unsupported algorithm %q (want argon2id)", parts[1])
	}

	// Version segment: "v=19"
	vStr, ok := strings.CutPrefix(parts[2], "v=")
	if !ok {
		return p, nil, nil, fmt.Errorf("auth: missing version field in PHC")
	}
	v, convErr := strconv.Atoi(vStr)
	if convErr != nil || v != phcVersion {
		return p, nil, nil, fmt.Errorf("auth: unsupported argon2id version %q (want v=%d)", parts[2], phcVersion)
	}

	// Params segment: "m=<M>,t=<T>,p=<P>"
	if err = parseParamsSegment(parts[3], &p); err != nil {
		return p, nil, nil, err
	}

	// Salt (RawStdEncoding, no padding).
	enc := base64.RawStdEncoding
	salt, err = enc.DecodeString(parts[4])
	if err != nil {
		return p, nil, nil, fmt.Errorf("auth: decode salt: %w", err)
	}
	p.SaltLen = uint32(len(salt)) //nolint:gosec // len(salt) is bounded by the PHC-encoded base64 length; no overflow possible

	// Key (RawStdEncoding, no padding).
	key, err = enc.DecodeString(parts[5])
	if err != nil {
		return p, nil, nil, fmt.Errorf("auth: decode key: %w", err)
	}
	p.KeyLen = uint32(len(key)) //nolint:gosec // len(key) is bounded by the PHC-encoded base64 length; no overflow possible

	return p, salt, key, nil
}

// parseParamsSegment parses the "m=<M>,t=<T>,p=<P>" segment into *p.
func parseParamsSegment(seg string, p *Params) error {
	// The segment contains exactly three comma-separated key=value pairs in the
	// canonical order m, t, p.  We accept them in that order only, matching the
	// PHC spec's argon2id encoding.
	fields := strings.Split(seg, ",")
	if len(fields) != 3 {
		return fmt.Errorf("auth: params segment %q: want m=<M>,t=<T>,p=<P>", seg)
	}

	// parseField parses a "key=value" pair, requiring the value to fit in bitSize bits and
	// to be strictly greater than zero (argon2id requires positive m, t, and p).
	parseField := func(kv, prefix string, bitSize int) (uint64, error) {
		val, ok := strings.CutPrefix(kv, prefix)
		if !ok {
			return 0, fmt.Errorf("auth: params segment: expected %s..., got %q", prefix, kv)
		}
		n, err := strconv.ParseUint(val, 10, bitSize)
		if err != nil {
			return 0, fmt.Errorf("auth: params segment: %s value %q: %w", prefix, val, err)
		}
		if n == 0 {
			return 0, fmt.Errorf("auth: params segment: %s value must be > 0, got 0", prefix)
		}
		return n, nil
	}

	m, err := parseField(fields[0], "m=", 32)
	if err != nil {
		return err
	}
	tt, err := parseField(fields[1], "t=", 32)
	if err != nil {
		return err
	}
	// Threads is uint8 in argon2 (max 255); parse with bit size 8 so p=256 is a parse
	// error rather than silently truncating to 0, which would panic inside argon2.IDKey.
	pVal, err := parseField(fields[2], "p=", 8)
	if err != nil {
		return err
	}

	p.Memory = uint32(m)    //nolint:gosec // bounded to uint32 by ParseUint bit-size argument
	p.Time = uint32(tt)     //nolint:gosec // bounded to uint32 by ParseUint bit-size argument
	p.Threads = uint8(pVal) //nolint:gosec // bounded to uint8 by ParseUint bit-size argument; > 0 checked above
	return nil
}

// ── Password policy ───────────────────────────────────────────────────────────

// ErrPasswordTooShort is returned by [Policy.ValidatePassword] when the plaintext is
// shorter than [Policy.MinLen] UTF-8 runes.  Use [errors.Is] to check.
var ErrPasswordTooShort = errors.New("password is too short")

// ErrPasswordTooLong is returned by [Policy.ValidatePassword] when the plaintext is
// longer than [Policy.MaxLen] UTF-8 runes.  Use [errors.Is] to check.
var ErrPasswordTooLong = errors.New("password is too long")

// Policy carries the password validation rules.  There are deliberately no composition
// rules (uppercase, digits, symbols) — length only, per the OWASP guidance embedded in
// the Qovira design.
//
// Use [DefaultPolicy] for the production defaults or construct your own for tests.
type Policy struct {
	// MinLen is the minimum number of UTF-8 runes required.  Must be > 0.
	// Production default: 12.
	MinLen int

	// MaxLen is the maximum number of UTF-8 runes allowed.  A generous upper bound
	// (e.g. 1024) admits passphrases and password-manager output while bounding abuse.
	// Production default: 1024.
	MaxLen int
}

// DefaultPolicy is the production password policy: min 12, max 1024 runes.
var DefaultPolicy = Policy{MinLen: 12, MaxLen: 1024}

// ValidatePassword checks that plaintext satisfies the policy length bounds.
// It counts UTF-8 runes (not bytes) so multi-byte characters count once.
//
// Returns [ErrPasswordTooShort] or [ErrPasswordTooLong] when the constraint is violated;
// nil when the password is acceptable.  No composition rules are applied.
func (pol Policy) ValidatePassword(plaintext string) error {
	n := utf8.RuneCountInString(plaintext)
	if n < pol.MinLen {
		return ErrPasswordTooShort
	}
	if n > pol.MaxLen {
		return ErrPasswordTooLong
	}
	return nil
}
