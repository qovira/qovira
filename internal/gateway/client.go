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

// modelsPath is the relative path appended to the resolved base URL to reach
// the OpenAI-compatible models list endpoint.
const modelsPath = "models"

// newHTTPClient constructs the shared *http.Client for the Gateway.
//
// No wall-clock Timeout is set: streaming chat responses are legitimately
// long-lived and cancellation is handled via context. The resilience layer
// ([chatWithResilience]) layers first-token and idle timeout policies on top
// via derived cancellable contexts.
func newHTTPClient() *http.Client {
	return &http.Client{}
}

// v1BaseURL normalises an operator-supplied base URL so that it ends with
// "/v1" (no trailing slash). The resulting string can be joined with a path
// segment by appending "/" + segment.
//
// Operators commonly paste the base URL in several forms — with or without a
// trailing slash, and with or without a "/v1" suffix. All four variants must
// produce the same normalised base:
//
//	https://h/     → https://h/v1
//	https://h      → https://h/v1
//	https://h/v1   → https://h/v1
//	https://h/v1/  → https://h/v1
//
// The function validates that the URL has both a scheme and a host, returning
// an error when either is absent.
func v1BaseURL(rawBase string) (*url.URL, error) {
	u, err := url.Parse(rawBase)
	if err != nil {
		return nil, fmt.Errorf("gateway: parse base URL %q: %w", rawBase, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("gateway: base URL %q missing scheme or host", rawBase)
	}

	// Normalise the path: strip trailing slash, then ensure /v1 suffix.
	p := strings.TrimSuffix(u.Path, "/")
	if !strings.HasSuffix(p, "/v1") {
		p += "/v1"
	}
	u.Path = p
	return u, nil
}

// joinEndpointURL joins a potentially messy operator-supplied base URL with the
// given path segment, inserting the "/v1" normalisation layer.
//
// Both chatEndpointURL and modelsEndpointURL delegate to this function so the
// normalisation logic is not duplicated.
func joinEndpointURL(rawBase, path string) (string, error) {
	u, err := v1BaseURL(rawBase)
	if err != nil {
		return "", err
	}
	u.Path = u.Path + "/" + path
	return u.String(), nil
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
func chatEndpointURL(rawBase string) (string, error) {
	return joinEndpointURL(rawBase, chatCompletionsPath)
}

// modelsEndpointURL joins a potentially messy operator-supplied base URL with
// the models list path, applying the same "/v1" normalisation as chatEndpointURL.
func modelsEndpointURL(rawBase string) (string, error) {
	return joinEndpointURL(rawBase, modelsPath)
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

// getJSON sends a GET request to the given URL with a Bearer authorization
// header. It is used for non-streaming, non-POST endpoints such as the models
// list. On success the caller owns the response body and must close it.
// On a dial/transport error the function returns a wrapped [ErrUpstream].
func (g *Gateway) getJSON(ctx context.Context, endpointURL, apiKey string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpointURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %w", ErrUpstream, err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

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
