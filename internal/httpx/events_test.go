package httpx

// White-box tests for eventsHandler. They live in package httpx (not httpx_test)
// so they can call the unexported eventsHandler constructor directly and reach
// the unexported writeSSEEvent / writeSSEPing helpers.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/events"
	"github.com/qovira/qovira/internal/store"
)

// ---- helpers ----------------------------------------------------------------

// principalCtx returns a context carrying the given Principal.
func principalCtx(ctx context.Context, p store.Principal) context.Context {
	return ContextWithPrincipal(ctx, p)
}

// newGETWithCtx returns a GET /events request with the given context.
func newGETWithCtx(ctx context.Context, t *testing.T) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, "/events", nil)
	if err != nil {
		t.Fatalf("http.NewRequestWithContext: %v", err)
	}
	return r
}

// ---- writeSSEEvent / writeSSEPing unit tests --------------------------------

// TestWriteSSEEvent_Format verifies that writeSSEEvent emits the three required
// SSE fields (event, id, data) with correct values and the blank-line terminator.
func TestWriteSSEEvent_Format(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	evt := events.Event{Type: "reminder.fired", Data: map[string]string{"msg": "hello"}}

	if err := writeSSEEvent(w, evt, 1); err != nil {
		t.Fatalf("writeSSEEvent: %v", err)
	}

	body := w.Body.String()

	if !strings.Contains(body, "event: reminder.fired\n") {
		t.Errorf("body missing event field; got: %q", body)
	}
	if !strings.Contains(body, "id: 1\n") {
		t.Errorf("body missing id field; got: %q", body)
	}
	if !strings.Contains(body, `"msg":"hello"`) {
		t.Errorf("body missing JSON data; got: %q", body)
	}
	if !strings.HasSuffix(body, "\n\n") {
		t.Errorf("body does not end with blank line; got: %q", body)
	}
}

// TestWriteSSEEvent_IDMonotonic verifies that successive calls increment the
// event id as a monotonic sequence number.
func TestWriteSSEEvent_IDMonotonic(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	evt := events.Event{Type: "assistant.token", Data: "tok"}

	for i := range uint64(3) {
		if err := writeSSEEvent(w, evt, i+1); err != nil {
			t.Fatalf("writeSSEEvent (id=%d): %v", i+1, err)
		}
	}

	body := w.Body.String()
	for _, id := range []string{"id: 1", "id: 2", "id: 3"} {
		if !strings.Contains(body, id) {
			t.Errorf("body missing %q; got: %q", id, body)
		}
	}
}

// TestWriteSSEPing_Format verifies the ping frame shape.
func TestWriteSSEPing_Format(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	if err := writeSSEPing(w); err != nil {
		t.Fatalf("writeSSEPing: %v", err)
	}

	body := w.Body.String()
	if !strings.Contains(body, "event: ping\n") {
		t.Errorf("ping missing event field; got: %q", body)
	}
	if !strings.HasSuffix(body, "\n\n") {
		t.Errorf("ping does not end with blank line; got: %q", body)
	}
}

// TestWriteSSEEvent_MarshalSkip verifies that when the event payload cannot be
// marshaled to JSON, writeSSEEvent skips the event (writes nothing to the
// ResponseWriter) and returns nil rather than an error, so the stream continues.
// A channel value is used as the payload because encoding/json cannot marshal
// channels and returns an error for them.
func TestWriteSSEEvent_MarshalSkip(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	// A channel is not JSON-marshalable; json.Marshal returns an error for it.
	unmarshalablePayload := make(chan struct{})
	evt := events.Event{Type: "bad.event", Data: unmarshalablePayload}

	err := writeSSEEvent(w, evt, 1)

	// Must return nil — the stream should not be killed by one bad event.
	if err != nil {
		t.Errorf("writeSSEEvent with unmarshalable data returned err = %v, want nil (skip, not kill)", err)
	}
	// Must write nothing — no partial SSE frame.
	if body := w.Body.String(); body != "" {
		t.Errorf("writeSSEEvent wrote %q to the wire for an unmarshalable event, want empty (skip)", body)
	}
}

