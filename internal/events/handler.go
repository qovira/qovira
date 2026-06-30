package events

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/qovira/qovira/internal/api/problem"
	"github.com/qovira/qovira/internal/httpx"
)

// BroadcastTopic is the well-known topic that every anonymous SSE connection subscribes to. Once
// authentication lands (unit 5), the handler will switch to a per-principal key derived from the
// validated session token; this constant is the pre-auth stand-in.
const BroadcastTopic = "broadcast"

// Timings carries the tunable intervals for the SSE handler. Callers that need fast test cycles inject
// small values; production passes DefaultTimings.
//
// Invariant: PingInterval must be strictly less than WriteDeadline so that a healthy connection always
// resets its per-write deadline before it expires.
//
// TODO(config): promote these fields to the instance config model (unit 9) so operators can tune
// heartbeat and deadline values for their deployment without recompiling.
type Timings struct {
	// PingInterval is how often the handler emits a system.ping heartbeat frame. Must be strictly
	// less than WriteDeadline.
	PingInterval time.Duration

	// WriteDeadline is the per-flush write deadline applied via http.NewResponseController before
	// each frame write. Rolling this forward on every flush lets a healthy stream outlive the
	// server's global WriteTimeout while still bounding a wedged write.
	WriteDeadline time.Duration

	// RetryHint is the value emitted in the SSE "retry:" directive on connect. It tells the browser's
	// EventSource how long to wait before reconnecting after a dropped connection.
	RetryHint time.Duration
}

// DefaultTimings holds the production SSE handler timings. PingInterval is strictly below
// WriteDeadline so a healthy connection always resets its per-write deadline before it can expire.
//
// TODO(config): wire from the instance config model (unit 9) so operators can tune heartbeat
// and deadline values for their deployment without recompiling.
var DefaultTimings = Timings{
	PingInterval:  15 * time.Second,
	WriteDeadline: 30 * time.Second,
	RetryHint:     3 * time.Second,
}

// readyPayload is the data field of a system.ready SSE frame. It carries the connection id so clients
// can correlate log lines with their stream.
type readyPayload struct {
	ConnectionID string `json:"connectionId"`
}

// pingPayload is the data field of a system.ping SSE frame. The Time field is set at write to the
// current wall-clock time in RFC 3339 UTC format.
type pingPayload struct {
	Time string `json:"time"`
}

// shutdownPayload is the data field of a system.shutdown SSE frame. RetryMs carries the reconnect hint
// in milliseconds, mirroring the "retry:" directive sent on connect, so the client's EventSource can
// schedule its reconnection attempt correctly even if it has already consumed the initial retry: directive.
type shutdownPayload struct {
	RetryMs int64 `json:"retryMs"`
}

// handler is the http.Handler that drives one SSE connection.
type handler struct {
	hub    *Hub
	log    *slog.Logger
	timing Timings
}

// NewHandler returns an http.Handler that upgrades the connection to a server-sent events stream.
// On connect it:
//
//   - Subscribes to BroadcastTopic; defers Unsubscribe for cleanup.
//   - Obtains an http.ResponseController and verifies Flush + SetWriteDeadline are supported; if not,
//     it writes a 500 problem+json response before any streaming starts.
//   - Writes SSE headers (Content-Type, Cache-Control, X-Accel-Buffering) and the retry: directive.
//   - Emits a system.ready frame carrying the connection id (from httpx.RequestID).
//   - Runs a select loop with four cases: fan-out event, heartbeat ping, hub shutdown, client disconnect.
func NewHandler(hub *Hub, log *slog.Logger, t Timings) http.Handler {
	return &handler{hub: hub, log: log, timing: t}
}

