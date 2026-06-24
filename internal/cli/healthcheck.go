package cli

import (
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/qovira/qovira/internal/config"
)

// healthcheckTimeout is the total request deadline for the health probe. Short enough to stay well inside Docker's
// --timeout=5s.
const healthcheckTimeout = 3 * time.Second

// newHealthcheckCmd returns the healthcheck subcommand. It performs a single HTTP GET to the local server's /healthz
// endpoint and exits 0 on HTTP 200, non-zero on any error or non-200 status. This is the exec-form target for the
// Dockerfile HEALTHCHECK instruction — distroless images have no shell or curl, so the app supplies its own probe.
func newHealthcheckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "healthcheck",
		Short: "Check that the local Qovira server is healthy",
		Long: `Perform a single HTTP GET to /healthz on the configured listen address and
exit 0 if the server responds HTTP 200. Exit non-zero on any error or
non-200 status. Designed to be called by the Dockerfile HEALTHCHECK instruction.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath, _ := cmd.Flags().GetString("config")

			cfg, err := config.Load(cfgPath)
			if err != nil {
				return fmt.Errorf("healthcheck: load config: %w", err)
			}

			addr := dialAddr(cfg.HTTPAddr)
			url := "http://" + addr + "/healthz"

			client := &http.Client{Timeout: healthcheckTimeout}
			resp, err := client.Get(url) //nolint:noctx // intentional: healthcheck is a one-shot CLI command
			if err != nil {
				return fmt.Errorf("healthcheck: GET %s: %w", url, err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("healthcheck: %s returned HTTP %d", url, resp.StatusCode)
			}

			return nil
		},
	}

	cmd.Flags().String("config", "", "path to TOML config file (optional; env vars and defaults are used when unset)")

	return cmd
}

// dialAddr converts a listen address (as stored in cfg.HTTPAddr) to a dial address suitable for an outbound HTTP
// connection to the local server.
//
// Rules:
//   - ":8080"       → "127.0.0.1:8080"  (empty host)
//   - "0.0.0.0:8080" → "127.0.0.1:8080"  (all-interfaces bind)
//   - "192.168.1.1:8080" → "192.168.1.1:8080" (explicit host unchanged)
func dialAddr(listenAddr string) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		// Malformed address: return as-is; the caller will surface an error.
		return listenAddr
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
