package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// chatCompletionsPath is the relative path appended to the resolved base URL
// to reach the OpenAI-compatible chat completions endpoint.
const chatCompletionsPath = "chat/completions"

// newHTTPClient constructs the shared *http.Client for the Gateway.
//
// No wall-clock Timeout is set: streaming chat responses are legitimately
// long-lived and cancellation is handled via context. QOV-63 will layer
// pre-first-token and idle timeout policies on top via the dial seam.
func newHTTPClient() *http.Client {
	return &http.Client{}
}

// chatEndpointURL joins a potentially messy operator-supplied base URL with
// the chat completions path.
//
// Operators commonly paste the base URL in several forms — with or without a
// trailing slash, and with or without a "/v1" suffix. All four variants must
// resolve to the same final URL:
//
//	https://h/             → https://h/v1/chat/completions
//	https://h              → https://h/v1/chat/completions
//	https://h/v1           → https://h/v1/chat/completions
//	https://h/v1/          → https://h/v1/chat/completions
//
// Algorithm:
//  1. Parse the raw base URL via net/url.
//  2. Strip a trailing slash from the path.
//  3. If the path does NOT already end with "/v1", append "/v1".
//  4. Append "/" + chatCompletionsPath.
func chatEndpointURL(rawBase string) (string, error) {
	u, err := url.Parse(rawBase)
	if err != nil {
		return "", fmt.Errorf("gateway: parse base URL %q: %w", rawBase, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("gateway: base URL %q missing scheme or host", rawBase)
	}

	// Normalise the path: strip trailing slash.
	p := strings.TrimSuffix(u.Path, "/")

	// Ensure the path ends with /v1.
	if !strings.HasSuffix(p, "/v1") {
		p += "/v1"
	}

	u.Path = p + "/" + chatCompletionsPath
	return u.String(), nil
}

// postJSON sends a POST request to the given URL with a JSON body. It sets the
// Authorization, Content-Type, and Accept headers required by
// OpenAI-compatible streaming endpoints, and passes ctx so the caller can
// cancel mid-flight.
//
// On success the caller owns the response body and must close it.
// On a dial/transport error the function returns a wrapped [ErrUpstream].
func (g *Gateway) postJSON(ctx context.Context, endpointURL, apiKey string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %w", ErrUpstream, err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUpstream, err)
	}
	return resp, nil
}

// drainClose reads the body to completion and then closes it, discarding all
// content. This ensures the underlying TCP connection is returned to the pool.
func drainClose(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()
}