// ServeHTTP implements http.Handler.
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)

	// Verify that the underlying ResponseWriter supports Flush and SetWriteDeadline BEFORE writing any
	// response bytes. This must happen before WriteHeader so we can still send a clean problem+json 500
	// if either capability is missing.
	//
	// SetWriteDeadline probe: set the first rolling write deadline (now + WriteDeadline). This both
	// verifies the capability (an unsupported writer returns an error) and establishes the initial
	// deadline, so there is no unbounded window between connect and the first flush. writeFrame rolls it
	// forward on every subsequent flush. We deliberately do NOT probe with a zero time: that would
	// DISABLE the deadline, leaving the connection unprotected until the first flush — and worse, it
	// would mask a regression in the rolling reset, since the connection would then outlive the server's
	// WriteTimeout even if writeFrame stopped resetting the deadline.
	//
	// Flush probe: we cannot call rc.Flush() here because on a real net/http connection Flush implicitly
	// commits headers (sending a 200 before our SSE headers are set). Instead we walk the Unwrap chain
	// manually to detect the http.Flusher interface — the same check ResponseController.Flush() itself
	// does, without the side effect.
	connID := httpx.RequestID(r.Context())

	if err := rc.SetWriteDeadline(time.Now().Add(h.timing.WriteDeadline)); err != nil {
		h.log.ErrorContext(r.Context(), "SSE: writer does not support SetWriteDeadline — rejecting connect",
			"requestId", connID,
			"err", err,
		)

		d := problem.Internal("SSE streaming is not supported by this connection.")
		d.RequestID = connID
		problem.WriteJSON(w, d)

		return
	}

	if !supportsFlusher(w) {
		h.log.ErrorContext(r.Context(), "SSE: writer does not support Flush — rejecting connect",
			"requestId", connID,
		)

		d := problem.Internal("SSE streaming is not supported by this connection.")
		d.RequestID = connID
		problem.WriteJSON(w, d)

		return
	}

	// Register this connection with the hub BEFORE subscribing and before entering the loop. If the hub is
	// already shutting down (connStart returns false), skip the loop entirely — the client will get an EOF
	// and its EventSource will reconnect using the retry: hint. We do NOT write a system.shutdown frame in
	// this early-exit path: we haven't yet sent SSE headers (so the response is not yet committed), and the
	// problem.json path above is the correct 5xx surface if we want to report something. A clean EOF is
	// sufficient — the browser will reconnect immediately.
	if !h.hub.connStart() {
		h.log.DebugContext(r.Context(), "SSE: hub is shutting down — rejecting late connect", "requestId", connID)
		return
	}

	defer h.hub.connDone()

	// Subscribe before writing the SSE headers so we cannot miss an event published between the header
	// write and the select loop start.
	sub := h.hub.Subscribe(BroadcastTopic)
	defer sub.Unsubscribe()

	// SSE response headers. Connection: keep-alive is omitted intentionally — net/http manages it, and
	// it is irrelevant under HTTP/2.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Emit the retry: directive then the system.ready canary frame. These share the same flush.
	// connID was resolved above during the capability checks; reuse it here.
	retryMs := h.timing.RetryHint.Milliseconds()

	if _, err := fmt.Fprintf(w, "retry:%d\n", retryMs); err != nil {
		h.log.DebugContext(r.Context(), "SSE: write retry directive failed", "requestId", connID, "err", err)
		return
	}

	if err := h.writeFrame(w, rc, Event{Type: "system.ready", Data: readyPayload{ConnectionID: connID}}); err != nil {
		h.log.DebugContext(r.Context(), "SSE: write system.ready failed", "requestId", connID, "err", err)
		return
	}

	ping := time.NewTicker(h.timing.PingInterval)
	defer ping.Stop()

	for {
		select {
		case e, ok := <-sub.C:
			if !ok {
				// The hub dropped this subscription (slow consumer). The client's EventSource will
				// reconnect using the retry: hint we sent on connect.
				h.log.InfoContext(r.Context(), "SSE: subscription dropped (slow consumer) — closing stream",
					"requestId", connID,
				)

				return
			}

			if err := h.writeFrame(w, rc, e); err != nil {
				h.log.DebugContext(r.Context(), "SSE: write event frame failed", "requestId", connID, "err", err)
				return
			}

		case <-ping.C:
			ts := time.Now().UTC().Format(time.RFC3339)
			if err := h.writeFrame(w, rc, Event{Type: "system.ping", Data: pingPayload{Time: ts}}); err != nil {
				h.log.DebugContext(r.Context(), "SSE: write ping frame failed", "requestId", connID, "err", err)
				return
			}

		case <-h.hub.Done():
			// Hub is shutting down. Write a system.shutdown frame directly to this connection's own
			// writer — do NOT use hub.Publish, which would attempt to send into the subscriber map
			// while the hub is tearing it down (a data race / use-after-teardown). Writing directly
			// here is safe: this goroutine owns w and rc for the lifetime of ServeHTTP.
			payload := shutdownPayload{RetryMs: h.timing.RetryHint.Milliseconds()}

			if err := h.writeFrame(w, rc, Event{Type: "system.shutdown", Data: payload}); err != nil {
				h.log.DebugContext(r.Context(), "SSE: write system.shutdown frame failed", "requestId", connID, "err", err)
			}
			// Return regardless of write error: the hub is shutting down and connDone (deferred above)
			// will decrement the WaitGroup so Shutdown can proceed.
			return

		case <-r.Context().Done():
			h.log.DebugContext(r.Context(), "SSE: client disconnected", "requestId", connID)
			return
		}
	}
}

