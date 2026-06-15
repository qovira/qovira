//go:build embed_spa

package httpx

// spa_embed.go — SPA filesystem for the full binary (//go:build embed_spa).
//
// This file is compiled ONLY when -tags embed_spa is passed (i.e. `make build`
// and the Docker image). It embeds the real SvelteKit adapter-static output from
// internal/httpx/webdist/ — which must be populated by `make sync-web` before
// `go build -tags embed_spa` is invoked. The directory is gitignored; it is never
// committed and must not exist on a fresh checkout (the noembed stub handles that
// case).
//
// The "all:" prefix on the embed directive is required: SvelteKit places its
// hashed JS/CSS chunks under _app/, and the embed package skips entries whose
// path components begin with "." or "_" by default.

import (
	"embed"
	"io/fs"
)

//go:embed all:webdist
var webdist embed.FS

func init() {
	var err error
	spaFS, err = fs.Sub(webdist, "webdist")
	if err != nil {
		panic("httpx: failed to sub webdist FS: " + err.Error())
	}
}
