//go:build !embed_spa

package httpx

// spa_noembed.go — SPA filesystem for the default (stub) build (//go:build !embed_spa).
//
// This file is compiled when -tags embed_spa is NOT passed: `go build ./...`, `go test ./...`, `make build-go`, and
// every CI step that does not need the real web UI. It supplies a minimal in-code fs.FS containing a single stub
// index.html, keeping the panic invariant in spaHandler() satisfied (index.html is always present) without any file on
// disk and without //go:embed, so a fresh checkout with no webdist/ directory compiles cleanly.
//
// testing/fstest.MapFS is used here. The Go documentation does not restrict MapFS to test code: it lives in the
// testing/fstest package rather than testing because it is a general-purpose in-memory FS. Using it in production
// non-test code is intentional and correct — its sole cost is a tiny const allocation at package init.

import "testing/fstest"

// stubIndexHTML is the in-code index.html served by the no-embed binary. It tells anyone who opens the server that the
// web UI was not embedded and directs them to `make build` for a binary with the real SPA.
const stubIndexHTML = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>Qovira</title>
    <style>
      body { font-family: system-ui, sans-serif; display: flex; align-items: center;
             justify-content: center; min-height: 100svh; margin: 0; background: #0f0f0f; color: #e5e5e5; }
      main { text-align: center; max-width: 480px; padding: 2rem; }
      code { background: #1f1f1f; padding: .2em .4em; border-radius: .25em; font-size: .9em; }
    </style>
  </head>
  <body>
    <main>
      <h1>Qovira</h1>
      <p>The web UI is <strong>not embedded</strong> in this build.</p>
      <p>Build the binary with <code>make build</code> to embed the real SvelteKit SPA.</p>
    </main>
  </body>
</html>
`

func init() {
	spaFS = fstest.MapFS{
		"index.html": &fstest.MapFile{
			Data: []byte(stubIndexHTML),
		},
		// A sentinel asset under the immutable prefix so that the handler's immutable-cache-header path is
		// exercisable in tests without a real SvelteKit build. The path follows the same _app/immutable/ convention
		// as real hashed SvelteKit chunks.
		"_app/immutable/stub.js": &fstest.MapFile{
			Data: []byte("// stub\n"),
		},
	}
}
