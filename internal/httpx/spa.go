package httpx

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// immutablePrefix is the URL path prefix under which SvelteKit emits content-hashed assets. Files served from here are
// immutable: their name changes whenever their content does, so clients never need to revalidate.
const immutablePrefix = "/_app/immutable/"

// spaFS is the sub-tree FS that spaHandler reads. It is initialised by exactly
// one of two build-tag-gated files:
//
//   - spa_embed.go   (//go:build embed_spa):   fs.Sub over the real //go:embed all:webdist FS.
//   - spa_noembed.go (//go:build !embed_spa):  a tiny in-code fs.FS with a single stub index.html.
//
// The handler reads spaFS only; it has no knowledge of which variant is active.
var spaFS fs.FS

// spaHandler returns an http.Handler that serves the embedded SPA with the
// required cache semantics:
//
//   - Files under /_app/immutable/ receive Cache-Control: public, max-age=31536000, immutable
//     (hashed filenames guarantee content-addressability; the client never needs to
//     revalidate).
//   - index.html and the SPA fallback receive Cache-Control: no-cache so browsers
//     recheck freshness on every navigation.
//
// Requests for a path that does not resolve to an embedded regular file fall back to index.html, enabling client-side
// routing in SvelteKit. Directories are never served as listings — a path that resolves to a directory falls back to
// index.html, so the asset tree is never enumerable over HTTP.
//
// Only GET and HEAD reach the file/fallback logic; any other method returns 405. This fallback path is consulted only
// for non-API requests: the mux routes /api/v1/{...} and /events ahead of it, so those are never served HTML.
func spaHandler() http.Handler {
	// Read index.html once at construction. A missing index.html means the embedded build is broken — fail loudly at
	// startup rather than serving empty fallbacks at runtime.
	indexHTML, err := fs.ReadFile(spaFS, "index.html")
	if err != nil {
		panic("httpx: embedded SPA is missing index.html: " + err.Error())
	}

	fileServer := http.FileServerFS(spaFS)

	serveIndex := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(indexHTML)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Serving static files and the SPA shell is read-only; reject anything that is not a safe, idempotent read so
		// non-GET requests to unknown paths never receive the SPA HTML.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Normalise to an fs path (no leading slash, cleaned). An empty path is the site root → index.html.
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			serveIndex(w)
			return
		}

		// Resolve the path against the embedded FS. Anything that is not an existing regular file (missing file,
		// directory, or invalid path) falls back to index.html — never a directory listing.
		info, err := fs.Stat(spaFS, name)
		if err != nil || info.IsDir() {
			serveIndex(w)
			return
		}

		// A real asset file. Hashed assets are immutable; everything else is revalidated on each request.
		if strings.HasPrefix(r.URL.Path, immutablePrefix) {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	})
}
