package events

import (
	"context"
	"sync"
)

// DefaultBufferSize is the per-subscription channel buffer used by production callers. A non-blocking send in
// Publish drops a subscription whose buffer is full, so this value balances SSE connection resilience
// (absorbing brief subscriber back-pressure) against memory per live connection.
//
// TODO(config): make this configurable via the instance config model (unit 9) so operators can tune the
// buffer for their deployment's expected connection count and event rate.
const DefaultBufferSize = 64

// Hub fans events out to all subscribers registered on a topic. Topics are opaque keys; they will later map
// to per-principal (per-user) channels once auth lands. Hub is safe for concurrent use.
//
// Design: the hub uses a mutex-guarded map with non-blocking sends rather than an owning goroutine. At
// single-node household scale the owning-goroutine pattern adds lifecycle complexity without a concurrency
// advantage.
//
// Lifecycle: Hub has a done channel and a WaitGroup that together coordinate graceful shutdown. The order in
// app.Run is intentionally inverted: hub.Shutdown (which drains all SSE connections) runs BEFORE
// srv.Shutdown. This means a new SSE connection can arrive between those two calls — while the listener is
// still accepting — even after done is closed. ConnStart guards against a wg.Add(1) racing a concurrent
// wg.Wait by serialising the done-check and the wg.Add under h.mu. See ConnStart for the full argument.
type Hub struct {
	mu         sync.RWMutex
	bufferSize int
	topics     map[string]map[*Subscription]struct{}

	// done is closed (exactly once, guarded by once) when Shutdown is called. Every connection's select
	// loop receives on h.Done() to detect shutdown and emit system.shutdown before returning.
	done chan struct{}
	once sync.Once // guards close(done)

	// wg tracks live connection goroutines. ConnStart calls wg.Add(1); ConnDone calls wg.Done().
	// Shutdown calls wg.Wait() (bounded by ctx) after closing done.
	wg sync.WaitGroup
}

// New constructs a Hub whose per-subscription channels are buffered to bufferSize. Production callers pass
// DefaultBufferSize; tests may inject a smaller value to fill the buffer deterministically. A non-positive
// bufferSize falls back to DefaultBufferSize: a zero would yield an unbuffered channel, making every Publish
// send fall through to the non-blocking default and drop the subscriber on its first event — a silent
// footgun if a misconfigured config value (unit 9) ever reaches here.
func New(bufferSize int) *Hub {
	if bufferSize < 1 {
		bufferSize = DefaultBufferSize
	}

	return &Hub{
		bufferSize: bufferSize,
		topics:     make(map[string]map[*Subscription]struct{}),
		done:       make(chan struct{}),
	}
}

// Done returns the hub's shutdown channel. It is closed when Shutdown is called. Connection goroutines
// select on this channel to detect shutdown and emit a system.shutdown frame before returning.
func (h *Hub) Done() <-chan struct{} {
	return h.done
}

// ConnStart registers a new live connection with the hub's WaitGroup. It returns true if the connection
// was successfully registered, false if the hub is already shutting down (done is closed). The caller
// MUST call ConnDone exactly once when the connection goroutine exits if and only if ConnStart returned true.
//
// Race-safety argument (the "sharp edge"):
//
// The order in app.Run is: hub.Shutdown → srv.Shutdown. Between those two calls the HTTP listener is still
// accepting connections, so a new /events request can arrive after done is closed. sync.WaitGroup forbids a
// wg.Add(1) that takes the counter from 0→1 from racing a concurrent wg.Wait — such a race is a data race
// and can panic.
//
// We serialize the done-channel check and the wg.Add under h.mu (the hub's existing write lock). Shutdown
// closes done under h.mu, then calls wg.Wait outside the lock. This gives a clean happens-before:
//
//   - If ConnStart takes h.mu BEFORE Shutdown closes done: it sees done open, calls wg.Add(1) (counter ≥ 1),
//     returns true. When it later observes Done() closed in its select loop it calls ConnDone → wg.Done.
//     Shutdown's wg.Wait sees the counter go to zero and returns normally.
//   - If Shutdown closes done and releases h.mu BEFORE ConnStart takes it: ConnStart sees done closed, skips
//     wg.Add, returns false. The caller must not register — no add after Wait.
//   - Shutdown cannot call wg.Wait before ConnStart's wg.Add because: if a goroutine is about to call
//     ConnStart and hasn't taken the lock yet when Shutdown closes done, that goroutine will see done closed
//     once it takes the lock, skip the Add, and return false — so wg.Wait will never have a missing Done.
func (h *Hub) ConnStart() bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	select {
	case <-h.done:
		// Hub is shutting down. Do not add to WaitGroup — done is closed and wg.Wait may already be running.
		return false
	default:
	}

	h.wg.Add(1)

	return true
}

