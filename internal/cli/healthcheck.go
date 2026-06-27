package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/qovira/qovira/internal/app"
)

// probeTimeout is the maximum time the health probe waits for a response.
// Kept short so an unreachable container fails fast in HEALTHCHECK evaluation.
const probeTimeout = 5 * time.Second

// Probe performs an HTTP GET against baseURL and returns nil if the response
// status is 200 OK, or a descriptive error otherwise. baseURL is the scheme +
// host (e.g. "http://127.0.0.1:8080"); Probe appends "/healthz" itself.
//
// Probe is exported so tests can drive it directly against an httptest.Server
// without going through cobra, mirroring the testability pattern of NewSPAHandler.
func Probe(ctx context.Context, baseURL string) error {
	url := baseURL + "/healthz"

	client := &http.Client{Timeout: probeTimeout}

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

// probeURLFromAddr converts a bind address (e.g. ":8080", "0.0.0.0:8080",
// "[::]:8080") into an HTTP URL suitable for an in-process health probe. An
// empty or wildcard host is replaced with "127.0.0.1" so the probe connects to
// loopback rather than the unspecified address.
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

// newHealthcheckCmd returns the `qovira healthcheck` subcommand. It resolves
// the target address from the same config as `serve` (QOVIRA_ADDR), probes
// /healthz in-process, and exits non-zero on any failure. No logic lives here —
// only config resolution, URL building, and delegation to Probe.
func newHealthcheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "healthcheck",
		Short: "Probe a running instance's /healthz endpoint",
		Long: `Probe the /healthz endpoint of a running Qovira instance.

Exits 0 if the instance responds with HTTP 200, non-zero otherwise.
The target address is resolved from QOVIRA_ADDR (same as 'serve').`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := app.LoadConfig(app.FlagOverrides{})
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			baseURL, err := probeURLFromAddr(cfg.Addr)
			if err != nil {
				return fmt.Errorf("resolve probe URL: %w", err)
			}

			return Probe(cmd.Context(), baseURL)
		},
	}
}
