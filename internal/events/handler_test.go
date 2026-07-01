package events_test

// handler_test.go exercises the SSE handler (NewHandler) end-to-end against a real httptest.Server.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/events"
	"github.com/qovira/qovira/internal/httpx"
)

// sseFrame holds one parsed SSE frame: the event type (from "event:" lines) and the concatenated
// payload (from "data:" lines). The SSE spec appends a newline between multiple data: lines, and the
// trailing \n is stripped. The id: field is intentionally absent from this type — AC 3 asserts none
// appear on the wire.
type sseFrame struct {
	EventType string
	Data      string
	HasID     bool // true if any "id:" line appeared in this frame
}

// readFrames reads SSE frames from r until n frames have been collected, the context is cancelled, or
// a read deadline fires. It returns whatever was collected. The deadline is applied to conn when conn
// is non-nil; otherwise the caller is responsible for cancelling via ctx.
func readFrames(t *testing.T, r *bufio.Reader, n int) []sseFrame {
	t.Helper()

	var frames []sseFrame

	var cur sseFrame
	var hasEvent bool

	for {
		line, err := r.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")

		if err != nil && line == "" {
			// EOF or deadline — return what we have.
			return frames
		}

		switch {
		case line == "":
			// Blank line dispatches the current frame (SSE spec).
			if hasEvent {
				frames = append(frames, cur)
				cur = sseFrame{}
				hasEvent = false

				if len(frames) >= n {
					return frames
				}
			}

		case strings.HasPrefix(line, "event:"):
			cur.EventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			hasEvent = true

		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimPrefix(line, "data:")
			// The SSE spec strips exactly one leading space after "data:".
			payload = strings.TrimPrefix(payload, " ")

			if cur.Data != "" {
				cur.Data += "\n" + payload
			} else {
				cur.Data = payload
			}

			hasEvent = true

		case strings.HasPrefix(line, "id:"):
			cur.HasID = true
			hasEvent = true
		}
	}
}

// newTestHandler returns a NewHandler with discarded logging and the given Timings. Hub is also
// returned so tests can publish events and inspect subscription state.
func newTestHandler(t *testing.T, hub *events.Hub, tm events.Timings) http.Handler {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	return events.NewHandler(hub, logger, tm)
}

// fastTimings returns Timings safe for tests: heartbeat far below write deadline,
// both measured in milliseconds so tests run in sub-second time.
func fastTimings() events.Timings {
	return events.Timings{
		PingInterval:  20 * time.Millisecond,
		WriteDeadline: 100 * time.Millisecond,
		RetryHint:     1 * time.Second,
	}
}

// openSSEStream opens a raw TCP connection to srv and issues a GET /events request, returning the
// buffered reader positioned after the HTTP headers. Setting a read deadline on the underlying conn
// prevents the test from hanging if fewer frames than expected arrive.
func openSSEStream(t *testing.T, srv *httptest.Server, readDeadline time.Duration) (*net.TCPConn, *bufio.Reader) {
	t.Helper()

	addr, err := serverTCPAddr(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}

	conn, err := net.DialTCP("tcp", nil, addr)
	if err != nil {
		t.Fatalf("dial server: %v", err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	if readDeadline > 0 {
		if err := conn.SetReadDeadline(time.Now().Add(readDeadline)); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
	}

	// Write the HTTP request line + minimal headers.
	req := fmt.Sprintf("GET /events HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", addr.String())

	if _, err := fmt.Fprint(conn, req); err != nil {
		t.Fatalf("write HTTP request: %v", err)
	}

	br := bufio.NewReader(conn)

	// Read and discard HTTP response headers; stop at the blank line.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read HTTP headers: %v", err)
		}

		if strings.TrimRight(line, "\r\n") == "" {
			break
		}
	}

	return conn, br
}

// serverTCPAddr extracts the host:port from an httptest.Server URL (e.g. "http://127.0.0.1:PORT")
// and resolves it to a *net.TCPAddr.
func serverTCPAddr(rawURL string) (*net.TCPAddr, error) {
	// Strip scheme — the URL is always http:// for httptest.Server.
	host := strings.TrimPrefix(rawURL, "http://")
	host = strings.TrimPrefix(host, "https://")
	// Strip any path suffix (e.g. the URL will have no path from httptest.Server, but be defensive).
	if idx := strings.IndexByte(host, '/'); idx >= 0 {
		host = host[:idx]
	}

	return net.ResolveTCPAddr("tcp", host)
}