// TestWriteSSEPing_HeartbeatFrame verifies that writeSSEPing emits the exact
// SSE ping frame documented in the function comment:
//
//	event: ping\ndata: \n\n
//
// The ping keeps proxies and load-balancers from closing idle connections and
// must not carry any event id.
func TestWriteSSEPing_HeartbeatFrame(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	if err := writeSSEPing(w); err != nil {
		t.Fatalf("writeSSEPing: %v", err)
	}

	body := w.Body.String()

	// Must begin with "event: ping\n".
	if !strings.HasPrefix(body, "event: ping\n") {
		t.Errorf("heartbeat frame does not start with 'event: ping\\n'; got: %q", body)
	}
	// Must carry a data field (even if empty) as documented.
	if !strings.Contains(body, "data: \n") {
		t.Errorf("heartbeat frame missing 'data: \\n'; got: %q", body)
	}
	// Must end with the SSE blank-line terminator.
	if !strings.HasSuffix(body, "\n\n") {
		t.Errorf("heartbeat frame does not end with blank line '\\n\\n'; got: %q", body)
	}
	// Must NOT carry an id field — heartbeat pings are not sequenced events.
	if strings.Contains(body, "id:") {
		t.Errorf("heartbeat frame must not carry an id field; got: %q", body)
	}
}

// ---- eventsHandler integration tests ----------------------------------------

// readyWriter wraps httptest.ResponseRecorder and sends a token on flushCh for
// every Flush call. eventsHandler flushes after the initial status line (flush 1 =
// readiness) and again after writing each event (flush 2 = event written). Tests
// can wait for specific flush counts to synchronise deterministically without
// sleeping.
//
// flushCh is buffered to 16 so that a Flush never blocks the handler goroutine
// even if the test goroutine is slow to drain it.
type readyWriter struct {
	*httptest.ResponseRecorder
	flushCh chan struct{} // one token per Flush call; buffered
}

func newReadyWriter() *readyWriter {
	return &readyWriter{
		ResponseRecorder: httptest.NewRecorder(),
		flushCh:          make(chan struct{}, 16),
	}
}

// Flush forwards to the embedded recorder and sends a token on flushCh so
// tests can count and sequence on flush events.
func (rw *readyWriter) Flush() {
	rw.ResponseRecorder.Flush()
	rw.flushCh <- struct{}{}
}

// waitFlush waits for the n-th Flush call (1-based) or until the timeout
// fires, failing the test if the deadline is exceeded.
func (rw *readyWriter) waitFlush(t *testing.T, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for i := range n {
		select {
		case <-rw.flushCh:
		case <-deadline:
			t.Fatalf("timed out waiting for flush #%d (received %d so far)", n, i)
		}
	}
}

// TestEventsHandler_StreamsEvent verifies that a published event reaches the
// response writer as a complete SSE frame with event:, id:, and data: fields.
//
// Synchronisation is flush-based:
//   - Flush 1: initial-flush guard — handler is subscribed and in the select loop.
//   - Flush 2: handler processed and wrote the published event.
//
// Cancelling only after flush 2 ensures the body contains the event.
func TestEventsHandler_StreamsEvent(t *testing.T) {
	t.Parallel()

	bus := events.NewBus()
	handler := eventsHandler(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ctx = principalCtx(ctx, store.Principal{UserID: "u1"})
	r := newGETWithCtx(ctx, t)
	w := newReadyWriter()

	// Run the handler in a goroutine — it blocks until the context is cancelled.
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(w, r)
	}()

	// Wait for flush 1 — handler is subscribed and in the select loop.
	w.waitFlush(t, 1, 2*time.Second)

	// Publish an event; the handler will write it and flush (flush 2).
	bus.Publish("u1", events.Event{Type: "reminder.fired", Data: map[string]string{"id": "r1"}})

	// Wait for flush 2 — event has been written to the response body.
	w.waitFlush(t, 1, 2*time.Second)

	// Safe to cancel now — the body already contains the event.
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after context cancel")
	}

	body := w.Body.String()

	if !strings.Contains(body, "event: reminder.fired") {
		t.Errorf("SSE body missing event field; got: %q", body)
	}
	if !strings.Contains(body, "id: 1") {
		t.Errorf("SSE body missing id field; got: %q", body)
	}
	if !strings.Contains(body, `"id":"r1"`) {
		t.Errorf("SSE body missing JSON data; got: %q", body)
	}
}

