package httpx

import (
	"context"
	"crypto/rand"
	"net/http"
	"regexp"
)

// requestIDContextKey keys the request ID in context; a named type avoids collisions with other packages.
type requestIDContextKey struct{}

// safeHeaderValueRE matches values that are safe to echo as HTTP header values: printable ASCII only
// (0x21–0x7E) plus space and horizontal tab, which are the octets permitted by RFC 9110 §5.5 for field
// values. The requirement here is tighter — we also rule out non-ASCII and length extremes so that a
// poisoned inbound Request-Id cannot inject a newline, pad the log, or trigger header smuggling.
var safeHeaderValueRE = regexp.MustCompile(`^[!-~][ \t!-~]{0,127}$`)

// maxRequestIDLen is the upper bound on an acceptable inbound Request-Id. A Crockford-base32 token of
// 10 random bytes encodes to 16 characters, so 128 chars leaves ample room for even long client-chosen
// IDs while preventing padding/log-flooding attacks.
const maxRequestIDLen = 128

// crockfordAlphabet is the Crockford base32 alphabet (digits + uppercase letters excluding I, L, O, U).
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// generateRequestID returns a "req_"-prefixed Crockford-base32 token backed by crypto/rand. It panics
// only if crypto/rand is unavailable (a fatal system error); callers may treat it as infallible.
func generateRequestID() string {
	const tokenBytes = 10 // 10 random bytes → 16-character Crockford base32 token → "req_XXXXXXXXXXXXXXXX"

	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand failure is fatal: panic rather than hand back a low-entropy token that looks random.
		panic("httpx: crypto/rand unavailable: " + err.Error())
	}

	// Encode each 5-bit group as a Crockford alphabet character. 10 bytes = 80 bits = 16 five-bit groups.
	const bitsPerChar = 5
	const mask = (1 << bitsPerChar) - 1

	result := make([]byte, 4+16) // "req_" + 16 characters
	copy(result, "req_")

	var acc uint64
	var bits int
	pos := 4

	for _, b := range raw {
		acc = (acc << 8) | uint64(b)
		bits += 8

		for bits >= bitsPerChar {
			bits -= bitsPerChar
			result[pos] = crockfordAlphabet[(acc>>uint(bits))&uint64(mask)]
			pos++
		}
	}

	return string(result)
}

// isWellFormedRequestID reports whether v is a valid inbound Request-Id: non-empty, within the length
// bound, and containing only printable ASCII characters that are safe to echo in a response header and
// log. This deliberately does NOT require the "req_" prefix — a client's own correlation ID (e.g. from
// a load balancer or another service) is echoed as-is when it passes these guards.
func isWellFormedRequestID(v string) bool {
	return len(v) > 0 && len(v) <= maxRequestIDLen && safeHeaderValueRE.MatchString(v)
}

// NewRequestIDMiddleware returns a middleware that:
//   - Accepts a well-formed inbound Request-Id header (bounded length + printable-ASCII guard) and places
//     it in the request context unchanged, echoing it on the response.
//   - Generates a "req_"-prefixed Crockford-base32 token when no inbound header is present or when the
//     inbound value fails the guard.
//
// The resolved ID is always echoed as the Request-Id response header and is accessible via
// [RequestID] on the request's context.
func NewRequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("Request-Id")
		if !isWellFormedRequestID(id) {
			id = generateRequestID()
		}

		// Set the response header before calling the next handler so that downstream middleware (e.g.
		// the access logger) and the inner handler can rely on the header being present.
		w.Header().Set("Request-Id", id)

		ctx := context.WithValue(r.Context(), requestIDContextKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestID retrieves the request ID from ctx. It returns "" when ctx carries no request ID (i.e. the
// request-ID middleware was not in the chain). Downstream handlers — the access logger, the error edge —
// use this to correlate log lines with the Request-Id response header.
func RequestID(ctx context.Context) string {
	v, _ := ctx.Value(requestIDContextKey{}).(string)
	return v
}
