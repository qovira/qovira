package events

// handler_capability_test.go tests two previously-uncovered branches of (*handler).ServeHTTP:
//
//  1. Capability-probe failures (Gap 1): the 500 problem+json paths triggered when the underlying
//     ResponseWriter does not support SetWriteDeadline or Flush. Because these tests need direct access to
//     supportsFlusher (unexported) and need to drive ServeHTTP with custom fakes, they live in the internal
//     package (package events, not package events_test).
//
//  2. Slow-consumer drop reaction (Gap 2): the !ok branch in the select loop, entered when the hub drops
//     a live subscription because its send buffer is full. The test uses a blocking ResponseWriter and
//     channel-based synchronization — no time.Sleep, no wall-clock races.

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── Gap 1 fakes ─────────────────────────────────────────────────────────────

// recordingWriter is a base http.ResponseWriter that captures status code and body for assertion. It
// deliberately does NOT implement http.Flusher, FlushError, or SetWriteDeadline, so any probe via
// http.ResponseController will fail unless the outer fake adds those methods.
type recordingWriter struct {
	mu      sync.Mutex
	code    int
	headers http.Header
	body    bytes.Buffer
}

func newRecordingWriter() *recordingWriter {
	return &recordingWriter{headers: make(http.Header)}
}

func (rw *recordingWriter) Header() http.Header { return rw.headers }

func (rw *recordingWriter) WriteHeader(code int) {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	if rw.code == 0 {
		rw.code = code
	}
}

func (rw *recordingWriter) Write(p []byte) (int, error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	return rw.body.Write(p)
}

func (rw *recordingWriter) statusCode() int {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	if rw.code == 0 {
		return http.StatusOK // net/http default when WriteHeader was never called
	}

	return rw.code
}

func (rw *recordingWriter) bodyString() string {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	return rw.body.String()
}

// fakeWithFlushNoDeadline wraps recordingWriter and adds http.Flusher (Flush is a no-op) but deliberately
// does NOT add SetWriteDeadline. This causes http.NewResponseController(w).SetWriteDeadline to return
// http.ErrNotSupported, driving the first 500 branch in ServeHTTP.
type fakeWithFlushNoDeadline struct {
	*recordingWriter
}

func (f *fakeWithFlushNoDeadline) Flush() {} // satisfies http.Flusher; no SetWriteDeadline present

// fakeWithDeadlineNoFlush wraps recordingWriter and adds SetWriteDeadline (returning nil, so the first
// probe passes) but deliberately does NOT add Flush or FlushError. This drives the second 500 branch
// (supportsFlusher returns false).
type fakeWithDeadlineNoFlush struct {
	*recordingWriter
}

func (f *fakeWithDeadlineNoFlush) SetWriteDeadline(_ time.Time) error { return nil }

// ── Gap 1: supportsFlusher unit tests ────────────────────────────────────────

// flusherW implements http.Flusher directly.
type flusherW struct{ recordingWriter }

func (f *flusherW) Flush() {}

// flushErrorW implements the FlushError() error interface (the newer variant that ResponseController
// checks first).
type flushErrorW struct{ recordingWriter }

func (f *flushErrorW) FlushError() error { return nil }

// plainW implements neither Flush nor FlushError.
type plainW struct{ recordingWriter }

// unwrapChainW wraps an inner ResponseWriter via Unwrap. supportsFlusher must walk the chain and find
// the inner Flusher. The outer type itself has no Flush method.
type unwrapChainW struct {
	recordingWriter
	inner http.ResponseWriter
}

func (u *unwrapChainW) Unwrap() http.ResponseWriter { return u.inner }

// TestSupportsFlusher_DirectFlusher verifies that a writer implementing http.Flusher returns true.
func TestSupportsFlusher_DirectFlusher(t *testing.T) {
	t.Parallel()

	if !supportsFlusher(&flusherW{}) {
		t.Error("supportsFlusher: writer implementing http.Flusher must return true")
	}
}

// TestSupportsFlusher_FlushErrorInterface verifies that a writer implementing FlushError() error
// (the interface checked before http.Flusher) returns true.
func TestSupportsFlusher_FlushErrorInterface(t *testing.T) {
	t.Parallel()

	if !supportsFlusher(&flushErrorW{}) {
		t.Error("supportsFlusher: writer implementing FlushError() error must return true")
	}
}

// TestSupportsFlusher_NeitherFlushNorFlushError verifies that a writer implementing neither interface
// returns false.
func TestSupportsFlusher_NeitherFlushNorFlushError(t *testing.T) {
	t.Parallel()

	if supportsFlusher(&plainW{}) {
		t.Error("supportsFlusher: writer implementing neither Flush nor FlushError must return false")
	}
}

// TestSupportsFlusher_UnwrapChainReachesInnerFlusher verifies that a writer whose inner (Unwrap'd)
// ResponseWriter implements http.Flusher is recognized via the Unwrap chain.
func TestSupportsFlusher_UnwrapChainReachesInnerFlusher(t *testing.T) {
	t.Parallel()

	inner := &flusherW{}
	outer := &unwrapChainW{inner: inner}

	if !supportsFlusher(outer) {
		t.Error("supportsFlusher: must walk Unwrap chain and find inner http.Flusher")
	}
}

