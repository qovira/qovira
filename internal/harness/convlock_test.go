package harness

// convlock_test.go — unit tests for the keyed per-conversation lock. In the harness package (not harness_test) to
// access unexported types.

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestConvLocks_SerialisesSameKey verifies that two goroutines for the same conversationID do not overlap — a counter
// incremented-and-decremented inside the lock must never exceed 1.
func TestConvLocks_SerialisesSameKey(t *testing.T) {
	t.Parallel()

	cl := newConvLocks()
	const convID = "conv-A"

	var maxConcurrent atomic.Int64
	var current atomic.Int64

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			entry := cl.acquire(convID)
			entry.mu.Lock()
			defer func() {
				entry.mu.Unlock()
				cl.release(convID)
			}()

			// Track concurrency inside the critical section.
			cur := current.Add(1)
			for {
				old := maxConcurrent.Load()
				if cur <= old || maxConcurrent.CompareAndSwap(old, cur) {
					break
				}
			}
			time.Sleep(time.Millisecond) // hold for a tick to expose races
			current.Add(-1)
		}()
	}
	wg.Wait()

	if got := maxConcurrent.Load(); got > 1 {
		t.Errorf("maxConcurrent = %d, want 1 — goroutines overlapped inside the lock", got)
	}
}

// TestConvLocks_IndependentKeys verifies that different conversation keys do not block each other — they should run
// fully concurrently.
func TestConvLocks_IndependentKeys(t *testing.T) {
	t.Parallel()

	cl := newConvLocks()

	const convA, convB = "conv-A", "conv-B"

	started := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	// Goroutine A: acquires convA lock and signals started, then holds it briefly.
	go func() {
		defer wg.Done()
		entry := cl.acquire(convA)
		entry.mu.Lock()
		defer func() {
			entry.mu.Unlock()
			cl.release(convA)
		}()

		close(started)
		time.Sleep(20 * time.Millisecond) // hold the convA lock
	}()

	// Goroutine B: waits for A to start, then acquires convB lock. If keys were shared, this would deadlock or
	// significantly delay.
	go func() {
		defer wg.Done()
		<-started
		begin := time.Now()
		entry := cl.acquire(convB)
		entry.mu.Lock()
		defer func() {
			entry.mu.Unlock()
			cl.release(convB)
		}()
		elapsed := time.Since(begin)
		// B should not have had to wait more than a few milliseconds for convB.
		if elapsed > 10*time.Millisecond {
			t.Errorf("acquiring different conv key took %v — keys are not independent", elapsed)
		}
	}()

	wg.Wait()
}

// TestConvLocks_NoLeak verifies that entries are removed from the map once all waiters have finished (no unbounded map
// growth).
func TestConvLocks_NoLeak(t *testing.T) {
	t.Parallel()

	cl := newConvLocks()

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		// Mix a small number of distinct keys so we exercise both creation and cleanup of entries under contention.
		convID := [2]string{"conv-X", "conv-Y"}[i%2]
		go func() {
			defer wg.Done()
			entry := cl.acquire(convID)
			// Acquire then immediately release — exercises refcount decrement to zero and map-entry cleanup without
			// doing any real work under the lock.
			entry.mu.Lock()
			locked := true // minimal work so the critical section is non-empty
			entry.mu.Unlock()
			_ = locked
			cl.release(convID)
		}()
	}
	wg.Wait()

	cl.mu.Lock()
	remaining := len(cl.entries)
	cl.mu.Unlock()

	if remaining != 0 {
		t.Errorf("convLocks map has %d entries after all goroutines finished, want 0 (leak)", remaining)
	}
}
