package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/qovira/qovira/internal/app"
)

// defaultProbeTimeout is the fallback health-probe timeout when neither --timeout nor
// QOVIRA_HEALTHCHECK_TIMEOUT is set. Kept short so an unreachable container fails fast in HEALTHCHECK
// evaluation.
const defaultProbeTimeout = 5 * time.Second

// timeoutEnv is the environment variable that overrides the default probe timeout (but loses to --timeout).
const timeoutEnv = "QOVIRA_HEALTHCHECK_TIMEOUT"

// Probe performs an HTTP GET against baseURL and returns nil if the response status is 200 OK, or a
// descriptive error otherwise. baseURL is the scheme + host (e.g. "http://127.0.0.1:8080"); Probe appends
// "/healthz" itself. timeout bounds the whole request.
//
// Probe is exported so tests can drive it directly against an httptest.Server without going through cobra,
// mirroring the testability pattern of NewSPAHandler.
func Probe(ctx context.Context, baseURL string, timeout time.Duration) error {
	url := baseURL + "/healthz"

	client := &http.Client{Timeout: timeout}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("probe %s: %w", url, err)
	}

	defer func() {
		// Best-effort drain so the connection is returned to the pool cleanly.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("probe %s: unexpected status %d", url, resp.StatusCode)
	}

	return nil
}

// probeURLFromAddr converts a bind address (e.g. ":8080", "0.0.0.0:8080", "[::]:8080") into an HTTP URL
// suitable for an in-process health probe. An empty or wildcard host is replaced with "127.0.0.1" so the
// probe connects to loopback rather than the unspecified address.
func probeURLFromAddr(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("parse addr %q: %w", addr, err)
	}

	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}

	return "http://" + net.JoinHostPort(host, port), nil
}

// resolveTimeout picks the probe timeout with precedence flag > env > default. flagVal/flagChanged come from
// the --timeout flag (flagChanged is cobra's Changed("timeout")); env is the raw QOVIRA_HEALTHCHECK_TIMEOUT
// value ("" when unset). A malformed env value or a non-positive resolved timeout is an error.
func resolveTimeout(flagVal time.Duration, flagChanged bool, env string) (time.Duration, error) {
	timeout := defaultProbeTimeout

	if env != "" {
		d, err := time.ParseDuration(env)
		if err != nil {
			return 0, fmt.Errorf("parse %s %q: %w", timeoutEnv, env, err)
		}

		timeout = d
	}

	if flagChanged {
		timeout = flagVal
	}

	if timeout <= 0 {
		return 0, fmt.Errorf("healthcheck timeout must be positive, got %s", timeout)
	}

	return timeout, nil
}

// newHealthcheckCmd returns the `qovira healthcheck` subcommand. It resolves the target address from the same
// config as `serve` (--addr / QOVIRA_ADDR) and the probe timeout from --timeout / QOVIRA_HEALTHCHECK_TIMEOUT,
// probes /healthz in-process, and exits non-zero on any failure. No logic lives here — only config
// resolution, URL building, and delegation to Probe.
func newHealthcheckCmd(addr *string) *cobra.Command {
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "healthcheck",
		Short: "Probe a running instance's /healthz endpoint",
		Long: `Probe the /healthz endpoint of a running Qovira instance.

Exits 0 if the instance responds with HTTP 200, non-zero otherwise.
The target address is resolved from --addr or QOVIRA_ADDR (same as 'serve').
The probe timeout is resolved from --timeout or QOVIRA_HEALTHCHECK_TIMEOUT (default 5s).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var overrides app.FlagOverrides

			// cmd.Flags() exposes the inherited persistent --addr flag once cobra has parsed it.
			if cmd.Flags().Changed("addr") {
				overrides.Addr = addr
			}

			cfg, err := app.LoadConfig(overrides)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			baseURL, err := probeURLFromAddr(cfg.Addr)
			if err != nil {
				return fmt.Errorf("resolve probe URL: %w", err)
			}

			resolved, err := resolveTimeout(timeout, cmd.Flags().Changed("timeout"), os.Getenv(timeoutEnv))
			if err != nil {
				return err
			}

			return Probe(cmd.Context(), baseURL, resolved)
		},
	}

	cmd.Flags().DurationVar(&timeout, "timeout", defaultProbeTimeout, "probe timeout, e.g. 5s (env: "+timeoutEnv+")")

	return cmd
}
