package gateway

import (
	"testing"
)

// TestChatEndpointURL verifies that all four common operator-supplied base URL
// variants resolve to the same canonical chat/completions endpoint URL (AC3).
func TestChatEndpointURL(t *testing.T) {
	t.Parallel()

	const want = "https://api.example.com/v1/chat/completions"

	tests := []struct {
		name    string
		rawBase string
		want    string
		wantErr bool
	}{
		{
			name:    "bare host with trailing slash",
			rawBase: "https://api.example.com/",
			want:    want,
		},
		{
			name:    "bare host without trailing slash",
			rawBase: "https://api.example.com",
			want:    want,
		},
		{
			name:    "with /v1 no trailing slash",
			rawBase: "https://api.example.com/v1",
			want:    want,
		},
		{
			name:    "with /v1 and trailing slash",
			rawBase: "https://api.example.com/v1/",
			want:    want,
		},
		{
			name:    "subpath host with trailing slash",
			rawBase: "https://proxy.example.com/openai/",
			want:    "https://proxy.example.com/openai/v1/chat/completions",
		},
		{
			name:    "subpath host with /v1 no trailing slash",
			rawBase: "https://proxy.example.com/openai/v1",
			want:    "https://proxy.example.com/openai/v1/chat/completions",
		},
		{
			name:    "no scheme",
			rawBase: "api.example.com/v1",
			wantErr: true,
		},
		{
			name:    "empty string",
			rawBase: "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := chatEndpointURL(tt.rawBase)
			if tt.wantErr {
				if err == nil {
					t.Errorf("chatEndpointURL(%q) = %q, want error", tt.rawBase, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("chatEndpointURL(%q): unexpected error: %v", tt.rawBase, err)
			}
			if got != tt.want {
				t.Errorf("chatEndpointURL(%q) = %q, want %q", tt.rawBase, got, tt.want)
			}
		})
	}
}
