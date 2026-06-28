package cli_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/qovira/qovira/internal/cli"
)

// TestProbe_200ReturnsNil verifies that a /healthz returning 200 maps to nil.
func TestProbe_200ReturnsNil(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	if err := cli.Probe(t.Context(), srv.URL); err != nil {
		t.Errorf("expected nil for 200 response, got: %v", err)
	}
}

// TestProbe_503ReturnsError verifies that a non-200 /healthz maps to an error.
func TestProbe_503ReturnsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	if err := cli.Probe(t.Context(), srv.URL); err == nil {
		t.Error("expected error for 503 response, got nil")
	}
}

// TestProbe_UnreachableReturnsError verifies that a closed/unreachable server maps to an error.
func TestProbe_UnreachableReturnsError(t *testing.T) {
	t.Parallel()

	// Start a server, capture its URL, then close it before probing.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	url := srv.URL
	srv.Close() // closed before probe — connection refused

	if err := cli.Probe(t.Context(), url); err == nil {
		t.Error("expected error for unreachable server, got nil")
	}
}

// TestProbe_CancelledContextReturnsError verifies Probe honors its context: a context cancelled before the call makes
// the request fail rather than hang.
func TestProbe_CancelledContextReturnsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel before probing — Probe must surface the context error

	if err := cli.Probe(ctx, srv.URL); err == nil {
		t.Error("expected error for cancelled context, got nil")
	}
}

// TestExecute_HealthcheckHelp verifies the subcommand is registered and --help exits 0.
func TestExecute_HealthcheckHelp(t *testing.T) {
	t.Parallel()

	code := cli.ExecuteArgsWithOutput([]string{"healthcheck", "--help"}, io.Discard)
	if code != 0 {
		t.Errorf("healthcheck --help: expected exit 0, got %d", code)
	}
}

// TestExecute_HealthcheckExitsZeroOnRunningServer verifies the full cobra-to-probe path exits 0 when the target
// returns 200.
// Not parallel — uses t.Setenv.
func TestExecute_HealthcheckExitsZeroOnRunningServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	// QOVIRA_ADDR is a bind address without scheme; srv.Listener.Addr gives host:port.
	t.Setenv("QOVIRA_ADDR", srv.Listener.Addr().String())

	code := cli.ExecuteArgsWithOutput([]string{"healthcheck"}, io.Discard)
	if code != 0 {
		t.Errorf("healthcheck: expected exit 0 against running server, got %d", code)
	}
}

// TestExecute_HealthcheckExitsNonZeroOnDownServer verifies the full cobra-to-probe path exits non-zero when the target
// is unreachable.
// Not parallel — uses t.Setenv.
func TestExecute_HealthcheckExitsNonZeroOnDownServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	addr := srv.Listener.Addr().String()
	srv.Close() // close before healthcheck runs

	t.Setenv("QOVIRA_ADDR", addr)

	code := cli.ExecuteArgsWithOutput([]string{"healthcheck"}, io.Discard)
	if code == 0 {
		t.Error("healthcheck: expected non-zero exit for unreachable server, got 0")
	}
}
