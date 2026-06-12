package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDialAddr verifies the helper that converts listen addresses to
// loopback dial addresses.
func TestDialAddr(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "empty host binds to 127.0.0.1",
			input: ":8080",
			want:  "127.0.0.1:8080",
		},
		{
			name:  "all-interfaces address binds to 127.0.0.1",
			input: "0.0.0.0:8080",
			want:  "127.0.0.1:8080",
		},
		{
			name:  "explicit loopback address unchanged",
			input: "127.0.0.1:9000",
			want:  "127.0.0.1:9000",
		},
		{
			name:  "explicit non-loopback address unchanged",
			input: "192.168.1.1:8080",
			want:  "192.168.1.1:8080",
		},
		{
			name:  "non-standard port preserved",
			input: "0.0.0.0:12345",
			want:  "127.0.0.1:12345",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := dialAddr(tc.input)
			if got != tc.want {
				t.Errorf("dialAddr(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestHealthcheckCmd exercises newHealthcheckCmd against an httptest.Server
// to verify the exit behaviour for HTTP 200 (success) and HTTP 503 (error),
// without requiring a real config file or master key.
//
// Not marked t.Parallel() at the top level because the subtests use t.Setenv,
// which is incompatible with parallel execution.
func TestHealthcheckCmd(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		wantErr    bool
		errContain string
	}{
		{
			name:       "HTTP 200 exits successfully",
			statusCode: http.StatusOK,
			wantErr:    false,
		},
		{
			name:       "HTTP 503 exits with error",
			statusCode: http.StatusServiceUnavailable,
			wantErr:    true,
			errContain: "503",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// t.Setenv is incompatible with t.Parallel() — subtests that mutate
			// environment variables must run sequentially within this test function.

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.statusCode)
			}))
			t.Cleanup(srv.Close)

			// Build the command and point it at the test server address via
			// QOVIRA_HTTP_ADDR. We also supply a dummy master key so config.Load
			// passes validation (the healthcheck command calls config.Load to read
			// the listen address).
			t.Setenv("QOVIRA_HTTP_ADDR", srv.Listener.Addr().String())
			t.Setenv("QOVIRA_MASTER_KEY", "this-is-sixteen-bytes-ok")

			cmd := newHealthcheckCmd()
			cmd.SetArgs([]string{})

			err := cmd.Execute()

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tc.errContain != "" && !strings.Contains(err.Error(), tc.errContain) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.errContain)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
