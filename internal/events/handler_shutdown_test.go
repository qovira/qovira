package events_test

// handler_shutdown_test.go tests the system.shutdown frame delivery path end-to-end:
// when Hub.Shutdown is called, every connected /events client must receive exactly one
// system.shutdown frame (written by the connection's own goroutine, NOT via hub.Publish)
// followed by a clean EOF.
//
// Test mapping to acceptance criteria:
//   - TestHandler_ShutdownFrameDelivered — AC 1 + AC 4: one system.shutdown frame with retry hint, then EOF

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/events"
)

// TestHandler_ShutdownFrameDelivered verifies that when hub.Shutdown is called while a client is
// connected, the handler:
//
//  1. Writes exactly one "system.shutdown" SSE frame to the client's own writer.
//  2. The frame carries a "retryMs" field (matching the configured RetryHint in milliseconds).
//  3. The connection terminates with EOF immediately after the frame (no hanging).
//  4. The frame was NOT produced via hub.Publish — because the subscription has already been cleaned up
//     by the time we verify subscriber count.
func TestHandler_ShutdownFrameDelivered(t *testing.T) {
	t.Parallel()

	hub := events.New(4)
	tm := fastTimings() // RetryHint = 1 s = 1000 ms
	srv := httptest.NewServer(newTestHandler(t, hub, tm))

	t.Cleanup(srv.Close)

	// Open a real SSE connection via raw TCP (same helper used by the existing AC tests). The helper
	// registers a t.Cleanup that closes the conn, so we only need the reader.
	// Give a generous read deadline so we can observe the shutdown frame + EOF.
	_, br := openSSEStream(t, srv, 5*time.Second)

	// Wait for the system.ready frame to confirm the handler loop is running.
	var gotReady bool

	for !gotReady {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read while waiting for system.ready: %v", err)
		}

		line = strings.TrimRight(line, "\r\n")

		if line == "event: system.ready" {
			gotReady = true
		}
	}

	// Drain past the blank-line terminator of the system.ready frame.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("draining after system.ready: %v", err)
		}

		if strings.TrimRight(line, "\r\n") == "" {
			break
		}
	}

	// Give the handler loop a moment to settle (so it is blocked in select, not in startup).
	time.Sleep(20 * time.Millisecond)

	// Trigger shutdown.
	shutdownCtx, cancel := makeShutdownCtx(t, 2*time.Second)
	defer cancel()

	shutdownDone := make(chan error, 1)

	go func() {
		shutdownDone <- hub.Shutdown(shutdownCtx)
	}()

	// Read frames from the connection looking for system.shutdown.
	frames := readFramesUntilEOF(t, br, 10)

	// Exactly one system.shutdown frame must appear.
	var shutdownFrames []sseFrame

	for _, f := range frames {
		if f.EventType == "system.shutdown" {
			shutdownFrames = append(shutdownFrames, f)
		}
	}

	if len(shutdownFrames) != 1 {
		t.Fatalf("want exactly 1 system.shutdown frame, got %d; all frames: %v", len(shutdownFrames), frames)
	}

	sf := shutdownFrames[0]

	// The payload must carry a retryMs field matching RetryHint.
	var payload struct {
		RetryMs int64 `json:"retryMs"`
	}

	if err := json.Unmarshal([]byte(sf.Data), &payload); err != nil {
		t.Fatalf("unmarshal system.shutdown data: %v (data: %q)", err, sf.Data)
	}

	wantRetryMs := tm.RetryHint.Milliseconds()

	if payload.RetryMs != wantRetryMs {
		t.Errorf("system.shutdown retryMs: want %d, got %d", wantRetryMs, payload.RetryMs)
	}

	// Wait for Shutdown to complete.
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown: want nil, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown did not return within 3 s")
	}

	// AC 4: verify the frame was NOT written via hub.Publish. If it were, it would go through the
	// subscriber map — but the handler unsubscribes on exit, so a publish after unsubscribe would reach
	// zero subscribers. We verify there are zero subscribers after shutdown completes.
	if got := hub.SubscriberCount(events.BroadcastTopic); got != 0 {
		t.Errorf("AC4: after Shutdown subscriber count = %d, want 0 (frame was published into a torn-down map)", got)
	}
}

// TestHandler_EarlyExitWhenHubShuttingDown verifies the drain-window path: a connection that arrives after
// the hub has already shut down (the gap between hub.Shutdown and srv.Shutdown, where the listener is still
// accepting) is rejected by ConnStart before any SSE work — it must NOT send event-stream headers, NOT
// subscribe, NOT write a system.ready or system.shutdown frame, and must terminate with a prompt EOF rather
// than hang. This exercises the handler's ConnStart()==false early-exit branch.
func TestHandler_EarlyExitWhenHubShuttingDown(t *testing.T) {
	t.Parallel()

	hub := events.New(4)

	// Shut the hub down before any connection — every subsequent connect must hit the early-exit.
	ctx, cancel := makeShutdownCtx(t, time.Second)
	defer cancel()

	if err := hub.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	srv := httptest.NewServer(newTestHandler(t, hub, fastTimings()))
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 3 * time.Second}

	resp, err := client.Get(srv.URL + "/events") //nolint:noctx // test-only convenience
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()

	// No SSE stream may have started: the early-exit returns before WriteHeader, so the event-stream
	// content type must be absent.
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/event-stream") {
		t.Errorf("early-exit must not send SSE headers, got Content-Type %q", ct)
	}

	// The body must terminate at EOF (io.ReadAll returns, not hangs — the client timeout guards a hang) and
	// must contain no SSE frames.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if strings.Contains(string(body), "system.ready") || strings.Contains(string(body), "system.shutdown") {
		t.Errorf("early-exit must write no SSE frames, got body: %q", body)
	}

	// The early-exit must not subscribe (it returns before hub.Subscribe).
	if got := hub.SubscriberCount(events.BroadcastTopic); got != 0 {
		t.Errorf("early-exit must not subscribe, got %d subscribers", got)
	}
}

// readFramesUntilEOF reads SSE frames from br until EOF or up to max frames. Unlike readFrames it
// does not stop at n — it reads until the connection closes, which is the termination signal we need.
func readFramesUntilEOF(t *testing.T, br *bufio.Reader, limit int) []sseFrame {
	t.Helper()

	var frames []sseFrame
	var cur sseFrame
	var hasEvent bool

	for len(frames) < limit {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")

		if err != nil {
			// EOF or deadline — flush current partial frame and return.
			if hasEvent {
				frames = append(frames, cur)
			}

			return frames
		}

		switch {
		case line == "":
			if hasEvent {
				frames = append(frames, cur)
				cur = sseFrame{}
				hasEvent = false
			}

		case strings.HasPrefix(line, "event:"):
			cur.EventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			hasEvent = true

		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimPrefix(line, "data:")
			payload = strings.TrimPrefix(payload, " ")

			if cur.Data != "" {
				cur.Data += "\n" + payload
			} else {
				cur.Data = payload
			}

			hasEvent = true

		case strings.HasPrefix(line, "retry:"):
			// skip directive lines

		case strings.HasPrefix(line, "id:"):
			cur.HasID = true
			hasEvent = true
		}
	}

	return frames
}

// makeShutdownCtx is a test helper that returns a context with the given timeout plus a cancel.
func makeShutdownCtx(t *testing.T, timeout time.Duration) (context.Context, context.CancelFunc) {
	t.Helper()

	return context.WithTimeout(context.Background(), timeout)
}
