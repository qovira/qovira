package cli

import (
	"testing"
	"time"
)

func TestProbeURLFromAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		addr    string
		want    string
		wantErr bool
	}{
		{
			name: "empty host colon port",
			addr: ":8080",
			want: "http://127.0.0.1:8080",
		},
		{
			name: "IPv4 wildcard",
			addr: "0.0.0.0:8080",
			want: "http://127.0.0.1:8080",
		},
		{
			name: "IPv6 wildcard bracketed",
			addr: "[::]:8080",
			want: "http://127.0.0.1:8080",
		},
		{
			name: "explicit loopback",
			addr: "127.0.0.1:8080",
			want: "http://127.0.0.1:8080",
		},
		{
			name:    "missing port",
			addr:    "8080",
			wantErr: true,
		},
		{
			name:    "bare IPv6 without brackets — too many colons",
			addr:    "::8080",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := probeURLFromAddr(tt.addr)
			if tt.wantErr {
				if err == nil {
					t.Errorf("probeURLFromAddr(%q): expected error, got nil (url=%q)", tt.addr, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("probeURLFromAddr(%q): unexpected error: %v", tt.addr, err)
			}

			if got != tt.want {
				t.Errorf("probeURLFromAddr(%q) = %q, want %q", tt.addr, got, tt.want)
			}
		})
	}
}

// TestResolveTimeout covers the flag > env > default precedence and the error paths (malformed env, and a
// non-positive resolved timeout from either source).
func TestResolveTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		flagVal     time.Duration
		flagChanged bool
		env         string
		want        time.Duration
		wantErr     bool
	}{
		{
			name: "default when nothing set",
			want: defaultProbeTimeout,
		},
		{
			name: "env overrides default",
			env:  "2s",
			want: 2 * time.Second,
		},
		{
			name:        "flag overrides env",
			flagVal:     3 * time.Second,
			flagChanged: true,
			env:         "2s",
			want:        3 * time.Second,
		},
		{
			name:        "flag overrides default",
			flagVal:     1 * time.Second,
			flagChanged: true,
			want:        1 * time.Second,
		},
		{
			name:    "malformed env errors",
			env:     "nope",
			wantErr: true,
		},
		{
			name:    "non-positive env errors",
			env:     "0s",
			wantErr: true,
		},
		{
			name:        "negative flag errors",
			flagVal:     -1 * time.Second,
			flagChanged: true,
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveTimeout(tt.flagVal, tt.flagChanged, tt.env)
			if tt.wantErr {
				if err == nil {
					t.Errorf("resolveTimeout(%s, %v, %q): expected error, got nil (timeout=%s)", tt.flagVal, tt.flagChanged, tt.env, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("resolveTimeout(%s, %v, %q): unexpected error: %v", tt.flagVal, tt.flagChanged, tt.env, err)
			}

			if got != tt.want {
				t.Errorf("resolveTimeout(%s, %v, %q) = %s, want %s", tt.flagVal, tt.flagChanged, tt.env, got, tt.want)
			}
		})
	}
}
