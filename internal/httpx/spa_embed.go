//go:build embed_spa

package httpx

import (
	"embed"
	"io/fs"
)

// All files under webdist/ are embedded. The `all:` prefix is mandatory:
// without it //go:embed silently skips entries whose names begin with `_` or `.`
// (including the entire `_app/` directory that holds the SvelteKit bundle).
//
//go:embed all:webdist
var embedded embed.FS

// Assets returns the embedded SvelteKit build rooted at webdist/, stripping
// that prefix so the handler sees paths like "index.html" and "_app/…".
func Assets() (fs.FS, error) {
	return fs.Sub(embedded, "webdist")
}
