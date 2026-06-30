package app_test

// graceful_shutdown_test.go tests the gracefulShutdown helper in isolation using fakes.
//
// Test mapping:
//   - TestGracefulShutdown_HubTimeoutAndSrvTimeoutReturnsNilAndCallsClose — the bug under fix:
//     when hub.Shutdown exhausts its budget (DeadlineExceeded) AND srv.Shutdown hits its own
//     budget (DeadlineExceeded), Run's shutdown logic must return nil — not promote the deadline
//     error to a fatal failure — and must call srv.Close() to force-close wedged connections.
//   - TestGracefulShutdown_HappyPathReturnNilNoClose — both sides drain cleanly; nil is returned
//     and Close is never called.
//   - TestGracefulShutdown_HubTimeoutSrvSucceedsReturnsNil — hub times out but srv drains within
//     its fresh budget; still returns nil, no Close needed.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/app"
)

// fakeHubShutdowner implements app.HubShutdowner for tests.
type fakeHubShutdowner struct {
	err error
}

func (f *fakeHubShutdowner) Shutdown(_ context.Context) error { return f.err }

// fakeServer implements app.ServerShutdowner for tests.
type fakeServer struct {
	shutdownErr error
	closeCalled bool
	closeErr    error
}

func (f *fakeServer) Shutdown(_ context.Context) error { return f.shutdownErr }

func (f *fakeServer) Close() error {
	f.closeCalled = true
	return f.closeErr
}

// TestGracefulShutdown_HubTimeoutAndSrvTimeoutReturnsNilAndCallsClose is the primary regression test
// for the bug: a single wedged client (hub drain times out, srv.Shutdown times out on its own budget)
// must NOT make Run fail — gracefulShutdown must return nil and call srv.Close() to force-close.
func TestGracefulShutdown_HubTimeoutAndSrvTimeoutReturnsNilAndCallsClose(t *testing.T) {
	t.Parallel()

	hub := &fakeHubShutdowner{err: context.DeadlineExceeded}
	srv := &fakeServer{shutdownErr: context.DeadlineExceeded}

	err := app.GracefulShutdown(context.Background(), hub, srv, time.Millisecond)

	if err != nil {
		t.Errorf("GracefulShutdown: want nil when both sides time out, got %v", err)
	}

	if !srv.closeCalled {
		t.Error("GracefulShutdown: srv.Close() must be called when srv.Shutdown times out, but was not")
	}
}

// TestGracefulShutdown_HappyPathReturnNilNoClose verifies the clean path: both sides drain within
// budget — nil is returned and Close is never called (no wedged connections to force-close).
func TestGracefulShutdown_HappyPathReturnNilNoClose(t *testing.T) {
	t.Parallel()

	hub := &fakeHubShutdowner{err: nil}
	srv := &fakeServer{shutdownErr: nil}

	err := app.GracefulShutdown(context.Background(), hub, srv, time.Millisecond)

	if err != nil {
		t.Errorf("GracefulShutdown: want nil on clean shutdown, got %v", err)
	}

	if srv.closeCalled {
		t.Error("GracefulShutdown: srv.Close() must NOT be called on clean shutdown")
	}
}

// TestGracefulShutdown_HubTimeoutSrvSucceedsReturnsNil verifies that a hub drain timeout does not
// prevent srv from getting a fresh budget: if srv drains cleanly, the result is still nil and no
// force-close is needed.
func TestGracefulShutdown_HubTimeoutSrvSucceedsReturnsNil(t *testing.T) {
	t.Parallel()

	hub := &fakeHubShutdowner{err: context.DeadlineExceeded}
	srv := &fakeServer{shutdownErr: nil}

	err := app.GracefulShutdown(context.Background(), hub, srv, time.Millisecond)

	if err != nil {
		t.Errorf("GracefulShutdown: want nil when hub times out but srv succeeds, got %v", err)
	}

	if srv.closeCalled {
		t.Error("GracefulShutdown: srv.Close() must NOT be called when srv.Shutdown succeeds")
	}
}

// TestGracefulShutdown_SrvNonTimeoutErrorIsSurfaced verifies that a non-deadline srv.Shutdown error
// (e.g. a genuine unexpected failure) is still surfaced — only deadline/timeout errors are tolerated.
func TestGracefulShutdown_SrvNonTimeoutErrorIsSurfaced(t *testing.T) {
	t.Parallel()

	hub := &fakeHubShutdowner{err: nil}

	srvErr := errors.New("unexpected transport failure")
	srv := &fakeServer{shutdownErr: srvErr}

	err := app.GracefulShutdown(context.Background(), hub, srv, time.Millisecond)

	if err == nil {
		t.Fatal("GracefulShutdown: want non-nil error when srv.Shutdown returns a non-timeout error, got nil")
	}

	if !errors.Is(err, srvErr) {
		t.Errorf("GracefulShutdown: want error wrapping %v, got %v", srvErr, err)
	}
}
