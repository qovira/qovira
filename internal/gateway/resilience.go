package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"math/rand/v2"
	"net/http"
	"time"
)

// ── Resilience configuration ──────────────────────────────────────────────────

// ResilienceConfig holds the timeout and retry knobs for [Gateway.Chat].
// All fields are optional; the zero value uses the package defaults set by
// [defaultResilienceConfig].  Construct a complete value via
// [defaultResilienceConfig] and override only the fields you want to change.
//
// These are intentionally public so operators and tests can tune them without
// rebuilding the gateway.
type ResilienceConfig struct {
	// FirstTokenTimeout is the maximum time to wait from the moment the HTTP
	// response body opens until the first [Chunk] arrives. Stalls longer than
	// this trip [ErrTimeout].
	//
	// Default: 45s. Raise for local models with slow time-to-first-token.
	FirstTokenTimeout time.Duration

	// IdleTimeout is the maximum time allowed between any two consecutive
	// [Chunk] values once the stream has started. A slow-but-progressing stream
	// (each chunk arrives within IdleTimeout) is never aborted; only a truly
	// stalled stream trips [ErrTimeout].
	//
	// Default: 30s.
	IdleTimeout time.Duration

	// MaxAttempts is the maximum number of dial attempts (initial + retries)
	// before the gateway gives up and returns an error. Must be ≥ 1.
	//
	// Default: 3.
	MaxAttempts int

	// BaseBackoff is the initial exponential-backoff delay (before jitter) for
	// transient failures. The delay before the retry following 0-indexed attempt
	// i (i = 0 is the first attempt, so its retry uses i = 0) is:
	//
	//   delay = BaseBackoff * 2^i + rand(0, BaseBackoff)
	//
	// Default: 500ms.
	BaseBackoff time.Duration

	// sleepFn is the function used to pause between retry attempts. It defaults
	// to time.Sleep and can be replaced in tests for determinism. It is
	// unexported so callers cannot set it accidentally.
	sleepFn func(time.Duration)
}

// defaultResilienceConfig returns a [ResilienceConfig] populated with the
// package defaults. It is called once in [New] and stored on the gateway.
func defaultResilienceConfig() ResilienceConfig {
	return ResilienceConfig{
		FirstTokenTimeout: 45 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxAttempts:       3,
		BaseBackoff:       500 * time.Millisecond,
		sleepFn:           time.Sleep,
	}
}

// ── Dial unit ─────────────────────────────────────────────────────────────────

// dialResult is a successful 2xx dial: a ready response body plus the
// cancellation function for the per-attempt context. The caller must call
// cancel() when it is done with body, even if it only calls it in a defer.
type dialResult struct {
	body   io.ReadCloser
	cancel context.CancelFunc
}

// dialChat is the single retryable unit: resolve → build wire body → POST →
// classify non-2xx. On a 2xx response it returns a dialResult whose body must
// be closed (and whose cancel must be called) by the caller.
//
// dialChat derives its own child context from ctx so that each attempt can be
// independently cancelled; ctx cancellation always propagates.
func (g *Gateway) dialChat(ctx context.Context, req ChatRequest) (dialResult, error) {
	// Derive a cancellable child context so per-attempt timeouts can be managed
	// by the resilience layer without affecting the parent.
	attemptCtx, cancel := context.WithCancel(ctx)

	resolved, err := g.resolve(attemptCtx, RoleChat)
	if err != nil {
		cancel()
		return dialResult{}, err
	}

	endpointURL, err := chatEndpointURL(resolved.BaseURL)
	if err != nil {
		cancel()
		return dialResult{}, err
	}

	body, err := buildWireRequest(req, resolved.Model)
	if err != nil {
		cancel()
		return dialResult{}, err
	}

	resp, err := g.postJSON(attemptCtx, endpointURL, resolved.APIKey, body) //nolint:bodyclose // Non-2xx path closes via drainClose below; 2xx body is owned by dialResult and closed by streamWithTimeouts.
	if err != nil {
		cancel()
		return dialResult{}, err
	}

	// Non-2xx: classify and close immediately.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		limitedBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		drainClose(resp.Body)
		cancel()

		// 451 Unavailable For Legal Reasons is handled by the resilience policy
		// (retryLegalUnavailable opt-in) so it needs a distinguishable error type.
		// We wrap it here before ClassifyResponse can fold it into generic ErrUpstream.
		if resp.StatusCode == http.StatusUnavailableForLegalReasons {
			return dialResult{}, &legalUnavailableError{}
		}

		classified := ClassifyResponse(resp.StatusCode, resp.Header, limitedBody)

		// 401/403 and 429 are handled directly by ClassifyResponse (ErrAuth and
		// *RateLimitedError respectively) and must reach isRetryable as-is.
		// All other 4xx responses are non-retryable: wrap them so isRetryable
		// can distinguish them from retryable 5xx/network ErrUpstream.
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			// ErrAuth — non-retryable but already typed; surface directly.
			return dialResult{}, classified
		case http.StatusTooManyRequests:
			// *RateLimitedError — retryable; surface directly.
			return dialResult{}, classified
		default:
			if resp.StatusCode >= 400 && resp.StatusCode < 500 {
				return dialResult{}, &nonRetryable4xxError{cause: classified}
			}
		}

		return dialResult{}, classified
	}

	return dialResult{body: resp.Body, cancel: cancel}, nil
}

