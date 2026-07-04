package sponsr

import (
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/time/rate"
)

// retryBaseDelay is the default exponential-backoff base used when a 429
// response carries no usable Retry-After header.
const retryBaseDelay = time.Second

// retryMaxDelay caps a single backoff wait. It bounds both very large
// Retry-After values and the exponential growth, which would otherwise
// overflow time.Duration at high retry counts and silently defeat backoff.
const retryMaxDelay = 2 * time.Minute

// rateLimitRetryTransport is an http.RoundTripper that spaces outgoing
// requests with a shared rate limiter and retries HTTP 429 with backoff that
// honors Retry-After. It wraps a base transport; a nil base uses
// http.DefaultTransport. One instance is shared across all client goroutines,
// so the limiter enforces global spacing.
type rateLimitRetryTransport struct {
	base           http.RoundTripper
	limiter        *rate.Limiter // nil disables client-side spacing
	maxRetries     int
	retryBaseDelay time.Duration
}

func newRateLimitRetryTransport(
	base http.RoundTripper,
	limiter *rate.Limiter,
	maxRetries int,
	retryBaseDelay time.Duration,
) *rateLimitRetryTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &rateLimitRetryTransport{
		base:           base,
		limiter:        limiter,
		maxRetries:     maxRetries,
		retryBaseDelay: retryBaseDelay,
	}
}

// RoundTrip applies rate limiting and retries 429 responses with backoff. On
// exhausted retries it returns the final 429 response (body intact) so the
// caller can surface it. The request body is rewound via GetBody on each
// replay, since callers may attach a body even to idempotent GETs.
func (t *rateLimitRetryTransport) RoundTrip(
	req *http.Request,
) (*http.Response, error) {
	ctx := req.Context()
	for attempt := 0; ; attempt++ {
		if attempt > 0 && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			req.Body = body
		}

		if t.limiter != nil {
			if err := t.limiter.Wait(ctx); err != nil {
				return nil, err
			}
		}

		resp, err := t.base.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}
		if attempt >= t.maxRetries {
			slog.Warn(
				"sponsr rate limit retries exhausted, giving up",
				"url", req.URL.String(),
				"attempts", attempt+1,
			)
			return resp, nil
		}

		wait := t.backoff(resp.Header, attempt)
		slog.Warn(
			"rate limited by sponsr, backing off",
			"url", req.URL.String(),
			"attempt", attempt+1,
			"wait", wait,
		)
		// Drain and close the 429 body so the connection can be reused.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// backoff returns how long to wait before retrying. It honors Retry-After when
// it specifies a usable delay (non-negative delta-seconds or a future
// HTTP-date), otherwise it falls back to exponential backoff based on
// retryBaseDelay. A non-empty but unparseable Retry-After is logged and treated
// as absent. Every result is capped at retryMaxDelay.
func (t *rateLimitRetryTransport) backoff(
	h http.Header,
	attempt int,
) time.Duration {
	if v := h.Get("Retry-After"); v != "" {
		if d, ok := parseRetryAfter(v); ok {
			return min(d, retryMaxDelay)
		}
		slog.Warn(
			"unparseable Retry-After header, using exponential backoff",
			"retry_after", v,
		)
	}

	// Double the base per attempt, stopping as soon as the cap is reached so
	// the multiplication can never overflow time.Duration.
	delay := t.retryBaseDelay
	for i := 0; i < attempt && delay < retryMaxDelay; i++ {
		delay *= 2
	}
	return min(delay, retryMaxDelay)
}

// parseRetryAfter interprets a Retry-After header value in either the
// delta-seconds or HTTP-date form. It reports ok=false when the value is
// malformed, negative, or refers to a time already in the past.
func parseRetryAfter(v string) (time.Duration, bool) {
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d, true
		}
	}
	return 0, false
}