// TestEventsHandler_401WithoutPrincipal verifies that a request with no
// principal in context receives a 401 problem+json response and no stream.
func TestEventsHandler_401WithoutPrincipal(t *testing.T) {
	t.Parallel()

	bus := events.NewBus()
	handler := eventsHandler(bus)

	r, err := http.NewRequest(http.MethodGet, "/events", nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	if !strings.Contains(w.Body.String(), "unauthenticated") {
		t.Errorf("body missing 'unauthenticated' code; got: %q", w.Body.String())
	}
}

// TestEventsHandler_401EmptyUserID verifies that a principal with an empty
// UserID is treated as unauthenticated (401).
func TestEventsHandler_401EmptyUserID(t *testing.T) {
	t.Parallel()

	bus := events.NewBus()
	handler := eventsHandler(bus)

	ctx := principalCtx(context.Background(), store.Principal{UserID: ""})
	r := newGETWithCtx(ctx, t)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

// TestEventsHandler_405NonGET verifies that non-GET methods receive a 405
// problem+json response with an Allow: GET header.
func TestEventsHandler_405NonGET(t *testing.T) {
	t.Parallel()

	bus := events.NewBus()
	handler := eventsHandler(bus)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			r, err := http.NewRequest(method, "/events", nil)
			if err != nil {
				t.Fatalf("http.NewRequest: %v", err)
			}
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, r)

			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s: status = %d, want 405", method, w.Code)
			}
			if allow := w.Header().Get("Allow"); allow != "GET" {
				t.Errorf("%s: Allow = %q, want GET", method, allow)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("%s: Content-Type = %q, want application/problem+json", method, ct)
			}
		})
	}
}

// TestEventsHandler_ContextCancelUnsubscribes verifies that cancelling the
// request context causes the handler to exit and the bus subscription to be
// released. A subsequent Publish must not panic. The test uses -race to confirm
// there are no data races.
// Readiness is determined by the first Flush call, not by a fixed sleep.
func TestEventsHandler_ContextCancelUnsubscribes(t *testing.T) {
	t.Parallel()

	bus := events.NewBus()
	handler := eventsHandler(bus)

	ctx, cancel := context.WithCancel(context.Background())
	ctx = principalCtx(ctx, store.Principal{UserID: "u2"})
	r := newGETWithCtx(ctx, t)
	w := newReadyWriter()

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(w, r)
	}()

	// Wait for flush 1 — handler is subscribed and in the select loop.
	w.waitFlush(t, 1, 2*time.Second)

	// Handler is subscribed; cancel to trigger unsubscription.
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after context cancel")
	}

	// Publishing after the handler has returned must not panic and must not
	// hang — the cancel func unregistered the connection.
	bus.Publish("u2", events.Event{Type: "noop", Data: nil})
}

