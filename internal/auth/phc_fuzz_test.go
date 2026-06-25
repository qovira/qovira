package auth

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"testing"
)

// FuzzParsePHC checks that parsePHC never panics on arbitrary input and that a nil-error parse upholds the invariants
// the rest of the package relies on. A stored PHC string is parser input on every login, so a malformed or adversarial
// value must surface as an error — never a crash, and never a "success" with degenerate parameters (e.g. a zero cost
// that would later panic inside argon2.IDKey).
func FuzzParsePHC(f *testing.F) {
	enc := base64.RawStdEncoding
	validPHC := fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=2$%s$%s",
		enc.EncodeToString([]byte("0123456789abcdef")), enc.EncodeToString(make([]byte, 32)))

	f.Add(validPHC)
	f.Add("")
	f.Add("$argon2id$v=19$m=0,t=3,p=2$c2FsdA$a2V5")       // zero memory cost
	f.Add("$argon2id$v=18$m=65536,t=3,p=2$c2FsdA$a2V5")   // unsupported version
	f.Add("$argon2id$v=19$m=65536,t=3,p=256$c2FsdA$a2V5") // threads overflow uint8
	f.Add("$argon2id$v=19$m=65536,t=3,p=2$!!!$a2V5")      // invalid base64 salt

	f.Fuzz(func(t *testing.T, phc string) {
		p, salt, key, err := parsePHC(phc)
		if err != nil {
			return // any error is acceptable; the contract is "no panic".
		}
		// Documented invariants on a successful parse.
		if p.Memory == 0 || p.Time == 0 || p.Threads == 0 {
			t.Errorf("parsePHC(%q) succeeded with a non-positive cost parameter: %+v", phc, p)
		}
		if int(p.SaltLen) != len(salt) {
			t.Errorf("parsePHC(%q): SaltLen=%d but len(salt)=%d", phc, p.SaltLen, len(salt))
		}
		if int(p.KeyLen) != len(key) {
			t.Errorf("parsePHC(%q): KeyLen=%d but len(key)=%d", phc, p.KeyLen, len(key))
		}
	})
}

// FuzzParsePHCRoundTrip checks that parsePHC is the inverse of the PHC encoding hashWithParams produces: a string built
// from well-formed parameters and arbitrary salt/key bytes parses back to the same values. This exercises the parser
// across the full space of cost parameters and salt/key lengths without paying for an argon2 derivation per input.
func FuzzParsePHCRoundTrip(f *testing.F) {
	f.Add(uint32(65536), uint32(3), uint8(2), []byte("0123456789abcdef"), make([]byte, 32))
	f.Add(uint32(1), uint32(1), uint8(1), []byte("s"), []byte("k"))

	f.Fuzz(func(t *testing.T, mem, iter uint32, par uint8, salt, key []byte) {
		// parsePHC requires strictly positive cost parameters; skip the inputs the encoder would never produce.
		if mem == 0 || iter == 0 || par == 0 {
			t.Skip()
		}
		enc := base64.RawStdEncoding
		phc := fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
			mem, iter, par, enc.EncodeToString(salt), enc.EncodeToString(key))

		p, gotSalt, gotKey, err := parsePHC(phc)
		if err != nil {
			t.Fatalf("parsePHC(%q) failed for well-formed input: %v", phc, err)
		}
		if p.Memory != mem || p.Time != iter || p.Threads != par {
			t.Errorf("param round-trip mismatch: encoded m=%d,t=%d,p=%d; got m=%d,t=%d,p=%d",
				mem, iter, par, p.Memory, p.Time, p.Threads)
		}
		if !bytes.Equal(gotSalt, salt) {
			t.Errorf("salt round-trip mismatch: encoded %x, got %x", salt, gotSalt)
		}
		if !bytes.Equal(gotKey, key) {
			t.Errorf("key round-trip mismatch: encoded %x, got %x", key, gotKey)
		}
	})
}
