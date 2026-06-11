package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/qovira/qovira/internal/events"
)

// heartbeatInterval is how often a ping frame is sent to keep proxies and load-balancers from closing idle SSE connections. It is a package-level var (not a const) so integration tests can shorten it without waiting on the real interval.
var heartbeatInterval = 10 * time.Second

// eventsHandler returns an http.HandlerFunc that bridges one HTTP request to a per-user subscription on the in-memory event bus. It is unexported, matching the package's unexported-handler style (cf. apiNotFoundHandler, healthzHandler).
//
// The handler:
//   - Rejects non-GET with 405 (the mux route is a bare all-methods pattern).
//   - Fails closed on auth: no principal in context, or empty UserID → 401.
//   - Subscribes to bus for the principal's UserID and streams events as SSE frames
//     until the client disconnects, the request context is cancelled, or the bus
//     closes/evicts the subscription (slow-consumer eviction).
//   - Emits a ping frame every heartbeatInterval to keep proxies open.
//   - Uses http.ResponseController.Flush (never the http.Flusher type assertion).
//   - Event id is a per-connection monotonic counter starting at 1 — sufficient
//     for client-side deduplication; there is no server replay buffer so the id
//     need not be globally unique.
func eventsHandler(bus events.Bus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Guard the method — the mux pattern is bare (all methods), so we enforce GET here. HEAD could be treated as GET-without-body, but a plain 405 is simpler and correct.
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			WriteProblem(w, r, Problem{
				Title:  "Method not allowed",
				Status: http.StatusMethodNotAllowed,
				Detail: fmt.Sprintf("The /events endpoint only accepts GET; got %s.", r.Method),
				Code:   "method_not_allowed",
			})
			return
		}

		// Fail closed on auth: the auth middleware must have run and placed a principal with a non-empty UserID in context. If not, return 401. This is what makes the "requires Authorization: Bearer" criterion hold — there is no URL-credential fallback.
		principal, ok := PrincipalFromContext(r.Context())
		if !ok || principal.UserID == "" {
			WriteProblem(w, r, Problem{
				Title:  "Unauthorized",
				Status: http.StatusUnauthorized,
				Detail: "A valid authenticated session is required to subscribe to the event stream.",
				Code:   "unauthorized",
			})
			return
		}

		// Subscribe before writing any response so that if the ResponseController cannot flush we can still clean up cleanly.
		stream, cancel := bus.Subscribe(principal.UserID)
		defer cancel()

		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()

		// Set SSE response headers and commit the 200 status line. Connection is a hop-by-hop header that HTTP/2 rejects; omit it.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		rc := http.NewResponseController(w)

		// Flush immediately after the status line so the client knows the stream is open. A ResponseWriter that can't flush cannot do SSE — end cleanly.
		if err := rc.Flush(); err != nil {
			if !errors.Is(err, errors.ErrUnsupported) {
				slog.Error("httpx: events: initial flush failed", "err", err, "userID", principal.UserID)
			}
			return
		}

		// Disable the per-response write deadline so the SSE stream can outlive the server's global WriteTimeout (60 s). The server timeout is correct for normal request/response handlers; this streaming route is the deliberate exception that opts out by clearing the deadline to zero.
		if err := rc.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, errors.ErrUnsupported) {
			// A writer that can't clear its deadline can't sustain a long-lived stream; log and end gracefully rather than getting force-closed at the server's WriteTimeout.
			slog.WarnContext(r.Context(), "httpx: cannot disable write deadline for SSE stream", "err", err)
			return
		}

		// eventID is the per-stream monotonic sequence number. It starts at 1 and increments with each event frame. The id is used for client-side deduplication only; no replay buffer exists server-side so it need not be globally unique.
		var eventID uint64

		for {
			select {
			case <-r.Context().Done():
				// Client disconnected or request context was cancelled.
				return

			case event, ok := <-stream:
				if !ok {
					// The bus closed this channel — slow-consumer eviction or explicit shutdown. Exit cleanly; the client should reconnect.
					return
				}
				eventID++
				if err := writeSSEEvent(w, event, eventID); err != nil {
					// Write error means the client is gone.
					return
				}
				if err := rc.Flush(); err != nil {
					return
				}

			case <-ticker.C:
				if err := writeSSEPing(w); err != nil {
					return
				}
				if err := rc.Flush(); err != nil {
					return
				}
			}
		}
	}
}

// writeSSEEvent marshals event.Data to JSON and writes a complete SSE frame:
//
//	event: <Type>
//	id: <eventID>
//	data: <json>
//	(blank line)
//
// If JSON marshalling fails the event is logged and skipped (returning nil so the stream continues) — a single bad payload should not kill the connection.
func writeSSEEvent(w http.ResponseWriter, event events.Event, eventID uint64) error {
	data, err := json.Marshal(event.Data)
	if err != nil {
		slog.Error("httpx: events: failed to marshal event data; skipping",
			"type", event.Type,
			"err", err,
		)
		return nil // skip, do not kill the stream
	}

	_, err = fmt.Fprintf(w, "event: %s\nid: %d\ndata: %s\n\n", event.Type, eventID, data)
	return err
}

// writeSSEPing writes an SSE ping frame — a named ping event with an empty data field — to keep proxies and load-balancers from closing idle connections:
//
//	event: ping
//	data:
//	(blank line)
func writeSSEPing(w http.ResponseWriter) error {
	_, err := fmt.Fprint(w, "event: ping\ndata: \n\n")
	return err
}
