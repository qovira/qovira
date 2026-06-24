package gateway

import (
	"strings"
	"testing"
)

// ── GW1 #2: v1BaseURL rejects non-http/https schemes and embedded userinfo ────

// TestV1BaseURL_SchemeValidation verifies that v1BaseURL rejects non-http/https
// schemes (gopher, ftp, file) and accepts only http/https, and that error
// messages never leak embedded credentials.
func TestV1BaseURL_SchemeValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		rawBase    string
		wantErr    bool
		errSub     string   // substring expected in the error message
		errNotSubs []string // substrings that must NOT appear in the error
	}{
		// Must be accepted.
		{name: "https accepted", rawBase: "https://api.example.com", wantErr: false},
		{name: "http accepted", rawBase: "http://api.example.com", wantErr: false},
		{name: "https with v1", rawBase: "https://api.example.com/v1", wantErr: false},
		{name: "https with port and path", rawBase: "https://api.example.com:8443/proxy", wantErr: false},
		// Must be rejected — non-http/https schemes.
		{name: "gopher rejected", rawBase: "gopher://internal:70/", wantErr: true, errSub: "scheme"},
		{name: "ftp rejected", rawBase: "ftp://files.example.com/", wantErr: true, errSub: "scheme"},
		{name: "file rejected", rawBase: "file:///etc/passwd", wantErr: true, errSub: "scheme"},
		// Must be rejected — embedded userinfo. Error must NOT echo the password.
		//nolint:gosec // G101: test fixture, not real credentials
		{
			name:       "userinfo rejected — no password in error",
			rawBase:    "https://user:s3cr3t@api.example.com",
			wantErr:    true,
			errSub:     "userinfo",
			errNotSubs: []string{"s3cr3t", "user:s3cr3t"},
		},
		// gopher + userinfo: error must not echo the password regardless of which
		// guard fires first.
		//nolint:gosec // G101: test fixture, not real credentials
		{
			name:       "gopher+userinfo — no password in error",
			rawBase:    "gopher://user:s3cr3t@internal:70/",
			wantErr:    true,
			errNotSubs: []string{"s3cr3t", "user:s3cr3t"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := v1BaseURL(tt.rawBase)
			if tt.wantErr {
				if err == nil {
					t.Errorf("v1BaseURL(%q) = nil error, want error", tt.rawBase)
					return
				}
				if tt.errSub != "" && !strings.Contains(err.Error(), tt.errSub) {
					t.Errorf("v1BaseURL(%q) error = %q, want substring %q", tt.rawBase, err.Error(), tt.errSub)
				}
				for _, bad := range tt.errNotSubs {
					if strings.Contains(err.Error(), bad) {
						t.Errorf("v1BaseURL error leaks credential %q in: %q", bad, err.Error())
					}
				}
				return
			}
			if err != nil {
				t.Errorf("v1BaseURL(%q) = unexpected error: %v", tt.rawBase, err)
			}
		})
	}
}

// TestV1BaseURL_ParseError verifies that a URL with a control character (which
// causes url.Parse to fail) does not expose any caller-supplied content in the
// error string. This guards the parse-error path against credential leaks.
func TestV1BaseURL_ParseError(t *testing.T) {
	t.Parallel()

	// A URL containing a raw control character causes url.Parse to return an
	// error. The raw input must not appear verbatim in the returned error.
	//nolint:gosec // G101: test fixture, not real credentials
	rawBase := "https://user:s3cr3t@host\x00/"
	_, err := v1BaseURL(rawBase)
	if err == nil {
		t.Fatal("v1BaseURL: expected error for URL with control character, got nil")
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("v1BaseURL parse-error leaks credential in: %q", err.Error())
	}
	// The error must still be recognisably a gateway config error.
	if !strings.Contains(err.Error(), "gateway:") {
		t.Errorf("v1BaseURL parse-error missing gateway: prefix, got: %q", err.Error())
	}
}

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
