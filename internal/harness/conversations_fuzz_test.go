package harness

import (
	"testing"
	"unicode/utf8"
)

// FuzzDecodeConvCursor checks that decodeConvCursor never panics on arbitrary input. The conversations-list cursor
// arrives base64-encoded in an attacker-controlled query parameter, so malformed input must always surface as an error,
// never a crash, and a nil-error result must satisfy the decoder's own non-empty contract.
func FuzzDecodeConvCursor(f *testing.F) {
	f.Add("")
	f.Add("not valid base64 !!!")
	f.Add("e30")                                                  // base64 of "{}" — valid base64, empty fields
	f.Add(encodeConvCursor("2026-06-25T12:00:00Z", "conv_01abc")) // a well-formed cursor

	f.Fuzz(func(t *testing.T, cursor string) {
		updatedAt, id, err := decodeConvCursor(cursor)
		if err != nil {
			return // any error is acceptable; the contract is "no panic".
		}
		// On a nil-error decode the decoder guarantees both keyset fields are present.
		if updatedAt == "" || id == "" {
			t.Errorf("decodeConvCursor(%q) returned nil error with an empty field: updatedAt=%q id=%q",
				cursor, updatedAt, id)
		}
	})
}

// FuzzConvCursorRoundTrip checks that encodeConvCursor and decodeConvCursor are inverses: for any non-empty,
// valid-UTF-8 pair, decode(encode(u, i)) == (u, i). Empty or invalid-UTF-8 inputs are skipped — the decoder rejects
// empty fields by contract, and json.Marshal replaces invalid UTF-8 with U+FFFD (which cannot round-trip), neither of
// which the encoder is ever fed in practice.
func FuzzConvCursorRoundTrip(f *testing.F) {
	f.Add("2026-06-25T12:00:00Z", "conv_01abc")
	f.Add("u", "i")

	f.Fuzz(func(t *testing.T, updatedAt, id string) {
		if updatedAt == "" || id == "" || !utf8.ValidString(updatedAt) || !utf8.ValidString(id) {
			t.Skip()
		}

		gotUpdatedAt, gotID, err := decodeConvCursor(encodeConvCursor(updatedAt, id))
		if err != nil {
			t.Fatalf("round-trip decode failed for (%q, %q): %v", updatedAt, id, err)
		}
		if gotUpdatedAt != updatedAt || gotID != id {
			t.Errorf("round-trip mismatch: encoded (%q, %q), decoded (%q, %q)",
				updatedAt, id, gotUpdatedAt, gotID)
		}
	})
}