// ConnDone signals to the hub that a connection goroutine has finished draining. The caller MUST call this
// exactly once, and only if ConnStart returned true. It wraps wg.Done.
func (h *Hub) ConnDone() {
	h.wg.Done()
}

// Shutdown signals all connection goroutines to drain by closing the done channel, then waits for them to
// finish, bounded by ctx. It returns nil when all connections drain within ctx's deadline, or ctx.Err() when
// the deadline fires first (the remaining connections will be force-closed by the subsequent srv.Shutdown).
//
// Shutdown is idempotent: calling it more than once is safe and the second call returns promptly (the once
// guard ensures done is closed at most once; wg.Wait returns immediately when the counter is already zero).
func (h *Hub) Shutdown(ctx context.Context) error {
	// Close done under the write-lock so ConnStart's lock-guarded check is fully serialized.
	// After this point, any new ConnStart call will see done closed and return false.
	h.once.Do(func() {
		h.mu.Lock()
		close(h.done)
		h.mu.Unlock()
	})

	// Wait for all live connections to drain, bounded by ctx.
	waitDone := make(chan struct{})

	go func() {
		h.wg.Wait()
		close(waitDone)
	}()

	select {
	case <-waitDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Subscribe registers a new Subscription on topic and returns it. The caller reads events from
// Subscription.C and calls Subscription.Unsubscribe when done.
func (h *Hub) Subscribe(topic string) *Subscription {
	ch := make(chan Event, h.bufferSize)
	sub := &Subscription{
		C:     ch,
		ch:    ch,
		hub:   h,
		topic: topic,
	}

	h.mu.Lock()
	if h.topics[topic] == nil {
		h.topics[topic] = make(map[*Subscription]struct{})
	}
	h.topics[topic][sub] = struct{}{}
	h.mu.Unlock()

	return sub
}

// Publish delivers e to every subscriber registered on topic. The send to each subscriber is non-blocking:
// if a subscriber's buffer is full its subscription is dropped — its channel is closed and it is removed from
// the topic map — so the publisher never blocks and OTHER subscribers on the topic still receive the event.
func (h *Hub) Publish(topic string, e Event) {
	h.mu.RLock()
	subs := h.topics[topic]
	if len(subs) == 0 {
		h.mu.RUnlock()
		return
	}

	// Collect victims (full-buffer subscribers) under the read-lock so we can release it quickly, then drop
	// them under the write-lock. This two-phase approach avoids holding the write-lock for the entire fan-out
	// and prevents any attempt to mutate the map while holding only the read-lock.
	var victims []*Subscription

	for sub := range subs {
		select {
		case sub.ch <- e:
		default:
			victims = append(victims, sub)
		}
	}

	h.mu.RUnlock()

	if len(victims) == 0 {
		return
	}

	for _, sub := range victims {
		sub.drop()
	}
}

// Subscription is a single subscriber's handle on a Hub topic. Read events from C; call Unsubscribe when
// done. A Subscription whose buffer was full at Publish time is dropped: its C channel is closed and it is
// removed from the hub, so the caller observes a closed channel.
type Subscription struct {
	// C is the receive-only event channel. It is closed when the subscription is dropped due to a full buffer
	// or when Unsubscribe is called.
	C <-chan Event

	// ch is the bidirectional alias of the same channel; used internally for sends and close.
	ch chan Event

	hub   *Hub
	topic string

	once sync.Once // guards the single close(ch) across concurrent drop and Unsubscribe calls
}

// Unsubscribe removes this subscription from the hub and closes its channel. Safe to call concurrently with
// Publish and idempotent (safe to call more than once; subsequent calls are no-ops).
func (s *Subscription) Unsubscribe() {
	s.drop()
}

// drop closes the subscription's channel (exactly once, guarded by sync.Once) and removes it from the hub.
// It is called from both Publish (on full buffer, under no lock) and Unsubscribe.
func (s *Subscription) drop() {
	s.once.Do(func() {
		// Remove from the hub map under the write-lock before closing the channel so that no new sends can
		// race against the close.
		s.hub.mu.Lock()
		if subs, ok := s.hub.topics[s.topic]; ok {
			delete(subs, s)
			if len(subs) == 0 {
				delete(s.hub.topics, s.topic)
			}
		}
		s.hub.mu.Unlock()

		close(s.ch)
	})
}