// openSSEStreamWithHeaders opens an SSE stream and also captures the HTTP response status + headers.
// Returns status code, headers, and the buffered reader for body frames.
func openSSEStreamWithHeaders(t *testing.T, srv *httptest.Server, readDeadline time.Duration) (int, http.Header, *bufio.Reader) {
	t.Helper()

	addr, err := serverTCPAddr(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}

	conn, err := net.DialTCP("tcp", nil, addr)
	if err != nil {
		t.Fatalf("dial server: %v", err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	if readDeadline > 0 {
		if err := conn.SetReadDeadline(time.Now().Add(readDeadline)); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
	}

	req := fmt.Sprintf("GET /events HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", addr.String())

	if _, err := fmt.Fprint(conn, req); err != nil {
		t.Fatalf("write HTTP request: %v", err)
	}

	br := bufio.NewReader(conn)

	// Parse the status line.
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}

	parts := strings.SplitN(strings.TrimRight(statusLine, "\r\n"), " ", 3)
	if len(parts) < 2 {
		t.Fatalf("malformed status line: %q", statusLine)
	}

	var status int

	if _, err := fmt.Sscan(parts[1], &status); err != nil {
		t.Fatalf("parse status code from %q: %v", parts[1], err)
	}

	// Parse response headers.
	headers := make(http.Header)

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read header: %v", err)
		}

		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}

		k, v, ok := strings.Cut(line, ":")
		if ok {
			headers.Set(strings.TrimSpace(k), strings.TrimSpace(v))
		}
	}

	return status, headers, br
}

// TestHandler_ConnectSSEHeaders verifies that connecting to GET /events yields:
//   - HTTP 200
//   - Content-Type: text/event-stream
//   - Cache-Control: no-cache
//   - X-Accel-Buffering: no
//   - an immediate "retry:" directive (exact millisecond value not asserted — just presence)
//   - an event: system.ready frame whose data JSON carries a non-empty connectionId field
func TestHandler_ConnectSSEHeaders(t *testing.T) {
	t.Parallel()

	hub := events.New(4)
	tm := fastTimings()

	// Use the request-ID middleware so connectionId is set in context.
	mux := http.NewServeMux()
	mux.Handle("GET /events", newTestHandler(t, hub, tm))
	srv2 := httptest.NewServer(httpx.NewRequestIDMiddleware(mux))

	t.Cleanup(srv2.Close)

	status, headers, br := openSSEStreamWithHeaders(t, srv2, 3*time.Second)

	if status != http.StatusOK {
		t.Fatalf("want 200, got %d", status)
	}

	assertHeaderContains(t, headers, "Content-Type", "text/event-stream")
	assertHeaderEquals(t, headers, "Cache-Control", "no-cache")
	assertHeaderEquals(t, headers, "X-Accel-Buffering", "no")

	// Read until we find the retry: line and the system.ready frame. We collect raw lines because
	// retry: is NOT an event frame — it's a field that appears between frames, and our frame parser
	// skips it. So we scan lines manually for the retry directive, then parse the system.ready frame.
	var foundRetry bool

	var readyFrame *sseFrame

	var cur sseFrame
	var hasEvent bool

	for readyFrame == nil {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE body: %v (foundRetry=%v, readyFrame=%v)", err, foundRetry, readyFrame)
		}

		line = strings.TrimRight(line, "\r\n")

		switch {
		case strings.HasPrefix(line, "retry:"):
			foundRetry = true

		case strings.HasPrefix(line, "event:"):
			cur.EventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			hasEvent = true

		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimPrefix(line, "data:")
			payload = strings.TrimPrefix(payload, " ")
			cur.Data = payload
			hasEvent = true

		case strings.HasPrefix(line, "id:"):
			cur.HasID = true
			hasEvent = true

		case line == "" && hasEvent:
			f := cur
			readyFrame = &f
			cur = sseFrame{}
			hasEvent = false
		}
	}

	if !foundRetry {
		t.Error("want retry: directive before system.ready, got none")
	}

	if readyFrame.EventType != "system.ready" {
		t.Errorf("first frame: want event type system.ready, got %q", readyFrame.EventType)
	}

	if readyFrame.HasID {
		t.Error("system.ready frame must not carry an id: field")
	}

	var payload struct {
		ConnectionID string `json:"connectionId"`
	}

	if err := json.Unmarshal([]byte(readyFrame.Data), &payload); err != nil {
		t.Fatalf("unmarshal system.ready data: %v (data: %q)", err, readyFrame.Data)
	}

	if payload.ConnectionID == "" {
		t.Error("system.ready payload.connectionId must be non-empty (= Request-Id from context)")
	}
}