// ── Gap 1: SetWriteDeadline unsupported → 500 problem+json ───────────────────

// TestHandler_CapabilityProbe_NoSetWriteDeadline verifies that ServeHTTP writes a 500 application/problem+json
// response and no SSE bytes when the underlying ResponseWriter does not support SetWriteDeadline (Fake A).
// Fake A implements http.Flusher but lacks SetWriteDeadline, so http.ResponseController.SetWriteDeadline
// returns http.ErrNotSupported, triggering the first error path in ServeHTTP.
func TestHandler_CapabilityProbe_NoSetWriteDeadline(t *testing.T) {
	t.Parallel()

	hub := New(DefaultBufferSize)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(hub, log, DefaultTimings)

	rec := newRecordingWriter()
	fake := &fakeWithFlushNoDeadline{recordingWriter: rec}

	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	h.ServeHTTP(fake, req)

	// Must respond with 500.
	if got := rec.statusCode(); got != http.StatusInternalServerError {
		t.Errorf("want HTTP 500, got %d", got)
	}

	// Content-Type must be application/problem+json.
	ct := rec.headers.Get("Content-Type")
	if !strings.Contains(ct, "application/problem+json") {
		t.Errorf("want Content-Type application/problem+json, got %q", ct)
	}

	// No SSE bytes must have been written.
	body := rec.bodyString()
	if strings.Contains(body, "event:") || strings.Contains(body, "data:") {
		t.Errorf("must write no SSE bytes before any streaming starts; body: %q", body)
	}
}

// TestHandler_CapabilityProbe_NoFlush verifies that ServeHTTP writes a 500 application/problem+json
// response and no SSE bytes when the underlying ResponseWriter supports SetWriteDeadline but does NOT
// support Flush or FlushError (Fake B). The SetWriteDeadline probe passes; supportsFlusher returns false,
// triggering the second error path in ServeHTTP.
func TestHandler_CapabilityProbe_NoFlush(t *testing.T) {
	t.Parallel()

	hub := New(DefaultBufferSize)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(hub, log, DefaultTimings)

	rec := newRecordingWriter()
	fake := &fakeWithDeadlineNoFlush{recordingWriter: rec}

	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	h.ServeHTTP(fake, req)

	// Must respond with 500.
	if got := rec.statusCode(); got != http.StatusInternalServerError {
		t.Errorf("want HTTP 500, got %d", got)
	}

	// Content-Type must be application/problem+json.
	ct := rec.headers.Get("Content-Type")
	if !strings.Contains(ct, "application/problem+json") {
		t.Errorf("want Content-Type application/problem+json, got %q", ct)
	}

	// No SSE bytes must have been written.
	body := rec.bodyString()
	if strings.Contains(body, "event:") || strings.Contains(body, "data:") {
		t.Errorf("must write no SSE bytes before any streaming starts; body: %q", body)
	}
}

// ── Gap 2: slow-consumer drop → handler !ok branch ───────────────────────────

// blockingWriter is a ResponseWriter whose Write method signals on the writeStarted channel each time it is
// entered, then blocks on the permit channel until the test sends a struct{}. It implements http.Flusher
// (Flush is a no-op) and SetWriteDeadline (no-op, returns nil) so both capability probes in ServeHTTP pass.
//
// Determinism guarantee: the test learns exactly when the handler is parked in Write (via writeStarted)
// and controls exactly when Write returns (via permit). No time.Sleep; no wall-clock races.
type blockingWriter struct {
	hdr          http.Header
	writeStarted chan struct{} // closed once per Write entry to notify the test
	permit       chan struct{} // Write returns after receiving one permit from the test
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{
		hdr:          make(http.Header),
		writeStarted: make(chan struct{}, 1), // buffered-1 so the writer never blocks on the notify
		permit:       make(chan struct{}),
	}
}

// waitForWrite waits until the handler goroutine has entered Write (i.e. is blocked on the permit channel).
// The test calls this before publishing events or releasing permits to guarantee ordering.
func (bw *blockingWriter) waitForWrite() {
	<-bw.writeStarted
}

// allowWrite releases the currently-blocked Write call. The test calls this after waitForWrite (or after
// the equivalent notification) to let the handler proceed.
func (bw *blockingWriter) allowWrite() { bw.permit <- struct{}{} }

func (bw *blockingWriter) Header() http.Header { return bw.hdr }

func (bw *blockingWriter) WriteHeader(_ int) {} // discard — we only care about the Write sequence

func (bw *blockingWriter) Write(p []byte) (int, error) {
	bw.writeStarted <- struct{}{} // notify test that we are about to block
	<-bw.permit                   // park until the test releases us

	return len(p), nil
}

// Flush satisfies http.Flusher so supportsFlusher returns true and rc.Flush() succeeds.
func (bw *blockingWriter) Flush() {}

// SetWriteDeadline satisfies http.ResponseController's deadline probe and always returns nil.
func (bw *blockingWriter) SetWriteDeadline(_ time.Time) error { return nil }

