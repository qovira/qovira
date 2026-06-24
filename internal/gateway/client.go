package gateway

import (
	"bytes"
	"context"
	"errors"
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
// an error when either is absent. It rejects any scheme other than http/https
// and any URL carrying embedded userinfo (credentials), without echoing the
// raw URL or any userinfo into the returned error string.
func v1BaseURL(rawBase string) (*url.URL, error) {
	u, err := url.Parse(rawBase)
	if err != nil {
		// Do not include rawBase or the url.Error (which re-embeds the raw URL)
		// in the message: a caller-supplied URL may contain embedded credentials
		// that must not reach error strings or logs. Extract only the Op/Err
		// fields that are safe to expose.
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, fmt.Errorf("gateway: base URL could not be parsed: %w", ue.Err)
		}
		return nil, fmt.Errorf("gateway: base URL could not be parsed")
	}

	// Reject userinfo FIRST, before any error that would echo the raw URL or
	// the scheme. Credentials embedded in URLs must never reach error strings.
	if u.User != nil {
		return nil, fmt.Errorf("gateway: base URL must not contain userinfo (credentials in URLs are not allowed)")
	}

	if u.Scheme == "" || u.Host == "" {
		// Don't echo rawBase: a scheme-confused input (e.g. "user:pass@host"
		// parses with no userinfo and an empty host) could otherwise leak
		// credentials into the error string.
		return nil, fmt.Errorf("gateway: base URL must include a scheme and host")
	}

	// Reject any scheme other than http/https. url.Parse lower-cases the
	// scheme, so a direct comparison suffices. The scheme itself is safe to
	// include in the error — it cannot carry a password.
	switch u.Scheme {
	case "http", "https":
		// accepted
	default:
		return nil, fmt.Errorf("gateway: base URL has unsupported scheme %q (only http/https allowed)", u.Scheme)
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

// errBodyLimit is the maximum number of bytes read from a non-2xx response
// body when constructing an error. It matches the cap used in dialChatResolved
// so that probe and chat paths cannot drift independently.
const errBodyLimit = 8 << 10 // 8 KiB

// readErrBody reads up to errBodyLimit bytes from rc and returns them.
// It does not close rc; the caller is responsible for closing the body.
func readErrBody(rc io.ReadCloser) []byte {
	b, _ := io.ReadAll(io.LimitReader(rc, errBodyLimit))
	return b
}
