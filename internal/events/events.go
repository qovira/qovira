// Package events implements the in-memory, per-user pub/sub bus for qovira's realtime subsystem. It is
// transport-agnostic: the HTTP/SSE layer is a separate concern that consumes this package's [Bus] (or the narrower
// [Publisher] interface).
//
// # Lock-during-send invariant
//
// Every channel send and every channel close happens while the registry mutex is held: [bus.Publish] holds the lock for
// its whole fan-out loop, and the two places that close a channel — [bus.Subscribe]'s cancel func and Publish's own
// slow-consumer eviction — take the same lock. Because a send and a close can therefore never overlap, Publish can
// never send on a channel another goroutine is closing. Sending on a closed channel panics, so this invariant is what
// keeps the bus free of panics and data races.
package events

import (
	"sync"
)

// Event is the unit of communication on the bus. Type follows the "<resource>.<action>" convention (e.g.
// "reminder.fired", "assistant.token"). Data must be JSON-marshalable; consumers own the value after receiving it.
type Event struct {
	Type string // "<resource>.<action>", e.g. reminder.fired, assistant.token
	Data any    // JSON-marshalable fat payload
}

// Publisher is the narrow write-side interface. Subsystems that only need to emit events should depend on Publisher,
// not on the full Bus.
type Publisher interface {
	// Publish fans e out to all of userID's active connections. It is non-blocking: a slow consumer is evicted rather
	// than stalling the caller.
	Publish(userID string, e Event)
}

// Bus is the full pub/sub interface: it extends Publisher with Subscribe so that transport adapters (SSE, WebSocket, …)
// can attach consumers.
type Bus interface {
	Publisher
	// Subscribe registers a new connection for userID. It returns a receive-only channel that will deliver events and a
	// cancel func that unregisters the connection and closes the channel. cancel is idempotent.
	Subscribe(userID string) (stream <-chan Event, cancel func())
}

// chanCap is the buffer size for each subscriber's channel. A full buffer triggers slow-consumer eviction (see
// Publish).
const chanCap = 32

// connID is an opaque connection identifier. An atomic counter under the lock is sufficient — no external dependency
// needed.
type connID = uint64

// bus is the concrete implementation of Bus. The zero value is not valid; use NewBus.
type bus struct {
	// mu guards all access to conns. The lock-during-send invariant (see package doc) requires that both channel sends
	// (in Publish) and channel closes (in cancel / eviction) happen while mu is held.
	mu sync.Mutex

	// conns maps userID → connID → channel. The outer map is allocated in NewBus. Inner maps are created lazily on
	// first Subscribe for a user and deleted when empty.
	conns map[string]map[connID]chan Event

	// nextID generates monotonically increasing connection IDs under mu.
	nextID uint64
}

// NewBus constructs and returns a ready-to-use *bus. The concrete type is returned (accept interfaces, return concrete
// types) so callers can depend on the Bus interface while retaining access to the full type if needed.
func NewBus() *bus { //nolint:revive // intentional unexported return per issue API spec
	return &bus{
		conns: make(map[string]map[connID]chan Event),
	}
}

// Subscribe registers a new buffered channel for userID and returns it as a receive-only channel. The returned cancel
// func removes the connection from the registry and closes the channel; it is safe to call more than once (idempotent).
func (b *bus) Subscribe(userID string) (<-chan Event, func()) {
	ch := make(chan Event, chanCap)

	b.mu.Lock()
	b.nextID++
	id := b.nextID
	if b.conns[userID] == nil {
		b.conns[userID] = make(map[connID]chan Event)
	}
	b.conns[userID][id] = ch
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			b.removeConn(userID, id)
			b.mu.Unlock()
		})
	}
	return ch, cancel
}

// Publish fans e out to all active connections for userID. It is non-blocking: if a connection's buffer is full (the
// select default fires) the connection is evicted — closed and removed from the registry — so a slow client self-heals
// by reconnecting. Users with no connections are a no-op.
//
// The registry mutex is held for the entire loop (lock-during-send invariant).
func (b *bus) Publish(userID string, e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()

	conns := b.conns[userID]
	if len(conns) == 0 {
		return
	}

	for id, ch := range conns {
		select {
		case ch <- e:
		default:
			// Slow consumer: evict it so the caller is never blocked and so other consumers for this user are not
			// affected.
			b.removeConn(userID, id)
		}
	}
}

// removeConn removes the connection identified by id from the registry for userID, closes its channel, and cleans up
// the per-user map when empty. It must be called with b.mu held.
func (b *bus) removeConn(userID string, id connID) {
	conns := b.conns[userID]
	if conns == nil {
		return
	}
	ch, ok := conns[id]
	if !ok {
		return
	}
	close(ch)
	delete(conns, id)
	if len(conns) == 0 {
		delete(b.conns, userID)
	}
}
