package httpx_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/qovira/qovira/internal/httpx"
)

// testFS returns a MapFS with an index.html and a static asset under _app/.
func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html": {
			Data: []byte(`<!doctype html><html><body>placeholder</body></html>`),
		},
		"_app/immutable/chunk.js": {
			Data: []byte(`console.log("chunk")`),
		},
	}
}

func TestSPAHandler_KnownStaticPath(t *testing.T) {
	t.Parallel()

	h := httpx.NewSPAHandler(testFS())

	req := httptest.NewRequest(http.MethodGet, "/_app/immutable/chunk.js", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	ct := rr.Header().Get("Content-Type")
	if ct != "text/javascript; charset=utf-8" && ct != "application/javascript" {
		t.Errorf("expected JS content type, got %q", ct)
	}

	body, _ := io.ReadAll(rr.Body)
	if string(body) != `console.log("chunk")` {
		t.Errorf("unexpected body: %q", string(body))
	}
}

func TestSPAHandler_UnknownPathFallsBackToIndex(t *testing.T) {
	t.Parallel()

	h := httpx.NewSPAHandler(testFS())

	for _, path := range []string{"/", "/some/deep/route", "/nonexistent"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, path, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("%s: expected 200, got %d", path, rr.Code)
			}

			ct := rr.Header().Get("Content-Type")
			// net/http file server sets text/html for .html files
			if ct != "text/html; charset=utf-8" {
				t.Errorf("%s: expected text/html; charset=utf-8 content type, got %q", path, ct)
			}

			body, _ := io.ReadAll(rr.Body)
			if string(body) != `<!doctype html><html><body>placeholder</body></html>` {
				t.Errorf("%s: unexpected body: %q", path, string(body))
			}
		})
	}
}

func TestSPAHandler_TraversalIsContained(t *testing.T) {
	t.Parallel()

	h := httpx.NewSPAHandler(testFS())

	const indexBody = `<!doctype html><html><body>placeholder</body></html>`

	// Traversal attempts must never escape the asset FS: fs.ValidPath rejects any ".." element and net/http
	// rejects dot-dot request paths, so each of these is either refused outright or falls back to index.html —
	// never a leaked sibling file. (Behind a ServeMux the path is also cleaned first; this pins the handler's
	// own contract directly.)
	for _, path := range []string{"/../spa.go", "/../../etc/passwd", "/_app/../../go.mod"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, path, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			body, _ := io.ReadAll(rr.Body)
			if rr.Code == http.StatusOK && string(body) != indexBody {
				t.Errorf("%s: traversal leaked non-index content (status %d): %q", path, rr.Code, string(body))
			}
		})
	}
}

func TestSPAHandler_IndexHTML_ContentType(t *testing.T) {
	t.Parallel()

	h := httpx.NewSPAHandler(testFS())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	ct := rr.Header().Get("Content-Type")
	if ct != "text/html; charset=utf-8" {
		t.Errorf("expected text/html; charset=utf-8, got %q", ct)
	}
}
