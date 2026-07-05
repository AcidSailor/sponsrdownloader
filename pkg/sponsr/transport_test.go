package sponsr

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackoff_RetryAfterSeconds(t *testing.T) {
	tr := &rateLimitRetryTransport{retryBaseDelay: time.Second}
	h := http.Header{}
	h.Set("Retry-After", "3")
	assert.Equal(t, 3*time.Second, tr.backoff(h, 0))
}

func TestBackoff_RetryAfterHTTPDate(t *testing.T) {
	tr := &rateLimitRetryTransport{retryBaseDelay: time.Second}
	h := http.Header{}
	h.Set(
		"Retry-After",
		time.Now().Add(2*time.Second).UTC().Format(http.TimeFormat),
	)
	// http.TimeFormat has second granularity, so the remaining delay lands
	// between ~1s and 2s.
	d := tr.backoff(h, 0)
	assert.Positive(t, d)
	assert.Greater(t, d, 500*time.Millisecond)
	assert.LessOrEqual(t, d, 2*time.Second)
}

func TestBackoff_RetryAfterFallbacks(t *testing.T) {
	tr := &rateLimitRetryTransport{retryBaseDelay: time.Second}
	for _, v := range []string{"abc", "-5", "-1", "not-a-date"} {
		h := http.Header{}
		h.Set("Retry-After", v)
		assert.Equal(
			t,
			time.Second,
			tr.backoff(h, 0),
			"Retry-After %q should fall back to exponential backoff",
			v,
		)
	}
	h := http.Header{}
	h.Set("Retry-After", "0")
	assert.Equal(t, time.Duration(0), tr.backoff(h, 0))
}

func TestBackoff_Exponential(t *testing.T) {
	tr := &rateLimitRetryTransport{retryBaseDelay: time.Second}
	assert.Equal(t, time.Second, tr.backoff(http.Header{}, 0))
	assert.Equal(t, 2*time.Second, tr.backoff(http.Header{}, 1))
	assert.Equal(t, 4*time.Second, tr.backoff(http.Header{}, 2))
}

func TestBackoff_CapsLargeDelays(t *testing.T) {
	tr := &rateLimitRetryTransport{retryBaseDelay: time.Second}

	h := http.Header{}
	h.Set("Retry-After", "100000")
	assert.Equal(t, retryMaxDelay, tr.backoff(h, 0))

	for attempt := range 200 {
		d := tr.backoff(http.Header{}, attempt)
		assert.Positive(t, d, "attempt %d must not overflow", attempt)
		assert.LessOrEqual(t, d, retryMaxDelay)
	}
}

func TestTransport_RetriesOn429(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) <= 2 {
				w.Header().Set("Retry-After", "0")
				http.Error(w, "throttled", http.StatusTooManyRequests)
				return
			}
			w.WriteHeader(http.StatusOK)
		}),
	)
	defer srv.Close()

	tr := newRateLimitRetryTransport(
		srv.Client().Transport, nil, 3, time.Millisecond,
	)
	resp, err := (&http.Client{Transport: tr}).Get(srv.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(3), calls.Load())
}

func TestTransport_DoesNotRetryNon429(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			http.Error(w, "boom", http.StatusInternalServerError)
		}),
	)
	defer srv.Close()

	tr := newRateLimitRetryTransport(
		srv.Client().Transport, nil, 3, time.Millisecond,
	)
	resp, err := (&http.Client{Transport: tr}).Get(srv.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, int32(1), calls.Load(), "non-429 must not be retried")
}

func TestNewTransport_ClampsInvalidParams(t *testing.T) {
	tr := newRateLimitRetryTransport(nil, nil, -5, -time.Second)
	assert.NotNil(t, tr.base)
	assert.Equal(t, 0, tr.maxRetries)
	assert.Equal(t, defaultRetryBaseDelay, tr.retryBaseDelay)
}

func TestTransport_429ExhaustsRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			http.Error(w, "throttled", http.StatusTooManyRequests)
		}),
	)
	defer srv.Close()

	tr := newRateLimitRetryTransport(
		srv.Client().Transport, nil, 2, time.Millisecond,
	)
	resp, err := (&http.Client{Transport: tr}).Get(srv.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	// The final 429 is returned, not an error. 1 initial + 2 retries.
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	assert.Equal(t, int32(3), calls.Load())
}

// TestTransport_RewindsBodyOnRetry proves a replayed request re-sends its body
// (restkit attaches a JSON body even to GETs, so a naive replay would drop it).
func TestTransport_RewindsBodyOnRetry(t *testing.T) {
	var lastBody atomic.Value
	var calls atomic.Int32
	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			lastBody.Store(string(b))
			if calls.Add(1) <= 1 {
				w.Header().Set("Retry-After", "0")
				http.Error(w, "throttled", http.StatusTooManyRequests)
				return
			}
			w.WriteHeader(http.StatusOK)
		}),
	)
	defer srv.Close()

	tr := newRateLimitRetryTransport(
		srv.Client().Transport, nil, 3, time.Millisecond,
	)
	req, err := http.NewRequest(
		http.MethodGet, srv.URL, bytes.NewReader([]byte(`{"a":1}`)),
	)
	require.NoError(t, err)
	resp, err := tr.RoundTrip(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, `{"a":1}`, lastBody.Load())
}

// TestTransport_ContextCanceledDuringBackoff ensures a cancelled context aborts
// the retry sleep promptly instead of blocking for the full backoff.
func TestTransport_ContextCanceledDuringBackoff(t *testing.T) {
	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "throttled", http.StatusTooManyRequests)
		}),
	)
	defer srv.Close()

	tr := newRateLimitRetryTransport(
		srv.Client().Transport, nil, 100, time.Hour,
	)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, srv.URL, nil,
	)
	require.NoError(t, err)
	start := time.Now()
	_, err = tr.RoundTrip(req)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(
		t,
		time.Since(start),
		time.Second,
		"cancellation should abort the backoff wait promptly",
	)
}
