package events

import (
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
type Hub struct {
	mu         sync.RWMutex
	bufferSize int
	topics     map[string]map[*Subscription]struct{}
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
