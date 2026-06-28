// Package httpx provides HTTP serving primitives for the Qovira application server.
package httpx

import (
	"io/fs"
	"net/http"
	"strings"
)

// NewSPAHandler returns an [http.Handler] that serves static files from assets. For any request path that
// does not correspond to an existing file in assets, it falls back to serving assets/index.html so that
// client-side routing, deep links, and hard reloads work correctly.
func NewSPAHandler(assets fs.FS) http.Handler {
	// TODO(perf): http.FileServerFS over an embed.FS emits no Cache-Control/ETag/Last-Modified (embed
	// reports a zero modtime), so browsers re-fetch every asset on each load. Before the real SPA ships, wrap
	// this with middleware setting `Cache-Control: public, max-age=31536000, immutable` for /_app/immutable/
	// (content-hashed) paths and a revalidate/short cache for index.html.
	fileServer := http.FileServerFS(assets)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Resolve the fs path: strip the leading "/" (fs.FS uses clean paths).
		urlPath := r.URL.Path
		var fsPath string
		switch urlPath {
		case "", "/":
			fsPath = "index.html"
		default:
			fsPath = strings.TrimPrefix(urlPath, "/")
		}

		// Check whether the path exists as a regular file in the FS.
		info, err := fs.Stat(assets, fsPath)
		if err != nil || info.IsDir() {
			// Not found or is a directory — fall back to index.html by serving the file directly so we avoid
			// any redirect from the file server (which would redirect /some/deep → /some/deep/).
			http.ServeFileFS(w, r, assets, "index.html")
			return
		}

		fileServer.ServeHTTP(w, r)
	})
}