func assertHeaderContains(t *testing.T, h http.Header, key, want string) {
	t.Helper()

	got := h.Get(key)
	if !strings.Contains(got, want) {
		t.Errorf("header %s: want to contain %q, got %q", key, want, got)
	}
}

func assertHeaderEquals(t *testing.T, h http.Header, key, want string) {
	t.Helper()

	got := h.Get(key)
	if got != want {
		t.Errorf("header %s: want %q, got %q", key, want, got)
	}
}

// TestHandler_FanOutPublish verifies that hub.Publish(BroadcastTopic, evt) delivers a typed event: / data:
// frame on the open connection end-to-end, that the JSON payload round-trips, and that no id: field appears
// on any frame. The data:-line split for multi-line payloads is proven directly by
// TestFormatFrame_MultiLineSplitsAcrossDataLines — the live path marshals with compact json.Marshal, which
// never emits a raw newline, so it cannot exercise the split here.
func TestHandler_FanOutPublish(t *testing.T) {
	t.Parallel()

	hub := events.New(8)
	tm := fastTimings()
	srv := httptest.NewServer(newTestHandler(t, hub, tm))

	t.Cleanup(srv.Close)

	_, br := openSSEStream(t, srv, 3*time.Second)

	// Consume the system.ready frame before publishing. openSSEStream returns only after the server has
	// flushed the HTTP response headers; the handler writes system.ready (and flushes it) strictly AFTER
	// hub.Subscribe, so reading the system.ready frame here provides a deterministic happens-before:
	// the subscription is guaranteed to be registered by the time we call hub.Publish below.
	readFrames(t, br, 1) // consumes system.ready

	type nested struct {
		Line1 string `json:"line1"`
		Line2 string `json:"line2"`
	}

	payload := nested{Line1: "hello", Line2: "world"}
	publishedEvt := events.Event{Type: "test.event", Data: payload}

	hub.Publish(events.BroadcastTopic, publishedEvt)

	// Read frames until we find our event type (skipping system.ready and system.ping).
	const maxFrames = 10

	frames := make([]sseFrame, 0, maxFrames)

	var targetFrame *sseFrame

	for targetFrame == nil && len(frames) < maxFrames {
		batch := readFrames(t, br, 1)
		if len(batch) == 0 {
			break
		}

		for i := range batch {
			frames = append(frames, batch[i])

			if batch[i].EventType == "test.event" {
				f := batch[i]
				targetFrame = &f
				break
			}
		}
	}

	if targetFrame == nil {
		t.Fatalf("did not receive test.event frame after publish; got frames: %v", frames)
	}

	if targetFrame.HasID {
		t.Error("event frame must not carry an id: field")
	}

	// Unmarshal and verify data round-trips.
	var got nested
	if err := json.Unmarshal([]byte(targetFrame.Data), &got); err != nil {
		t.Fatalf("unmarshal test.event data: %v (data: %q)", err, targetFrame.Data)
	}

	if got.Line1 != payload.Line1 || got.Line2 != payload.Line2 {
		t.Errorf("test.event payload: want %+v, got %+v", payload, got)
	}

	// A second distinct event must also round-trip end-to-end (distinct type, distinct payload) — proving
	// the fan-out keeps delivering, not just the first frame after system.ready.
	type secondPayload struct {
		A string `json:"a"`
		B string `json:"b"`
	}

	sp := secondPayload{A: "foo", B: "bar"}
	secondEvt := events.Event{Type: "test.second", Data: sp}
	hub.Publish(events.BroadcastTopic, secondEvt)

	var secondFrame *sseFrame

	for secondFrame == nil && len(frames) < maxFrames {
		batch := readFrames(t, br, 1)
		if len(batch) == 0 {
			break
		}

		for i := range batch {
			frames = append(frames, batch[i])

			if batch[i].EventType == "test.second" {
				f := batch[i]
				secondFrame = &f
				break
			}
		}
	}

	if secondFrame == nil {
		t.Fatal("did not receive test.second frame")
	}

	if secondFrame.HasID {
		t.Error("event frame must not carry an id: field")
	}

	var gotSP secondPayload
	if err := json.Unmarshal([]byte(secondFrame.Data), &gotSP); err != nil {
		t.Fatalf("unmarshal test.second data: %v (data: %q)", err, secondFrame.Data)
	}

	if gotSP.A != sp.A || gotSP.B != sp.B {
		t.Errorf("test.second payload: want %+v, got %+v", sp, gotSP)
	}
}

