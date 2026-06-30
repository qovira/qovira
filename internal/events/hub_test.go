package events_test

import (
	"sync"
	"testing"

	"github.com/qovira/qovira/internal/events"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// collect drains up to n events from sub's channel without blocking; returns whatever arrived.
func collect(sub *events.Subscription, n int) []events.Event {
	out := make([]events.Event, 0, n)

	for range n {
		select {
		case e, ok := <-sub.C:
			if !ok {
				return out // channel closed — stop early
			}

			out = append(out, e)
		default:
			return out // nothing buffered — done
		}
	}

	return out
}

// drain reads all remaining events until the channel closes, returning the collected slice. It is used to
// observe a closed channel after a slow-consumer drop.
func drainUntilClosed(sub *events.Subscription) []events.Event {
	var out []events.Event

	for e := range sub.C {
		out = append(out, e)
	}

	return out
}

// ── AC 1: fan-out and topic isolation ────────────────────────────────────────

// TestHub_PublishReachesAllSubscribersOnTopic verifies that every active subscriber on the published topic
// receives the event (fan-out), and that a subscriber on a different topic never receives it (isolation).
func TestHub_PublishReachesAllSubscribersOnTopic(t *testing.T) {
	t.Parallel()

	h := events.New(4)
	evt := events.Event{Type: "system.ready", Data: "payload"}

	subA := h.Subscribe("topicA")
	subB := h.Subscribe("topicA")
	subOther := h.Subscribe("topicB")

	h.Publish("topicA", evt)

	// Both topicA subscribers must receive the event.
	gotA := collect(subA, 1)
	if len(gotA) != 1 {
		t.Errorf("subA: want 1 event, got %d", len(gotA))
	} else if gotA[0].Type != evt.Type {
		t.Errorf("subA: want Type %q, got %q", evt.Type, gotA[0].Type)
	}

	gotB := collect(subB, 1)
	if len(gotB) != 1 {
		t.Errorf("subB: want 1 event, got %d", len(gotB))
	}

	// The topicB subscriber must NOT receive any event.
	gotOther := collect(subOther, 1)
	if len(gotOther) != 0 {
		t.Errorf("subOther (topicB): want 0 events, got %d — topic isolation violated", len(gotOther))
	}
}

// ── AC 2: slow-consumer drop ─────────────────────────────────────────────────

// TestHub_SlowConsumerIsDroppedWithoutBlockingPublisher verifies that when a subscription's buffer is full
// and a further Publish arrives, the hub:
//   - drops (closes the channel of) the slow subscriber,
//   - returns from Publish without blocking (the test itself would hang if it did),
//   - continues to deliver to other subscribers on the same topic.
func TestHub_SlowConsumerIsDroppedWithoutBlockingPublisher(t *testing.T) {
	t.Parallel()

	const bufSize = 2
	h := events.New(bufSize)

	slow := h.Subscribe("topic")
	fast := h.Subscribe("topic")

	evt := events.Event{Type: "reminder.fired", Data: nil}

	// Fill slow's buffer to capacity. fast drains each time, so it is never slow.
	for range bufSize {
		h.Publish("topic", evt)
		collect(fast, 1) // drain fast immediately so it stays non-full
	}

	// One more publish: slow's buffer is now full, so its send must be non-blocking — the hub drops it.
	// fast is empty, so it receives normally.
	h.Publish("topic", evt)

	// fast must have received the final event.
	gotFast := collect(fast, 1)
	if len(gotFast) != 1 {
		t.Errorf("fast subscriber: want 1 event after drop, got %d", len(gotFast))
	}

	// slow's channel must be closed after the drop. drainUntilClosed uses range — a range loop over a closed,
	// buffered channel drains remaining buffered items and then exits. If the channel were not closed this call
	// would block and the test would time out, which is a reliable failure signal.
	drainUntilClosed(slow)

	// A subsequent publish must not reach the dropped slow subscription.
	h.Publish("topic", evt)
	collect(fast, 1) // consume fast's copy so the hub stays healthy
}

// TestHub_PublisherDoesNotBlockOnFullBuffer verifies that Publish returns without blocking even when all
// subscribers are at full capacity. This test would hang if the implementation blocked.
func TestHub_PublisherDoesNotBlockOnFullBuffer(t *testing.T) {
	t.Parallel()

	const bufSize = 1
	h := events.New(bufSize)

	_ = h.Subscribe("t")

	evt := events.Event{Type: "x"}
	h.Publish("t", evt) // fills the buffer — subscriber is now full
	h.Publish("t", evt) // must drop, not block — test would time-out if it does
}

// ── AC 3: Unsubscribe ────────────────────────────────────────────────────────

// TestHub_UnsubscribeRemovesSubscription verifies that after Unsubscribe, a subsequent Publish no longer
// reaches that subscription.
func TestHub_UnsubscribeRemovesSubscription(t *testing.T) {
	t.Parallel()

	h := events.New(4)
	sub := h.Subscribe("topic")
	sub.Unsubscribe()

	h.Publish("topic", events.Event{Type: "system.ready"})

	got := collect(sub, 1)
	if len(got) != 0 {
		t.Errorf("after Unsubscribe: want 0 events, got %d", len(got))
	}
}

// TestHub_UnsubscribeIsIdempotent verifies that calling Unsubscribe twice never panics.
func TestHub_UnsubscribeIsIdempotent(t *testing.T) {
	t.Parallel()

	h := events.New(4)
	sub := h.Subscribe("topic")

	// Must not panic.
	sub.Unsubscribe()
	sub.Unsubscribe()
}

// TestHub_ConcurrentDropAndUnsubscribeNoPanic verifies the race-free invariant: a concurrent slow-consumer
// drop (from Publish) and Unsubscribe can never double-close the channel. Run under -race; a panic from a
// double-close would surface here.
func TestHub_ConcurrentDropAndUnsubscribeNoPanic(t *testing.T) {
	t.Parallel()

	const (
		bufSize    = 1
		goroutines = 50
	)

	for range goroutines {
		h := events.New(bufSize)
		sub := h.Subscribe("t")

		// Fill the buffer up front so the racing publish below triggers the drop immediately, and gate both
		// goroutines on a shared start signal so they enter the drop-vs-Unsubscribe window together — without
		// the barrier the two could serialize (one finishing before the other starts) and never overlap.
		h.Publish("t", events.Event{Type: "x"}) // fills buffer (bufSize=1)

		start := make(chan struct{})

		var wg sync.WaitGroup
		wg.Add(2)

		// Goroutine 1: the overflowing publish that triggers the drop.
		go func() {
			defer wg.Done()
			<-start
			h.Publish("t", events.Event{Type: "x"}) // buffer full → triggers drop
		}()

		// Goroutine 2: unsubscribe concurrently, in the same window.
		go func() {
			defer wg.Done()
			<-start
			sub.Unsubscribe()
		}()

		close(start) // release both at once
		wg.Wait()
	}
}

// ── AC 4: defaultBufferSize constant and injectable buffer ───────────────────

// TestHub_DefaultBufferSizeConstantExists verifies that events.DefaultBufferSize is exported and positive.
// The production caller will pass it to New; the test-injected size in other tests proves the constructor
// param is honoured.
func TestHub_DefaultBufferSizeConstantExists(t *testing.T) {
	t.Parallel()

	if events.DefaultBufferSize <= 0 {
		t.Errorf("DefaultBufferSize must be positive, got %d", events.DefaultBufferSize)
	}
}

// TestHub_NonPositiveBufferSizeFallsBackToDefault verifies that New guards a non-positive buffer size: a
// zero (e.g. a misconfigured config value, unit 9) would otherwise yield an unbuffered channel, making every
// Publish send fall through to the non-blocking default and drop the subscriber on its first event. New must
// fall back to DefaultBufferSize so a single un-drained publish is buffered, not dropped.
func TestHub_NonPositiveBufferSizeFallsBackToDefault(t *testing.T) {
	t.Parallel()

	h := events.New(0)
	sub := h.Subscribe("topic")

	// One publish to an un-drained subscriber must be buffered (not dropped). With an unbuffered channel this
	// would fall through to default and the subscriber would receive nothing.
	h.Publish("topic", events.Event{Type: "system.ready"})

	got := collect(sub, 1)
	if len(got) != 1 {
		t.Errorf("New(0): want the publish buffered (fallback to DefaultBufferSize), got %d events", len(got))
	}
}

// TestHub_InjectableBufferSizeIsHonoured verifies that the hub's channel buffer size matches what was
// passed to New. A bufSize of 1 means a second publish to an un-drained subscriber drops it; a bufSize of 2
// means it is kept after one extra publish but dropped after two.
func TestHub_InjectableBufferSizeIsHonoured(t *testing.T) {
	t.Parallel()

	// With bufSize=2, 2 publishes fill the buffer but don't trigger a drop; the 3rd does.
	const bufSize = 2
	h := events.New(bufSize)
	sub := h.Subscribe("topic")

	evt := events.Event{Type: "x"}
	h.Publish("topic", evt)
	h.Publish("topic", evt)
	// 2 events in the buffer; subscriber is not yet dropped.
	got := collect(sub, 2)
	if len(got) != 2 {
		t.Errorf("bufSize=%d: want 2 buffered events before drop, got %d", bufSize, len(got))
	}

	// Now fill again and overflow — the 3rd publish triggers the drop.
	h.Publish("topic", evt)
	h.Publish("topic", evt)
	h.Publish("topic", evt) // drop happens here

	drainUntilClosed(sub) // channel must close; this must not block
}

// ── AC 5: concurrent stress — race detector ──────────────────────────────────

// TestHub_ConcurrentPublishSubscribeUnsubscribe is the race-detector stress test: many goroutines publish,
// subscribe, and unsubscribe on the same topic concurrently. Any data race surfaces under -race.
func TestHub_ConcurrentPublishSubscribeUnsubscribe(t *testing.T) {
	t.Parallel()

	const (
		bufSize   = 8
		workers   = 20
		publishes = 50
	)

	h := events.New(bufSize)
	evt := events.Event{Type: "stress.tick", Data: 42}

	var wg sync.WaitGroup

	for range workers {
		wg.Go(func() {
			sub := h.Subscribe("stress")

			for j := range publishes {
				h.Publish("stress", evt)

				// Drain periodically so this subscriber doesn't always become slow.
				if j%5 == 0 {
					collect(sub, bufSize)
				}
			}

			sub.Unsubscribe()
		})
	}

	wg.Wait()
}
