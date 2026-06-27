package cli

import (
	"testing"
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
