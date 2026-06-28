package cli_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/cli"
)

// testProbeTimeout is a generous per-request timeout for tests that expect the probe to complete or fail on
// connection (not on the timeout itself).
const testProbeTimeout = 5 * time.Second

// TestProbe_200ReturnsNil verifies that a /healthz returning 200 maps to nil.
func TestProbe_200ReturnsNil(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	if err := cli.Probe(t.Context(), srv.URL, testProbeTimeout); err != nil {
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

	if err := cli.Probe(t.Context(), srv.URL, testProbeTimeout); err == nil {
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

	if err := cli.Probe(t.Context(), url, testProbeTimeout); err == nil {
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

	if err := cli.Probe(ctx, srv.URL, testProbeTimeout); err == nil {
		t.Error("expected error for cancelled context, got nil")
	}
}

// TestExecute_HealthcheckHelp verifies the subcommand is registered and --help exits 0.
func TestExecute_HealthcheckHelp(t *testing.T) {
	t.Parallel()

	code := cli.ExecuteArgsWithOutput([]string{"healthcheck", "--help"}, io.Discard, io.Discard)
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

	code := cli.ExecuteArgsWithOutput([]string{"healthcheck"}, io.Discard, io.Discard)
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

	code := cli.ExecuteArgsWithOutput([]string{"healthcheck"}, io.Discard, io.Discard)
	if code == 0 {
		t.Error("healthcheck: expected non-zero exit for unreachable server, got 0")
	}
}

// TestExecute_HealthcheckAddrFlagOverridesEnv verifies the --addr persistent flag wins over QOVIRA_ADDR through
// the full cobra-to-probe path: the env points at a dead port while the flag points at the live server, so a
// 0 exit proves the flag took precedence.
// Not parallel — uses t.Setenv.
func TestExecute_HealthcheckAddrFlagOverridesEnv(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	t.Setenv("QOVIRA_ADDR", "127.0.0.1:1") // nothing listens on port 1

	code := cli.ExecuteArgsWithOutput([]string{"healthcheck", "--addr", srv.Listener.Addr().String()}, io.Discard, io.Discard)
	if code != 0 {
		t.Errorf("healthcheck --addr <live>: expected exit 0 (flag over env), got %d", code)
	}
}

// TestExecute_HealthcheckInvalidTimeoutEnvExitsNonZero verifies a malformed QOVIRA_HEALTHCHECK_TIMEOUT fails the
// command (before probing) and names the offending variable on stderr.
// Not parallel — uses t.Setenv.
func TestExecute_HealthcheckInvalidTimeoutEnvExitsNonZero(t *testing.T) {
	t.Setenv("QOVIRA_HEALTHCHECK_TIMEOUT", "not-a-duration")

	var errOut strings.Builder
	code := cli.ExecuteArgsWithOutput([]string{"healthcheck"}, io.Discard, &errOut)
	if code == 0 {
		t.Fatal("healthcheck: expected non-zero exit for malformed timeout env, got 0")
	}

	if !strings.Contains(errOut.String(), "QOVIRA_HEALTHCHECK_TIMEOUT") {
		t.Errorf("expected error to name QOVIRA_HEALTHCHECK_TIMEOUT, got %q", errOut.String())
	}
}
