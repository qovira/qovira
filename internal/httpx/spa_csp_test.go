package httpx

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

// directive returns the source-list of the named CSP directive (e.g. the part
// after "script-src " up to the next ";"), or "" if absent.
func directive(csp, name string) string {
	for part := range strings.SplitSeq(csp, ";") {
		part = strings.TrimSpace(part)
		if rest, ok := strings.CutPrefix(part, name+" "); ok {
			return rest
		}
	}
	return ""
}

// token is the CSP source token a browser expects for the given inline script
// body — the independent oracle the extraction is checked against.
func token(inner string) string {
	sum := sha256.Sum256([]byte(inner))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

func TestInlineScriptHashes_VerbatimContent(t *testing.T) {
	t.Parallel()

	// Note the exact whitespace inside the element — it is part of the hashed
	// content, so the extractor must capture it byte-for-byte.
	inner := "\n      console.log('boot');\n    "
	html := "<html><head>\n    <script>" + inner + "</script>\n  </head></html>"

	got := inlineScriptHashes([]byte(html))
	if len(got) != 1 {
		t.Fatalf("got %d hashes, want 1: %v", len(got), got)
	}
	if want := token(inner); got[0] != want {
		t.Errorf("hash = %s, want %s (boundaries must capture the inner text verbatim)", got[0], want)
	}
}

func TestInlineScriptHashes_SkipsExternalAndOrdersInline(t *testing.T) {
	t.Parallel()

	a := "/* first */"
	b := "/* second */"
	html := `<head>` +
		`<script>` + a + `</script>` +
		`<script src="/_app/immutable/entry/start.js"></script>` +
		`<script type="module">` + b + `</script>` +
		`</head>`

	got := inlineScriptHashes([]byte(html))
	want := []string{token(a), token(b)}
	if len(got) != len(want) {
		t.Fatalf("got %d hashes, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("hash[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestInlineScriptHashes_None(t *testing.T) {
	t.Parallel()

	html := `<head><script src="/app.js"></script><link rel="stylesheet" href="/a.css"></head>`
	if got := inlineScriptHashes([]byte(html)); len(got) != 0 {
		t.Errorf("got %v, want no hashes (only an external script present)", got)
	}
}

func TestHasSrcAttr(t *testing.T) {
	t.Parallel()

	cases := []struct {
		tag  string
		want bool
	}{
		{`<script>`, false},
		{`<script type="module">`, false},
		{`<script src="/a.js">`, true},
		{`<script  src = "/a.js">`, true},
		{`<script nomodule data-foo="x">`, false},
		{`<script data-src="/a.js">`, false}, // a different attribute, not src
	}
	for _, c := range cases {
		if got := hasSrcAttr([]byte(c.tag)); got != c.want {
			t.Errorf("hasSrcAttr(%q) = %v, want %v", c.tag, got, c.want)
		}
	}
}

func TestCSPForSPA_Shape(t *testing.T) {
	t.Parallel()

	// No inline scripts → script-less baseline, still strict on script.
	if got := cspForSPA(nil); got != "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'" {
		t.Errorf("empty CSP = %q", got)
	}

	// With an inline script, its hash is admitted on script-src — and the script
	// axis must NEVER fall back to 'unsafe-inline' (only style may).
	html := []byte(`<head><script>x()</script></head>`)
	got := cspForSPA(html)
	if !strings.Contains(got, "script-src 'self' "+token("x()")) {
		t.Errorf("CSP %q must admit the inline script by hash", got)
	}
	if d := directive(got, "script-src"); strings.Contains(d, "unsafe-inline") {
		t.Errorf("script-src %q must never contain unsafe-inline", d)
	}
	if d := directive(got, "style-src"); !strings.Contains(d, "'unsafe-inline'") {
		t.Errorf("style-src %q must allow unsafe-inline (Svelte transition styles)", d)
	}
	if !strings.Contains(got, "frame-ancestors 'none'") {
		t.Errorf("CSP %q must keep frame-ancestors 'none'", got)
	}
}
