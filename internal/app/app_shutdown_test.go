package app_test

// End-to-end test of the inverted shutdown order (hub before server): with a live SSE connection open,
// app.Run must still return promptly after ctx cancel.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/app"
)

// TestRun_ShutdownDeliversShutdownFrameAndReturnsPromptly verifies the full end-to-end shutdown path:
//
//  1. A live SSE connection is open when ctx is cancelled.
//  2. The client receives a system.shutdown frame followed by EOF.
//  3. app.Run returns well under shutdownGrace (15 s) — concretely within 5 s — because hub.Shutdown
//     drains the connection BEFORE srv.Shutdown is called, so srv.Shutdown finds no live connections.
//
// If the shutdown order were inverted (srv.Shutdown before hub.Shutdown), the SSE stream would never
// complete (it has no natural EOF), srv.Shutdown would block until the full 15 s shutdownGrace, and
// this test would time out.
func TestRun_ShutdownDeliversShutdownFrameAndReturnsPromptly(t *testing.T) {
	// Not parallel — this test is timing-sensitive and involves a real listening server + cancel.
	// Parallel execution on a loaded runner could make the elapsed-time assertions flaky.

	addr := freePort(t)
	cfg := app.Config{Addr: addr, LogLevel: "error", LogFormat: "json"}

	//nolint:testingcontext // need to cancel manually inside the test body
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)

	go func() {
		runErr <- app.Run(ctx, cfg)
	}()

	// Wait for the server to be ready using the existing waitReady helper.
	client := &http.Client{Timeout: time.Second}
	healthURL := fmt.Sprintf("http://%s/api/v1/health", addr)

	resp := waitReady(t, client, healthURL)
	if resp == nil {
		t.Fatalf("server did not become ready at %s", healthURL)
	}
	// Drain and close the poll response so the body is not leaked.
	if resp.Body != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	// Open a raw TCP SSE connection.
	conn, br := openRawSSEConn(t, addr)
	defer conn.Close() //nolint:errcheck // test cleanup

	// Wait for system.ready to confirm the handler loop is running.
	waitForSSEFrame(t, br, "system.ready", 3*time.Second)

	// Cancel the root context — this triggers Run's graceful shutdown.
	cancel()

	// The client MUST receive a system.shutdown frame before the connection closes.
	// Give a generous deadline: the drain path must be fast, but we need to account for scheduling.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck

	gotShutdown := scanForSSEShutdownFrame(t, br)

	if !gotShutdown {
		t.Error("client did not receive a system.shutdown frame before EOF")
	}

	// Run must return promptly — well under shutdownGrace (15 s). Use 5 s to give ample scheduling room
	// while discriminating against the blocking case (which would take ~15 s).
	start := time.Now()

	select {
	case err := <-runErr:
		elapsed := time.Since(start)

		if err != nil {
			t.Errorf("Run returned error: %v", err)
		}

		t.Logf("Run returned in %v (well under shutdownGrace=15s)", elapsed)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5 s after ctx cancel — likely blocked in srv.Shutdown on the open SSE stream (missing hub.Shutdown inversion)")
	}
}

// openRawSSEConn opens a raw TCP connection to addr and issues GET /events, returning the connection
// and a buffered reader positioned after the HTTP response headers. This mirrors openSSEStream from
// handler_test.go but works against a real bound address rather than an httptest.Server URL.
func openRawSSEConn(t *testing.T, addr string) (*net.TCPConn, *bufio.Reader) {
	t.Helper()

	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		t.Fatalf("resolve %s: %v", addr, err)
	}

	conn, err := net.DialTCP("tcp", nil, tcpAddr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}

	req := fmt.Sprintf("GET /events HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", addr)

	if _, err := fmt.Fprint(conn, req); err != nil {
		t.Fatalf("write request: %v", err)
	}

	br := bufio.NewReader(conn)

	// Discard HTTP response headers.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read response headers: %v", err)
		}

		if strings.TrimRight(line, "\r\n") == "" {
			break
		}
	}

	return conn, br
}

// waitForSSEFrame reads SSE lines from br until it finds a frame with the given event type, or the
// deadline fires. It consumes through the frame's blank-line terminator before returning.
func waitForSSEFrame(t *testing.T, br *bufio.Reader, eventType string, deadline time.Duration) {
	t.Helper()

	done := time.Now().Add(deadline)

	var inTarget bool

	for time.Now().Before(done) {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("waitForSSEFrame %q: read error: %v", eventType, err)
		}

		line = strings.TrimRight(line, "\r\n")

		switch {
		case strings.HasPrefix(line, "event:"):
			et := strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			inTarget = (et == eventType)

		case line == "" && inTarget:
			// Blank line terminates the target frame — done.
			return

		case line == "":
			inTarget = false
		}
	}

	t.Fatalf("waitForSSEFrame: did not receive %q frame within %v", eventType, deadline)
}

// scanForSSEShutdownFrame reads from br until it finds a system.shutdown frame or EOF. Returns true if
// the frame was found before EOF.
func scanForSSEShutdownFrame(t *testing.T, br *bufio.Reader) bool {
	t.Helper()

	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")

		if after, ok := strings.CutPrefix(line, "event:"); ok {
			if strings.TrimSpace(after) == "system.shutdown" {
				return true
			}
		}

		if err != nil {
			// EOF or deadline — frame not found.
			return false
		}
	}
}
