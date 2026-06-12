// Internal test package — tests live in package events so they can be
// compiled and run from the worktree directory without needing a resolvable
// external import. All behavior is exercised via the exported Bus interface.
package events

import (
	"sync"
	"testing"
	"time"
)

// newTestBus returns a fresh Bus for each test, typed as Bus so tests exercise
// the interface, not the concrete type.
func newTestBus(t *testing.T) Bus {
	t.Helper()
	return NewBus()
}

// recvEvent drains one event from ch or fails the test if nothing arrives
// within the deadline.
func recvEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
		panic("unreachable")
	}
}

// TestFanOut verifies criterion 1: Publish fans out to every connection
// subscribed for that user and does NOT deliver to another user's connection.
func TestFanOut(t *testing.T) {
	t.Parallel()

	b := newTestBus(t)

	const userA = "user-a"
	const userB = "user-b"

	ch1, cancel1 := b.Subscribe(userA)
	ch2, cancel2 := b.Subscribe(userA)
	chB, cancelB := b.Subscribe(userB)
	t.Cleanup(cancel1)
	t.Cleanup(cancel2)
	t.Cleanup(cancelB)

	want := Event{Type: "reminder.fired", Data: "payload"}
	b.Publish(userA, want)

	// Both userA connections must receive the event.
	got1 := recvEvent(t, ch1)
	got2 := recvEvent(t, ch2)

	if got1 != want {
		t.Errorf("ch1: got %+v, want %+v", got1, want)
	}
	if got2 != want {
		t.Errorf("ch2: got %+v, want %+v", got2, want)
	}

	// userB's channel must remain empty.
	select {
	case e := <-chB:
		t.Errorf("userB unexpectedly received event: %+v", e)
	default:
		// correct: nothing delivered cross-user
	}
}

// TestNonBlockingPublish verifies criterion 2: Publish returns quickly even
// when every subscribed consumer is slow (i.e. never reads).
func TestNonBlockingPublish(t *testing.T) {
	t.Parallel()

	b := newTestBus(t)

	const user = "slow-user"

	// Fill the buffer to capacity so the next send would block a naive
	// implementation. chanCap is the package constant (32).
	ch, cancel := b.Subscribe(user)
	t.Cleanup(cancel)

	fill := Event{Type: "fill", Data: nil}
	for range chanCap {
		b.Publish(user, fill)
	}

	// The buffer is now full. Publish must still return without blocking.
	done := make(chan struct{})
	go func() {
		b.Publish(user, Event{Type: "overflow", Data: nil})
		close(done)
	}()

	select {
	case <-done:
		// Publish returned promptly — correct.
	case <-time.After(time.Second):
		t.Fatal("Publish blocked with a full-buffer consumer")
	}

	// Drain whatever is buffered so cancel doesn't double-close.
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		default:
			return
		}
	}
}

// TestSlowConsumerEviction verifies criterion 3: when a consumer's buffer is
// full and Publish overflows it, the connection is evicted and the channel is
// closed so the consumer can detect disconnection via ok==false.
func TestSlowConsumerEviction(t *testing.T) {
	t.Parallel()

	b := newTestBus(t)

	const user = "evict-me"
	ch, _ := b.Subscribe(user) // intentionally don't cancel — eviction does it

	// Fill the channel to capacity (chanCap == 32).
	fill := Event{Type: "fill", Data: nil}
	for range chanCap {
		b.Publish(user, fill)
	}

	// One more publish overflows → eviction closes the channel.
	b.Publish(user, Event{Type: "overflow", Data: nil})

	// Drain the buffered events; eventually we must see the channel closed.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				// Channel closed — eviction confirmed.
				return
			}
		case <-deadline:
			t.Fatal("slow-consumer channel was not closed after eviction")
		}
	}
}

// TestCancelRemovesConnection verifies criterion 4 (part A): after cancel() is
// called a subsequent Publish does not deliver to the cancelled channel.
func TestCancelRemovesConnection(t *testing.T) {
	t.Parallel()

	b := newTestBus(t)

	const user = "cancel-user"
	ch, cancel := b.Subscribe(user)

	// Cancel before any publish.
	cancel()

	// Publish after cancel — should be a no-op for this connection.
	b.Publish(user, Event{Type: "should-not-arrive", Data: nil})

	select {
	case e, ok := <-ch:
		if ok {
			t.Errorf("received event on cancelled channel: %+v", e)
		}
		// ok==false means channel was closed by cancel — that's fine.
	default:
		// Nothing received — also correct.
	}
}

// TestCancelIdempotent verifies that calling cancel multiple times does not
// panic (no double-close).
func TestCancelIdempotent(t *testing.T) {
	t.Parallel()

	b := newTestBus(t)

	_, cancel := b.Subscribe("idem-user")

	// Must not panic on repeated calls.
	cancel()
	cancel()
	cancel()
}

// TestConcurrent verifies criterion 4 (part B): many goroutines
// subscribing, publishing, and cancelling concurrently are race-clean.
// Run with: go test -race ./internal/events/...
func TestConcurrent(t *testing.T) {
	t.Parallel()

	b := newTestBus(t)

	const (
		numWorkers = 50
		numEvents  = 20
	)

	var wg sync.WaitGroup
	wg.Add(numWorkers)

	for i := range numWorkers {
		go func(i int) {
			defer wg.Done()

			userID := "concurrent-user"

			ch, cancel := b.Subscribe(userID)
			defer cancel()

			// Publish a batch of events for this user.
			for j := range numEvents {
				b.Publish(userID, Event{
					Type: "tick",
					Data: i*numEvents + j,
				})
			}

			// Drain any events that arrived before cancel cleans up.
			for {
				select {
				case _, ok := <-ch:
					if !ok {
						return
					}
				default:
					return
				}
			}
		}(i)
	}

	wg.Wait()
}
