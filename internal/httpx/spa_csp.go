package httpx

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"strings"
)

// The embedded SPA's index.html carries first-party inline <script> elements that a strict "script-src 'self'" would
// block: SvelteKit's bootstrap (which imports the start/app chunks and calls kit.start) and the pre-paint theme boot.
// Neither can be served as an external file without regressing the SPA (the bootstrap is framework-generated; the theme
// boot must run before first paint). The blessed way to admit a known inline script under a strict CSP is its SHA-256
// hash — never 'unsafe-inline'.
//
// Rather than maintain those hashes by hand (the bootstrap's bytes change every build, since it names the
// content-hashed chunk files), the server computes them at startup from the exact index.html bytes it will serve. The
// policy is therefore always correct for whatever SPA is embedded, with no build step and no drift, and the E2E suite
// exercises the same policy production ships.

// spaCSP returns the Content-Security-Policy for the running binary, derived from the embedded SPA's index.html. It is
// computed once, when the security middleware is constructed.
func spaCSP() string {
	indexHTML, err := fs.ReadFile(spaFS, "index.html")
	if err != nil {
		// spaHandler() panics on this same missing-index.html condition when the server's routes are mounted, which is
		// the loud failure for a broken embed. Here we simply fall back to the script-less baseline so the header is
		// still well-formed.
		return cspForSPA(nil)
	}
	return cspForSPA(indexHTML)
}

// cspForSPA builds the policy string, allow-listing every inline script in indexHTML by hash. It is pure (no I/O) so
// the hashing logic is unit-testable.
func cspForSPA(indexHTML []byte) string {
	var b strings.Builder
	b.WriteString("default-src 'self'; script-src 'self'")
	for _, h := range inlineScriptHashes(indexHTML) {
		b.WriteByte(' ')
		b.WriteString(h)
	}
	// style-src must allow 'unsafe-inline': Svelte's transition/animation runtime injects <style> elements with
	// per-instance generated keyframes, whose content varies at runtime (so it cannot be hashed) and which
	// adapter-static cannot nonce. The script axis stays strict ('self' + explicit hashes, never 'unsafe-inline'); only
	// style is relaxed, a far lower-severity surface.
	b.WriteString("; style-src 'self' 'unsafe-inline'")
	b.WriteString("; frame-ancestors 'none'")
	return b.String()
}

// inlineScriptHashes returns a CSP source token ("'sha256-<base64>'") for each inline <script> element in html, in
// document order. A <script> carrying a src attribute is external (covered by 'self') and is skipped.
//
// The hash is taken over the element's child text verbatim — exactly the bytes between the opening tag's ">" and the
// matching "</script" — which is what a browser hashes when matching a script-src hash source. The same bytes are
// served to the browser (spaHandler writes index.html unmodified), so the hashes always match.
func inlineScriptHashes(html []byte) []string {
	var hashes []string
	rest := html
	for {
		open := indexFold(rest, "<script")
		if open < 0 {
			break
		}
		rest = rest[open:]

		// End of the opening tag.
		gt := bytes.IndexByte(rest, '>')
		if gt < 0 {
			break
		}
		openTag := rest[:gt+1]
		body := rest[gt+1:]

		// Child text runs up to the closing tag.
		end := indexFold(body, "</script")
		if end < 0 {
			break
		}
		inner := body[:end]

		if !hasSrcAttr(openTag) {
			sum := sha256.Sum256(inner)
			hashes = append(hashes, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
		}

		rest = body[end:]
	}
	return hashes
}

// hasSrcAttr reports whether the <script ...> opening tag carries a src attribute, marking the script as external. It
// matches a "src" token that is attribute-positioned (preceded by whitespace, followed by "=" or whitespace) so a
// substring like "datasrc" does not count.
func hasSrcAttr(openTag []byte) bool {
	lower := bytes.ToLower(openTag)
	for i := 0; i+3 <= len(lower); i++ {
		if lower[i] != 's' || lower[i+1] != 'r' || lower[i+2] != 'c' {
			continue
		}
		// Must be preceded by a space/tab/newline (attribute boundary).
		if i == 0 || !isASCIISpace(lower[i-1]) {
			continue
		}
		// Must be followed by "=" or whitespace (then "=").
		j := i + 3
		for j < len(lower) && isASCIISpace(lower[j]) {
			j++
		}
		if j < len(lower) && lower[j] == '=' {
			return true
		}
	}
	return false
}

func isASCIISpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

// indexFold is a case-insensitive bytes.Index for an ASCII-lowercase needle.
func indexFold(haystack []byte, lowerNeedle string) int {
	return bytes.Index(bytes.ToLower(haystack), []byte(lowerNeedle))
}