// ── Retry policy ─────────────────────────────────────────────────────────────

// isRetryable reports whether err warrants a retry attempt, taking into account
// the 451 opt-in flag.
func isRetryable(err error, retryLegal bool) bool {
	if err == nil {
		return false
	}
	// Non-retryable 4xx wrapper: the upstream semantically rejected the request.
	var nr *nonRetryable4xxError
	if errors.As(err, &nr) {
		return false
	}
	// 451 Unavailable For Legal Reasons: retryable only when opted in.
	if isLegalUnavailable(err) {
		return retryLegal
	}
	// ErrContextLength is never retried — the payload won't get smaller.
	if errors.Is(err, ErrContextLength) {
		return false
	}
	// Auth and model-not-found are permanent 4xx errors.
	if errors.Is(err, ErrAuth) || errors.Is(err, ErrModelNotFound) {
		return false
	}
	// 429: always retryable (within budget).
	if errors.Is(err, ErrRateLimited) {
		return true
	}
	// 5xx and network failures (bare ErrUpstream, not wrapped by nonRetryable4xxError).
	if errors.Is(err, ErrUpstream) {
		return true
	}
	return false
}

// isLegalUnavailable reports whether err is a 451 legal-unavailable error.
// ClassifyResponse maps 451 through classify4xx → ErrUpstream with no
// distinguishing wrapper, so we can't distinguish 451-specific ErrUpstream
// from generic 5xx ErrUpstream via errors.Is alone.
//
// Instead we use a dedicated sentinel type that wraps ErrUpstream so the
// resilience layer can inspect it with errors.As.
func isLegalUnavailable(err error) bool {
	var le *legalUnavailableError
	return errors.As(err, &le)
}

// legalUnavailableError is a private sentinel returned by dialChat when the
// upstream responds with 451. It wraps ErrUpstream so that callers who don't
// know about 451 still see an upstream error, and errors.As allows the
// resilience layer to apply the retryLegalUnavailable policy.
type legalUnavailableError struct{}

func (e *legalUnavailableError) Error() string {
	return "gateway: upstream error (451 legal unavailable)"
}
func (e *legalUnavailableError) Unwrap() error { return ErrUpstream }

// nonRetryable4xxError wraps the classified error for non-retryable 4xx
// responses (other than 401, 403, 429, 451, and context-length). It unwraps to
// the underlying classified error so callers see the right sentinel, but the
// resilience layer can recognise it as non-retryable via errors.As.
type nonRetryable4xxError struct{ cause error }

func (e *nonRetryable4xxError) Error() string { return e.cause.Error() }
func (e *nonRetryable4xxError) Unwrap() error { return e.cause }

