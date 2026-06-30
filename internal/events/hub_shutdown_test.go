package events_test

// hub_shutdown_test.go tests Hub.Shutdown, Hub.Done, connStart/connDone lifecycle, and the WaitGroup
// race-safety invariant. Every test here was written BEFORE the implementation code (TDD: watch it fail
// for the right reason first).
//
// Test mapping to acceptance criteria:
//   - TestHub_ShutdownDrainsAllConnections       — AC 2: multi-connection drain within a deadline
//   - TestHub_ShutdownWedgedClientBoundedByCtx   — AC 2: wedged client / ctx-deadline path
//   - TestHub_ShutdownIdempotent                 — second Shutdown call must not double-close done
//   - TestHub_ConnectAfterShutdownSafe           — AC 2 + sharp edge: connection arriving after Shutdown
//   - TestHub_DoneChannelSignalsShutdown         — Hub.Done() closes when Shutdown is called
//   - TestHub_ConcurrentConnectShutdownNoRace    — -race harness for Add-vs-Wait

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/events"
)

// TestHub_DoneChannelSignalsShutdown verifies that Hub.Done() returns a channel that is closed exactly
// when Shutdown is called and that the channel is non-nil even before Shutdown.
func TestHub_DoneChannelSignalsShutdown(t *testing.T) {
	t.Parallel()

	h := events.New(4)

	// Before Shutdown the channel must be open (select on it must take the default branch).
	select {
	case <-h.Done():
		t.Fatal("Done() channel was already closed before Shutdown")
	default:
		// expected
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := h.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: want nil, got %v", err)
	}

	// After Shutdown the channel must be closed.
	select {
	case <-h.Done():
		// expected
	default:
		t.Fatal("Done() channel was not closed after Shutdown")
	}
}

// TestHub_ShutdownDrainsAllConnections verifies that when multiple connections are registered via
// ConnStart/ConnDone, Shutdown waits until all goroutines call ConnDone, and returns nil (not ctx.Err())
// when they all drain well within the deadline.
func TestHub_ShutdownDrainsAllConnections(t *testing.T) {
	t.Parallel()

	h := events.New(4)

	const conns = 5

	// Pre-register a handful of connections, each draining promptly once Done() fires.
	for range conns {
		if ok := h.ConnStart(); !ok {
			t.Fatal("ConnStart returned false before Shutdown was called")
		}

		go func() {
			<-h.Done()
			h.ConnDone()
		}()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()

	if err := h.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: want nil (all drained), got %v", err)
	}

	elapsed := time.Since(start)

	// The drain should complete far faster than the 2 s budget. Use a generous 500 ms margin to avoid
	// flakiness on loaded CI, but confirm it is not blocking the full budget.
	if elapsed > 500*time.Millisecond {
		t.Errorf("Shutdown took %v — expected well under 500 ms when all conns drain promptly", elapsed)
	}
}

// TestHub_ShutdownWedgedClientBoundedByCtx verifies that Shutdown returns ctx.Err() when a connection
// goroutine never calls ConnDone and the ctx deadline fires first. The wedged connection must not cause
// Shutdown to hang indefinitely.
func TestHub_ShutdownWedgedClientBoundedByCtx(t *testing.T) {
	t.Parallel()

	h := events.New(4)

	// Register one connection that will never call ConnDone (the wedged client).
	ok := h.ConnStart()
	if !ok {
		t.Fatal("ConnStart returned false before Shutdown was called")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := h.Shutdown(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Shutdown: want ctx.Err(), got nil — wedged client was not bounded by ctx deadline")
	}

	// Must be exactly the ctx deadline error, not some other error.
	if err != ctx.Err() {
		t.Errorf("Shutdown: want ctx.Err() (%v), got %v", ctx.Err(), err)
	}

	// Must not have taken significantly longer than the deadline.
	if elapsed > 500*time.Millisecond {
		t.Errorf("Shutdown took %v — expected close to 100 ms (ctx deadline), not blocked indefinitely", elapsed)
	}

	// Unblock the wedged client now so the WaitGroup is eventually satisfied (avoids leaking goroutines
	// into -race analysis after the test ends).
	h.ConnDone()
}

// TestHub_ShutdownIdempotent verifies that calling Shutdown twice does not panic (no double-close of done).
func TestHub_ShutdownIdempotent(t *testing.T) {
	t.Parallel()

	h := events.New(4)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := h.Shutdown(ctx); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}

	// Second call must not panic.
	if err := h.Shutdown(ctx); err != nil {
		// Second Shutdown may return ctx.Err() or nil — both are acceptable. What's not acceptable is a panic.
		t.Logf("second Shutdown returned %v — acceptable (no panic)", err)
	}
}

// TestHub_ConnectAfterShutdownSafe verifies the sharp edge: a ConnStart call that arrives after Shutdown
// has closed done must not call wg.Add (which would race wg.Wait) and must return false to signal the
// caller that shutdown is in progress.
func TestHub_ConnectAfterShutdownSafe(t *testing.T) {
	t.Parallel()

	h := events.New(4)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// Shutdown with no registered connections — drains immediately.
	if err := h.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// ConnStart after Shutdown must return false (don't register — done is already closed).
	ok := h.ConnStart()
	if ok {
		t.Fatal("ConnStart after Shutdown returned true — must return false to avoid wg.Add after wg.Wait")
	}
}

// TestHub_ConcurrentConnectShutdownNoRace is the -race harness for the Add-vs-Wait edge case.
// Many goroutines race ConnStart/ConnDone against Shutdown. The race detector must see no violations.
func TestHub_ConcurrentConnectShutdownNoRace(t *testing.T) {
	t.Parallel()

	const rounds = 20

	for range rounds {
		h := events.New(4)

		var wg sync.WaitGroup

		const connGoroutines = 10

		// Goroutines that try to register connections concurrently with Shutdown.
		for range connGoroutines {
			wg.Go(func() {
				ok := h.ConnStart()
				if ok {
					// Registered: wait for done then deregister.
					<-h.Done()
					h.ConnDone()
				}
				// Not registered: nothing to do.
			})
		}

		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)

		if err := h.Shutdown(ctx); err != nil {
			// ctx timeout is acceptable if some goroutine hasn't drained — the point is no race.
			t.Logf("Shutdown round returned %v (acceptable)", err)
		}

		cancel()
		wg.Wait()
	}
}