// TestEventsHandler_ClearsWriteDeadline verifies that the handler calls
// SetWriteDeadline with the zero time before entering the select loop. This is
// the regression guard for the bug where the server's global WriteTimeout (60 s)
// would force-close every SSE stream after ~60 s.
// Readiness is determined by the first Flush call, not by a fixed sleep.
func TestEventsHandler_ClearsWriteDeadline(t *testing.T) {
	t.Parallel()

	bus := events.NewBus()
	handler := eventsHandler(bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ctx = principalCtx(ctx, store.Principal{UserID: "u-deadline"})
	r := newGETWithCtx(ctx, t)
	w := newDeadlineRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(w, r)
	}()

	// Wait for the handler's initial flush — the deterministic readiness signal.
	// SetWriteDeadline is called immediately after the initial flush, before the
	// select loop, so at this point we know the deadline has already been cleared.
	select {
	case <-w.readyCh:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not emit initial flush within 2 s")
	}

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after context cancel")
	}

	// The handler must have called SetWriteDeadline exactly once, with the
	// zero time, to disable the server-level write deadline on this stream.
	if len(w.deadlines) != 1 {
		t.Fatalf("SetWriteDeadline called %d times, want exactly 1", len(w.deadlines))
	}
	if !w.deadlines[0].IsZero() {
		t.Errorf("SetWriteDeadline called with %v, want zero time.Time{}", w.deadlines[0])
	}
}

// TestEventsHandler_FlushUnsupportedWriter verifies that a ResponseWriter whose
// Flush returns errors.ErrUnsupported causes the handler to exit gracefully
// without panicking.
func TestEventsHandler_FlushUnsupportedWriter(t *testing.T) {
	t.Parallel()

	bus := events.NewBus()
	handler := eventsHandler(bus)

	ctx := principalCtx(context.Background(), store.Principal{UserID: "u3"})
	r := newGETWithCtx(ctx, t)

	// bareWriter only implements the http.ResponseWriter interface and nothing
	// else — no http.Flusher, no Unwrap. http.NewResponseController(w).Flush()
	// will therefore return errors.ErrUnsupported, exercising the graceful-exit
	// path in eventsHandler.
	w := &bareWriter{header: make(http.Header)}

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(w, r)
	}()

	select {
	case <-done:
		// Good — handler exited cleanly.
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not exit gracefully on unsupported flush")
	}
}

// bareWriter is a minimal http.ResponseWriter with no http.Flusher, no Unwrap,
// and no other extension interfaces. http.NewResponseController sees only the
// plain ResponseWriter, so Flush() returns errors.ErrUnsupported.
type bareWriter struct {
	header http.Header
	status int
	body   strings.Builder
}

func (b *bareWriter) Header() http.Header         { return b.header }
func (b *bareWriter) WriteHeader(code int)        { b.status = code }
func (b *bareWriter) Write(p []byte) (int, error) { return b.body.Write(p) }

// deadlineRecorder is a ResponseWriter that supports Flush and SetWriteDeadline.
// It records every SetWriteDeadline call so tests can assert the handler cleared
// the deadline before entering the select loop. readyCh is closed on the first
// Flush call so tests can wait for the handler's initial-flush guard without a
// fixed sleep.
type deadlineRecorder struct {
	header    http.Header
	status    int
	body      strings.Builder
	deadlines []time.Time // one entry per SetWriteDeadline call, in order
	readyCh   chan struct{}
	flushOnce sync.Once
}

func newDeadlineRecorder() *deadlineRecorder {
	return &deadlineRecorder{
		header:  make(http.Header),
		readyCh: make(chan struct{}),
	}
}

func (d *deadlineRecorder) Header() http.Header         { return d.header }
func (d *deadlineRecorder) WriteHeader(code int)        { d.status = code }
func (d *deadlineRecorder) Write(p []byte) (int, error) { return d.body.Write(p) }

// Flush satisfies http.Flusher so that http.NewResponseController.Flush()
// succeeds and the handler proceeds past the initial-flush guard. It signals
// readyCh on the first call.
func (d *deadlineRecorder) Flush() {
	d.flushOnce.Do(func() { close(d.readyCh) })
}

// SetWriteDeadline satisfies the interface that http.NewResponseController
// looks for. It records the supplied deadline and returns nil.
func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
	return nil
}