// retryBackoff computes the delay before attempt i (0-indexed, so attempt 1
// means the first retry). The formula is:
//
//	delay = base * 2^i + rand(0, base)
//
// The jitter is seeded from the global math/rand/v2 source (non-cryptographic;
// only used for backoff). In tests, the sleepFn is replaced so the actual delay
// value doesn't matter for correctness.
func retryBackoff(base time.Duration, attempt int) time.Duration {
	exp := base
	for range attempt {
		exp *= 2
	}
	var jitter time.Duration
	if base > 0 {
		//nolint:gosec // G404: non-crypto rand is fine for retry jitter
		jitter = time.Duration(rand.Int64N(int64(base)))
	}
	return exp + jitter
}

// ── chatWithResilience: Chat's resilience wrapper ────────────────────────────

// chatWithResilience is the resilience-wrapped implementation called by
// [Gateway.Chat]. It owns the retry loop (pre-first-token) and the
// timeout-wrapped streaming iterator.
//
// chatWithResilience modifies Chat's behaviour as follows:
//  1. Dial attempts are retried on transient errors (network, 5xx, 429) up to
//     cfg.MaxAttempts times, with jittered backoff. 429 Retry-After is
//     honoured within the overall budget.
//  2. Once a 2xx body is obtained, the streaming iterator wraps it with:
//     a. A first-token timer: if no chunk arrives within cfg.FirstTokenTimeout,
//     the stream is cancelled and ErrTimeout surfaces as the per-yield error.
//     b. An idle timer: reset on every chunk; trips on stalled streams.
//  3. ctx cancellation propagates promptly via the derived attempt context.
func (g *Gateway) chatWithResilience(
	ctx context.Context,
	req ChatRequest,
	cfg ResilienceConfig,
) (iter.Seq2[Chunk, error], error) {
	// Read the 451 opt-in once before the retry loop (it's a config read; cheap
	// and idempotent, and we don't want it racing on every attempt).
	retryLegal, err := g.retryLegalUnavailable(ctx)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 0; attempt < cfg.MaxAttempts; attempt++ {
		// Check context before each attempt.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %w", ErrTimeout, ctx.Err())
		}

		dr, dialErr := g.dialChat(ctx, req)
		if dialErr == nil {
			// 2xx: wrap the body in the timeout-aware streaming iterator.
			seq := g.streamWithTimeouts(ctx, dr, cfg)
			return seq, nil
		}

		lastErr = dialErr

		// Non-retryable: surface immediately.
		if !isRetryable(dialErr, retryLegal) {
			// Unwrap internal wrapper types so callers see the typed sentinels
			// (ErrUpstream, ErrContextLength, ErrModelNotFound, ErrAuth) rather
			// than unexported wrapper types they can't introspect.
			var nr *nonRetryable4xxError
			if errors.As(dialErr, &nr) {
				return nil, nr.cause
			}
			var le *legalUnavailableError
			if errors.As(dialErr, &le) {
				return nil, ErrUpstream // 451 with retryLegal=false → ErrUpstream
			}
			return nil, dialErr
		}

		// Last attempt: no more retries.
		if attempt == cfg.MaxAttempts-1 {
			break
		}

		// Compute backoff. For 429, honour Retry-After within a fixed cap so a
		// misconfigured or hostile header can't stall the gateway indefinitely.
		const retryAfterCap = 60 * time.Second
		delay := retryBackoff(cfg.BaseBackoff, attempt)
		var rle *RateLimitedError
		if errors.As(dialErr, &rle) && rle.RetryAfter != nil {
			delay = min(*rle.RetryAfter, retryAfterCap)
		}

		cfg.sleepFn(delay)
	}

	// All attempts exhausted. Unwrap internal wrappers and surface the
	// underlying typed sentinel (ErrRateLimited, ErrUpstream, …) to the caller.
	var le *legalUnavailableError
	if errors.As(lastErr, &le) {
		return nil, ErrUpstream
	}
	return nil, lastErr
}

// ── Timeout-aware streaming iterator ─────────────────────────────────────────

// chunkOrErr is the type sent on the internal producer channel.
type chunkOrErr struct {
	chunk Chunk
	err   error
	done  bool // true when the producer exited cleanly (no more items)
}