// TestHandler_SlowConsumerDrop_HandlerExitsOnClosedChannel verifies the handler's !ok branch: when the hub
// drops a live subscription (closes sub.C) because the send buffer is full, ServeHTTP observes the closed
// channel, logs the slow-consumer message, and returns. Determinism is guaranteed exclusively via channel
// operations — no time.Sleep.
//
// Sequence (each step is enforced by channel synchronization):
//
//  1. hub buffer=1. Build handler. Run ServeHTTP in a goroutine with a blockingWriter.
//  2. Wait for Write #1 (retry: directive). Release. Wait for Write #2 (system.ready frame). Release.
//     Assert SubscriberCount(BroadcastTopic)==1: subscription is live before any writes, so by the time
//     both writes complete and the handler reaches the select loop, SubscriberCount is guaranteed to be 1.
//  3. Publish event A. Wait for Write #3 (handler read A from sub.C, called Write; now blocked). At this
//     point sub.C is empty — the handler consumed A and is blocked writing it.
//  4. Publish event B → sub.C empty → B buffered (buffer now full, B in it).
//  5. Publish event C → sub.C full → hub drops subscription → close(sub.C) with [B] still in buffer.
//  6. Release Write #3. Handler calls Flush, loops to select. Reads B (ok=true) from closed-but-non-empty
//     sub.C. Calls Write #4 → blocked.
//  7. Wait for Write #4. Release. Handler calls Flush, loops to select. Reads from closed empty sub.C →
//     ok=false → returns (the !ok branch).
//  8. Assert: done channel fires. SubscriberCount(BroadcastTopic)==0.
func TestHandler_SlowConsumerDrop_HandlerExitsOnClosedChannel(t *testing.T) {
	t.Parallel()

	// hub buffer=1 so a single unconsumed event fills the per-subscription channel.
	hub := New(1)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// DefaultTimings: PingInterval=15s, WriteDeadline=30s. The heartbeat ticker fires every 15s, which is
	// far longer than this test runs (microseconds of channel round-trips), so no ping can interfere.
	h := NewHandler(hub, log, DefaultTimings)

	bw := newBlockingWriter()

	req := httptest.NewRequest(http.MethodGet, "/events", nil)

	// done is closed when ServeHTTP returns; used to assert handler exit without time.Sleep.
	done := make(chan struct{})

	go func() {
		defer close(done)
		h.ServeHTTP(bw, req)
	}()

	// ── Step 2: allow the two startup writes (retry: and system.ready) through.
	// Subscribe is called before any Write, so the subscription is live before Write #1.
	bw.waitForWrite() // Write #1: retry: directive (fmt.Fprintf)
	bw.allowWrite()   // release #1
	bw.waitForWrite() // Write #2: system.ready frame bytes (writeFrame → w.Write)
	bw.allowWrite()   // release #2

	// The subscription was registered before Write #1. After releasing Write #2, the handler completed
	// writeFrame (Flush is a no-op) and is now blocked in the for/select. SubscriberCount must be 1.
	if got := hub.SubscriberCount(BroadcastTopic); got != 1 {
		t.Fatalf("after startup writes: want 1 subscription, got %d", got)
	}

	// ── Step 3: Publish A. The handler is in select; A is immediately picked up from sub.C, and the
	// handler calls Write(A frame) → blocked. waitForWrite confirms the handler is parked.
	hub.Publish(BroadcastTopic, Event{Type: "test.A", Data: nil})
	bw.waitForWrite() // Write #3: event A frame — handler is now parked; sub.C is empty (A was consumed)

	// ── Step 4: Publish B → sub.C is empty (A was consumed above) → B is buffered (sub.C=[B], full).
	hub.Publish(BroadcastTopic, Event{Type: "test.B", Data: nil})

	// ── Step 5: Publish C → sub.C full → hub drops subscription → close(sub.C) with [B] in buffer.
	hub.Publish(BroadcastTopic, Event{Type: "test.C", Data: nil})

	// ── Step 6: release Write #3. Handler completes writeFrame(A), calls Flush (no-op), loops to select.
	// Select case: sub.C is closed but has [B] → reads B (ok=true) → calls Write(B frame) → blocked.
	bw.allowWrite() // release #3

	// ── Step 7: wait for Write #4 to confirm the handler is processing B, then release.
	// After Flush and the select read, the handler is now parked writing B.
	bw.waitForWrite() // Write #4: event B frame
	bw.allowWrite()   // release #4

	// ── Step 8: handler loops to select. sub.C is closed and empty → ok=false → handler returns.
	select {
	case <-done:
		// ServeHTTP returned via the !ok branch — correct.
	case <-time.After(5 * time.Second):
		t.Fatal("ServeHTTP did not return after sub.C was closed (slow-consumer drop); !ok branch may not have been reached")
	}

	// Defer sub.Unsubscribe() ran when ServeHTTP returned. The subscription must be gone from the hub.
	if got := hub.SubscriberCount(BroadcastTopic); got != 0 {
		t.Errorf("after slow-consumer drop: want 0 subscriptions, got %d — subscription leaked", got)
	}
}