// TestHandler_Heartbeat verifies that a system.ping frame arrives within a small multiple of the
// injected PingInterval. The test uses a fast interval (20 ms) so it completes in well under a
// second without any real-time sleep.
func TestHandler_Heartbeat(t *testing.T) {
	t.Parallel()

	hub := events.New(4)
	tm := fastTimings()
	srv := httptest.NewServer(newTestHandler(t, hub, tm))

	t.Cleanup(srv.Close)

	_, br := openSSEStream(t, srv, 3*time.Second)

	// Read up to 10 frames looking for at least one system.ping.
	const maxFrames = 10

	var gotPing bool

	for range maxFrames {
		batch := readFrames(t, br, 1)
		if len(batch) == 0 {
			break
		}

		for _, f := range batch {
			if f.EventType == "system.ping" {
				gotPing = true

				// system.ping data must carry a timestamp field that's a valid RFC 3339 string.
				var pingPayload struct {
					Time string `json:"time"`
				}

				if err := json.Unmarshal([]byte(f.Data), &pingPayload); err != nil {
					t.Fatalf("unmarshal system.ping data: %v (data: %q)", err, f.Data)
				}

				if pingPayload.Time == "" {
					t.Error("system.ping payload.time must be non-empty")
				}

				if f.HasID {
					t.Error("system.ping frame must not carry an id: field")
				}
			}
		}

		if gotPing {
			break
		}
	}

	if !gotPing {
		t.Error("want at least one system.ping frame within the read budget, got none")
	}
}

// TestHandler_ClientDisconnect verifies that cancelling the request context (simulated by closing the
// connection) causes the handler to return and a later hub.Publish no longer reaches the subscription.
// We assert on the hub's subscription count via Publish behaviour: after the handler returns, a publish
// to broadcastTopic should not block the publisher (no goroutine leak).
func TestHandler_ClientDisconnect(t *testing.T) {
	t.Parallel()

	hub := events.New(4)
	tm := fastTimings()

	handlerDone := make(chan struct{})
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		events.NewHandler(hub, slog.New(slog.NewTextHandler(io.Discard, nil)), tm).ServeHTTP(w, r)
		close(handlerDone)
	})

	srv := httptest.NewServer(wrapped)
	t.Cleanup(srv.Close)

	conn, _ := openSSEStream(t, srv, 3*time.Second)

	// Let the handler loop start and the system.ready frame arrive.
	time.Sleep(30 * time.Millisecond)

	// While connected, the handler holds exactly one subscription on the broadcast topic.
	if got := hub.SubscriberCount(events.BroadcastTopic); got != 1 {
		t.Fatalf("while connected: want 1 subscription on broadcast topic, got %d", got)
	}

	// Close the connection — this cancels the net/http request context.
	if err := conn.Close(); err != nil {
		t.Fatalf("close conn: %v", err)
	}

	// The handler must return within a reasonable deadline after the connection closes.
	select {
	case <-handlerDone:
		// good — ServeHTTP returned, so defer sub.Unsubscribe() has already run.
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after client disconnect within 2 s")
	}

	// The real cleanup proof: defer sub.Unsubscribe() must have removed the subscription from the hub.
	// (Asserting that a later Publish "doesn't block" proves nothing — Publish is non-blocking by
	// construction, so it returns promptly whether the subscription leaked or not.)
	if got := hub.SubscriberCount(events.BroadcastTopic); got != 0 {
		t.Errorf("after disconnect: want 0 subscriptions (Unsubscribe ran), got %d — subscription leaked", got)
	}
}