// streamWithTimeouts wraps a dialResult's body in an iter.Seq2[Chunk, error]
// that enforces first-token and idle timeouts.
//
// Architecture: a producer goroutine calls streamSSE and forwards chunks (and
// the terminal error) onto a buffered channel. The consumer (the iter.Seq2
// body) selects over the channel, the active timer, and ctx.Done.
//
// Goroutine lifetime guarantee: the producer always terminates because:
//   - On consumer break/stop: cancelAttempt() cancels the attempt context,
//     which aborts the HTTP body read, causing streamSSE to return; the
//     producer then sends a sentinel and exits.
//   - On timeout: same cancellation path.
//   - On ctx cancellation: same path via the derived attempt context.
//
// The response body is always drained and closed via defer inside the producer;
// cancelAttempt is called in every exit path of the consumer.
func (g *Gateway) streamWithTimeouts(
	ctx context.Context,
	dr dialResult,
	cfg ResilienceConfig,
) iter.Seq2[Chunk, error] {
	return func(yield func(Chunk, error) bool) {
		// streamCtx is cancelled when the consumer returns for ANY reason (break,
		// error, timeout, parent ctx cancel). It is what unblocks the producer
		// goroutine when it is parked on a channel send with no consumer left —
		// dr.cancel() only aborts the HTTP body *read*, so a producer blocked on
		// the send (not the read) needs this separate lever. streamCtx derives
		// from ctx, so a parent-ctx cancellation also propagates to it.
		streamCtx, cancelStream := context.WithCancel(ctx)
		defer cancelStream()

		// Also cancel the per-attempt context so the in-flight HTTP request/body
		// read is aborted (and the connection released) on every consumer exit.
		defer dr.cancel()

		// The producer sends to this buffered channel. The buffer of 1 lets the
		// producer deposit one item and return to scanning without blocking, which
		// avoids a lock-step synchronisation that would degrade throughput.
		ch := make(chan chunkOrErr, 1)

		go func() {
			defer drainClose(dr.body)

			streamErr := streamSSE(dr.body, func(c Chunk) bool {
				select {
				case ch <- chunkOrErr{chunk: c}:
					return true
				case <-streamCtx.Done():
					return false
				}
			})
			// Signal terminal state to the consumer. If the consumer has already
			// gone, streamCtx is cancelled; the select guarantees the goroutine
			// never wedges on this send.
			select {
			case ch <- chunkOrErr{err: streamErr, done: true}:
			case <-streamCtx.Done():
			}
		}()

		// The first-token timer fires if no chunk arrives within FirstTokenTimeout.
		// Once we receive the first chunk we switch to the idle timer.
		firstToken := time.NewTimer(cfg.FirstTokenTimeout)
		defer firstToken.Stop()

		// Idle timer is started (replacing firstToken) after the first chunk.
		var idleTimer *time.Timer
		defer func() {
			if idleTimer != nil {
				idleTimer.Stop()
			}
		}()

		activeTimer := firstToken.C // starts as the first-token timer

		for {
			select {
			case <-ctx.Done():
				// Parent context cancelled.
				yield(Chunk{}, fmt.Errorf("%w: %w", ErrTimeout, ctx.Err()))
				return

			case <-activeTimer:
				// First-token or idle timeout.
				yield(Chunk{}, ErrTimeout)
				return

			case item := <-ch:
				if item.done {
					// Producer finished. item.err is nil on clean completion or a
					// parse error from streamSSE.
					if item.err != nil {
						yield(Chunk{}, fmt.Errorf("%w: %w", ErrUpstreamProtocol, item.err))
					}
					// Normal termination: iterator simply returns (no final yield —
					// the Done chunk was already forwarded above as a regular chunk).
					return
				}

				// We have a real chunk. Switch from first-token timer to idle timer.
				if idleTimer == nil {
					// First chunk received: stop first-token timer, start idle timer.
					firstToken.Stop()
					idleTimer = time.NewTimer(cfg.IdleTimeout)
					activeTimer = idleTimer.C
				} else {
					// Subsequent chunks: reset the idle timer.
					if !idleTimer.Stop() {
						// Drain the channel if the timer already fired but we're only
						// now processing the chunk (rare race window).
						select {
						case <-idleTimer.C:
						default:
						}
					}
					idleTimer.Reset(cfg.IdleTimeout)
				}

				if !yield(item.chunk, nil) {
					// Consumer stopped (break).
					return
				}
			}
		}
	}
}
