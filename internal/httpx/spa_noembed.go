//go:build !embed_spa

package httpx

import (
	"io/fs"
	"testing/fstest"
)

// Assets returns a placeholder in-memory filesystem used when the SPA has not
// been built and embedded (i.e. the embed_spa build tag is absent). This keeps
// `go build ./...` and `go test ./...` functional without a populated webdist/
// directory.
//
// The real embed is provided by spa_embed.go (//go:build embed_spa) in a later
// issue — do not add that file here.
func Assets() (fs.FS, error) {
	return fstest.MapFS{
		"index.html": {
			Data: []byte(`<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>Qovira</title></head>
<body>
<h1>Qovira</h1>
<p>Server is running. Build and embed the SPA for the full client.</p>
</body>
</html>
`),
		},
	}, nil
}