// writeFrame sets a rolling write deadline, marshals e.Data as JSON, emits an SSE frame
// (event: / data: / blank-line) per the SSE specification, then flushes. Multi-line JSON payloads
// are split across multiple data: lines as required by the SSE spec.
//
// The rolling SetWriteDeadline call before each flush is the key mechanism that lets a healthy stream
// outlive the server's global WriteTimeout: each successful flush resets the deadline so the window
// slides forward indefinitely. A wedged write still trips the bound (WriteDeadline).
func (h *handler) writeFrame(w http.ResponseWriter, rc *http.ResponseController, e Event) error {
	if err := rc.SetWriteDeadline(time.Now().Add(h.timing.WriteDeadline)); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}

	data, err := json.Marshal(e.Data)
	if err != nil {
		return fmt.Errorf("marshal event data: %w", err)
	}

	// Build the whole frame, then write it in one call so the frame reaches the client atomically.
	if _, err := w.Write(formatFrame(e.Type, data)); err != nil {
		return fmt.Errorf("write frame: %w", err)
	}

	// Flush pushes the frame to the client immediately rather than waiting for the response buffer to fill.
	if err := rc.Flush(); err != nil {
		return fmt.Errorf("flush: %w", err)
	}

	return nil
}

// formatFrame serializes an event type and its already-marshaled JSON data into a single SSE frame:
//
//	event: <type>\n
//	data: <segment>\n   (one data: line per newline-separated segment of the payload)
//	\n
//
// The newline split is required by the SSE spec: a raw newline inside a data: value would otherwise be read
// as a record/field boundary, corrupting the stream. The client's parser rejoins the data: lines with a
// single newline, reconstructing the payload exactly. json.Marshal emits compact (single-line) output
// today, so for the system.* events the split yields one line — it is defensive framing that keeps the wire
// correct if a payload ever carries embedded newlines (e.g. pre-formatted JSON from a future producer). No
// id: field is emitted: the hub holds no history, so a Last-Event-ID would only be ignored.
func formatFrame(eventType string, data []byte) []byte {
	var b strings.Builder

	b.WriteString("event: ")
	b.WriteString(eventType)
	b.WriteByte('\n')

	for segment := range strings.SplitSeq(string(data), "\n") {
		b.WriteString("data: ")
		b.WriteString(segment)
		b.WriteByte('\n')
	}

	b.WriteByte('\n') // blank line terminates the frame

	return []byte(b.String())
}

// supportsFlusher reports whether rw (or an http.ResponseWriter reached via Unwrap) implements
// http.Flusher. This replicates the walk that http.ResponseController.Flush() performs internally but
// without calling Flush(), which would implicitly commit the response headers before we have set the
// SSE-specific ones.
func supportsFlusher(rw http.ResponseWriter) bool {
	type unwrapper interface {
		Unwrap() http.ResponseWriter
	}

	for {
		switch v := rw.(type) {
		case interface{ FlushError() error }:
			return true
		case http.Flusher:
			return true
		case unwrapper:
			rw = v.Unwrap()
		default:
			return false
		}
	}
}
