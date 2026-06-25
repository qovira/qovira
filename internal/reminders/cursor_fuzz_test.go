package reminders

import (
	"testing"
	"unicode/utf8"
)

// FuzzDecodeCursor checks that decodeCursor never panics on arbitrary input. The reminders-list cursor arrives
// base64-encoded in an attacker-controlled query parameter, so malformed input must always surface as an error, never a
// crash, and a nil-error result must satisfy the decoder's own non-empty contract.
func FuzzDecodeCursor(f *testing.F) {
	f.Add("")
	f.Add("not valid base64 !!!")
	f.Add("e30")                                             // base64 of "{}" — valid base64, empty fields
	f.Add(encodeCursor("2026-06-25T12:00:00Z", "rem_01abc")) // a well-formed cursor

	f.Fuzz(func(t *testing.T, cursor string) {
		dueAt, id, err := decodeCursor(cursor)
		if err != nil {
			return // any error is acceptable; the contract is "no panic".
		}
		// On a nil-error decode the decoder guarantees both keyset fields are present.
		if dueAt == "" || id == "" {
			t.Errorf("decodeCursor(%q) returned nil error with an empty field: dueAt=%q id=%q", cursor, dueAt, id)
		}
	})
}

// FuzzCursorRoundTrip checks that encodeCursor and decodeCursor are inverses: for any non-empty, valid-UTF-8 pair,
// decode(encode(d, i)) == (d, i). Empty or invalid-UTF-8 inputs are skipped — the decoder rejects empty fields by
// contract, and json.Marshal replaces invalid UTF-8 with U+FFFD (which cannot round-trip), neither of which the encoder
// is ever fed in practice.
func FuzzCursorRoundTrip(f *testing.F) {
	f.Add("2026-06-25T12:00:00Z", "rem_01abc")
	f.Add("d", "i")

	f.Fuzz(func(t *testing.T, dueAt, id string) {
		if dueAt == "" || id == "" || !utf8.ValidString(dueAt) || !utf8.ValidString(id) {
			t.Skip()
		}

		gotDueAt, gotID, err := decodeCursor(encodeCursor(dueAt, id))
		if err != nil {
			t.Fatalf("round-trip decode failed for (%q, %q): %v", dueAt, id, err)
		}
		if gotDueAt != dueAt || gotID != id {
			t.Errorf("round-trip mismatch: encoded (%q, %q), decoded (%q, %q)", dueAt, id, gotDueAt, gotID)
		}
	})
}