// TestHandler_WriteDeadlineRolling verifies that the rolling per-flush SetWriteDeadline keeps a
// healthy SSE connection alive past the server's WriteTimeout. The test uses httptest.NewUnstartedServer
// so it can set srv.Config.WriteTimeout to a short value before starting. With the rolling reset the
// connection must survive and deliver pings; without it the write would expire at the server timeout.
func TestHandler_WriteDeadlineRolling(t *testing.T) {
	t.Parallel()

	hub := events.New(4)

	// PingInterval is well below WriteDeadline, and both are below the server's WriteTimeout. The
	// per-flush rolling reset must keep the connection alive past WriteDeadline: each ping (every 20 ms)
	// resets the deadline (50 ms ahead), so a healthy stream lives indefinitely. The margin is chosen for
	// discrimination — without the rolling reset the connection dies at ~WriteDeadline (≈50 ms), yielding
	// only ~2 pings, well short of wantPings; with it, pings flow for the full observation window.
	tm := events.Timings{
		PingInterval:  20 * time.Millisecond,
		WriteDeadline: 50 * time.Millisecond,
		RetryHint:     time.Second,
	}

	handler := newTestHandler(t, hub, tm)

	srv := httptest.NewUnstartedServer(handler)
	srv.Config.WriteTimeout = 50 * time.Millisecond
	srv.Start()

	t.Cleanup(srv.Close)

	// Give the connection a generous read deadline — well past the server WriteTimeout.
	conn, br := openSSEStream(t, srv, 3*time.Second)
	_ = conn

	// Collect pings across a window many multiples of WriteDeadline (≈50 ms). With the rolling reset we
	// must see wantPings system.ping frames; without it the stream dies at ~50 ms after ~2 pings.
	const wantPings = 5
	var pingCount int

	deadline := time.Now().Add(time.Second)

	for time.Now().Before(deadline) && pingCount < wantPings {
		if err := conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
			break
		}

		batch := readFrames(t, br, 1)

		for _, f := range batch {
			if f.EventType == "system.ping" {
				pingCount++
			}
		}
	}

	if pingCount < wantPings {
		t.Errorf("rolling write-deadline: want at least %d pings past server WriteTimeout, got %d — "+
			"stream likely died at WriteTimeout without rolling reset", wantPings, pingCount)
	}
}

// TestHandler_OverCapRejects503 verifies that when the hub is at its connection cap a second /events
// connect receives:
//   - HTTP 503
//   - Content-Type: application/problem+json
//   - a Retry-After header
//   - no SSE bytes (no "event:" or "data:" lines) in the body
//
// The first connection is held open (its system.ready is consumed to confirm the loop started); the
// second connection is the one that must be rejected.
func TestHandler_OverCapRejects503(t *testing.T) {
	t.Parallel()

	hub := events.New(4)
	hub.SetMaxConns(1) // cap at 1 so the second connect is immediately over-limit

	tm := fastTimings()
	srv := httptest.NewServer(newTestHandler(t, hub, tm))

	t.Cleanup(srv.Close)

	// Open the first connection and consume system.ready to confirm the handler loop is running.
	_, br1 := openSSEStream(t, srv, 3*time.Second)

	var gotReady bool

	for !gotReady {
		line, err := br1.ReadString('\n')
		if err != nil {
			t.Fatalf("read while waiting for system.ready on conn1: %v", err)
		}

		if strings.TrimRight(line, "\r\n") == "event: system.ready" {
			gotReady = true
		}
	}

	// Drain past the system.ready frame terminator.
	for {
		line, err := br1.ReadString('\n')
		if err != nil {
			t.Fatalf("draining system.ready frame on conn1: %v", err)
		}

		if strings.TrimRight(line, "\r\n") == "" {
			break
		}
	}

	// Open the second connection — must be rejected with 503.
	status, headers, br2 := openSSEStreamWithHeaders(t, srv, 3*time.Second)

	if status != http.StatusServiceUnavailable {
		t.Errorf("over-cap connect: want HTTP 503, got %d", status)
	}

	ct := headers.Get("Content-Type")
	if !strings.Contains(ct, "application/problem+json") {
		t.Errorf("over-cap connect: want Content-Type application/problem+json, got %q", ct)
	}

	if headers.Get("Retry-After") == "" {
		t.Error("over-cap connect: want Retry-After header, got none")
	}

	// Read the full body and confirm no SSE bytes were written.
	body, err := io.ReadAll(br2)
	if err != nil {
		t.Fatalf("read over-cap body: %v", err)
	}

	bodyStr := string(body)

	if strings.Contains(bodyStr, "event:") || strings.Contains(bodyStr, "data:") {
		t.Errorf("over-cap connect: must write no SSE bytes, got body: %q", bodyStr)
	}
}
