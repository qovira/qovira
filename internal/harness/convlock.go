package harness

import "sync"

// convLocks serialises concurrent run goroutines on a per-conversation basis. At most one run goroutine may execute
// for a given conversationID at a time.
//
// Design: a map[string]*entry guarded by a top-level mutex. Each entry holds a *sync.Mutex for the conversation and a
// reference count. Acquire increments the refcount under the guard mutex, then blocks on the entry mutex (outside the
// guard mutex to avoid holding it across the turn). Release decrements the refcount and removes the entry when it
// reaches zero, preventing unbounded map growth.
//
// The guard mutex is held only long enough to read/write the map; the per-conversation mutex is acquired after the
// guard is released, so a long-running turn never blocks other conversations from being scheduled.
type convLocks struct {
	mu      sync.Mutex
	entries map[string]*convLockEntry
}

type convLockEntry struct {
	mu       sync.Mutex
	refcount int
}

// newConvLocks constructs an empty convLocks.
func newConvLocks() *convLocks {
	return &convLocks{entries: make(map[string]*convLockEntry)}
}

// acquire returns the entry for convID with its refcount incremented but the per-conversation mutex NOT yet locked.
// The caller must call entry.mu.Lock() to enter the critical section, and then call release(convID) in a defer.
//
// Typical usage:
//
//	entry := cl.acquire(convID)
//	entry.mu.Lock()
//	defer func() {
//	    entry.mu.Unlock()
//	    cl.release(convID)
//	}()
func (cl *convLocks) acquire(convID string) *convLockEntry {
	cl.mu.Lock()
	e, ok := cl.entries[convID]
	if !ok {
		e = &convLockEntry{}
		cl.entries[convID] = e
	}
	e.refcount++
	cl.mu.Unlock()
	return e
}

// release decrements the refcount for convID and removes the entry when it reaches zero. Must be called after the
// per-conversation mutex is unlocked.
func (cl *convLocks) release(convID string) {
	cl.mu.Lock()
	e, ok := cl.entries[convID]
	if ok {
		e.refcount--
		if e.refcount == 0 {
			delete(cl.entries, convID)
		}
	}
	cl.mu.Unlock()
}
