package sponsr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

func TestCalculatePages(t *testing.T) {
	tests := []struct {
		total, limit, want int
	}{
		{1, 10, 1},
		{10, 10, 1},
		{11, 10, 2},
		{20, 10, 2},
		{21, 10, 3},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, CalculatePages(tt.total, tt.limit))
	}
}

func TestPaginatedURL(t *testing.T) {
	got := PaginatedURL("https://example.com/api?foo=bar", 2, 20)
	assert.Contains(t, got, "page=2")
	assert.Contains(t, got, "limit=20")
	assert.Contains(t, got, "foo=bar")
}

func TestProjectIDBySlug(t *testing.T) {
	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprint(
				w,
				`<html><script>{"project_id": 42}</script></html>`,
			)
		}),
	)
	defer srv.Close()

	id, err := newTestClient(
		srv,
	).projectIDBySlugURL(context.Background(), srv.URL+"/")
	require.NoError(t, err)
	assert.Equal(t, 42, id)
}

func TestProjectIDBySlug_NotFound(t *testing.T) {
	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprint(w, `<html>no project here</html>`)
		}),
	)
	defer srv.Close()

	_, err := newTestClient(
		srv,
	).projectIDBySlugURL(context.Background(), srv.URL+"/")
	require.Error(t, err)
}

func TestGetObjects(t *testing.T) {
	posts := []Post{
		{ID: 1, Title: "Post One", Available: true, Date: time.Now()},
		{ID: 2, Title: "Post Two", Available: false, Date: time.Now()},
	}
	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).
				Encode(Objects[Post]{Total: 2, List: posts, Page: 1, Limit: 20})
		}),
	)
	defer srv.Close()

	got, err := GetObjects[Post](
		newTestClient(srv),
		context.Background(),
		srv.URL+"/posts?project_id=1",
		1,
		20,
	)
	require.NoError(t, err)
	require.Len(t, got.List, 2)
	assert.Equal(t, "Post One", got.List[0].Title)
}

func TestGetObjectsAll_Pagination(t *testing.T) {
	const total = 5
	const limit = 2

	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			page := 1
			fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page) //nolint:errcheck
			lim := limit
			fmt.Sscanf(r.URL.Query().Get("limit"), "%d", &lim) //nolint:errcheck

			start := (page - 1) * lim
			end := min(start+lim, total)

			var list []Post
			for i := start; i < end; i++ {
				list = append(
					list,
					Post{
						ID:        i + 1,
						Title:     fmt.Sprintf("Post %d", i+1),
						Available: true,
					},
				)
			}
			_ = json.NewEncoder(w).
				Encode(Objects[Post]{Total: total, List: list, Page: page, Limit: lim})
		}),
	)
	defer srv.Close()

	client := newTestClient(srv)
	client.paginatorLimit = limit

	got, err := GetObjectsAll[Post](
		client,
		context.Background(),
		srv.URL+"/posts?project_id=1",
	)
	require.NoError(t, err)
	assert.Len(t, got, total)
}

func TestGetObjects_HTTPError(t *testing.T) {
	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}),
	)
	defer srv.Close()

	_, err := GetObjects[Post](
		newTestClient(srv),
		context.Background(),
		srv.URL+"/posts?project_id=1",
		1,
		20,
	)
	require.Error(t, err)
}

func TestGetObjects_RetriesOn429(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Fail the first two attempts with 429, then succeed.
			if calls.Add(1) <= 2 {
				w.Header().Set("Retry-After", "0")
				http.Error(w, "throttled", http.StatusTooManyRequests)
				return
			}
			_ = json.NewEncoder(w).
				Encode(Objects[Post]{Total: 1, List: []Post{{ID: 1}}, Page: 1, Limit: 20})
		}),
	)
	defer srv.Close()

	client := newTestClient(srv)
	client.maxRetries = 3

	got, err := GetObjects[Post](
		client,
		context.Background(),
		srv.URL+"/posts?project_id=1",
		1,
		20,
	)
	require.NoError(t, err)
	require.Len(t, got.List, 1)
	assert.Equal(t, int32(3), calls.Load())
}

func TestGetObjects_429ExhaustsRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			http.Error(w, "throttled", http.StatusTooManyRequests)
		}),
	)
	defer srv.Close()

	client := newTestClient(srv)
	client.maxRetries = 2

	_, err := GetObjects[Post](
		client,
		context.Background(),
		srv.URL+"/posts?project_id=1",
		1,
		20,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "429")
	// 1 initial attempt + 2 retries.
	assert.Equal(t, int32(3), calls.Load())
}

func TestBackoff_RetryAfterSeconds(t *testing.T) {
	c := &Client{retryBaseDelay: time.Second}
	h := http.Header{}
	h.Set("Retry-After", "3")
	assert.Equal(t, 3*time.Second, c.backoff(h, 0))
}

func TestBackoff_RetryAfterHTTPDate(t *testing.T) {
	c := &Client{retryBaseDelay: time.Second}
	h := http.Header{}
	h.Set(
		"Retry-After",
		time.Now().Add(2*time.Second).UTC().Format(http.TimeFormat),
	)
	// http.TimeFormat has second granularity, so the remaining delay lands
	// between ~1s and 2s.
	d := c.backoff(h, 0)
	assert.Positive(t, d)
	assert.Greater(t, d, 500*time.Millisecond)
	assert.LessOrEqual(t, d, 2*time.Second)
}

func TestBackoff_RetryAfterFallbacks(t *testing.T) {
	c := &Client{retryBaseDelay: time.Second}
	// Malformed, negative, and unparseable values fall back to exponential
	// backoff (retryBaseDelay at attempt 0).
	for _, v := range []string{"abc", "-5", "-1", "not-a-date"} {
		h := http.Header{}
		h.Set("Retry-After", v)
		assert.Equal(
			t,
			time.Second,
			c.backoff(h, 0),
			"Retry-After %q should fall back to exponential backoff",
			v,
		)
	}
	// Retry-After: 0 is honored as "retry immediately". The client-side
	// limiter still paces the next attempt.
	h := http.Header{}
	h.Set("Retry-After", "0")
	assert.Equal(t, time.Duration(0), c.backoff(h, 0))
}

func TestBackoff_Exponential(t *testing.T) {
	c := &Client{retryBaseDelay: time.Second}
	assert.Equal(t, time.Second, c.backoff(http.Header{}, 0))
	assert.Equal(t, 2*time.Second, c.backoff(http.Header{}, 1))
	assert.Equal(t, 4*time.Second, c.backoff(http.Header{}, 2))
}

func TestBackoff_CapsLargeDelays(t *testing.T) {
	c := &Client{retryBaseDelay: time.Second}

	// A very large Retry-After is clamped to retryMaxDelay.
	h := http.Header{}
	h.Set("Retry-After", "100000")
	assert.Equal(t, retryMaxDelay, c.backoff(h, 0))

	// Exponential growth is capped and never overflows to a non-positive
	// duration, regardless of how many retries are configured.
	for attempt := range 200 {
		d := c.backoff(http.Header{}, attempt)
		assert.Positive(t, d, "attempt %d must not overflow", attempt)
		assert.LessOrEqual(t, d, retryMaxDelay)
	}
}

// TestGetObjectsAll_RateLimiterSpacesRequests exercises the actual limiter Wait
// path across the concurrent paginator goroutines, not just its construction.
func TestGetObjectsAll_RateLimiterSpacesRequests(t *testing.T) {
	var mu sync.Mutex
	var stamps []time.Time
	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			stamps = append(stamps, time.Now())
			mu.Unlock()
			// Total 100 with limit 20 => 5 pages => 5 requests.
			_ = json.NewEncoder(w).
				Encode(Objects[Post]{Total: 100, List: []Post{{ID: 1}}, Page: 1, Limit: 20})
		}),
	)
	defer srv.Close()

	const delay = 25 * time.Millisecond
	client := newTestClient(srv)
	client.limiter = rate.NewLimiter(rate.Every(delay), 1)

	_, err := GetObjectsAll[Post](
		client,
		context.Background(),
		srv.URL+"/posts?project_id=1",
	)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(stamps), 5)
	sort.Slice(
		stamps,
		func(i, j int) bool { return stamps[i].Before(stamps[j]) },
	)
	elapsed := stamps[len(stamps)-1].Sub(stamps[0])
	// Allow slack for scheduling jitter while still proving requests are paced.
	minExpected := time.Duration(len(stamps)-1) * delay * 8 / 10
	assert.GreaterOrEqual(
		t,
		elapsed,
		minExpected,
		"limiter should space requests by ~%s each",
		delay,
	)
}

// TestDoRequest_ContextCanceledDuringBackoff ensures a cancelled context aborts
// the retry sleep promptly instead of blocking for the full backoff.
func TestDoRequest_ContextCanceledDuringBackoff(t *testing.T) {
	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "throttled", http.StatusTooManyRequests)
		}),
	)
	defer srv.Close()

	client := newTestClient(srv)
	client.maxRetries = 100
	client.retryBaseDelay = time.Hour // long backoff we expect to interrupt

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := GetObjects[Post](
		client,
		ctx,
		srv.URL+"/posts?project_id=1",
		1,
		20,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(
		t,
		time.Since(start),
		time.Second,
		"cancellation should abort the backoff wait promptly",
	)
}

func TestNewClient_RateLimiter(t *testing.T) {
	c, err := NewClient("t", time.Second, 4, 20, 250*time.Millisecond, 5)
	require.NoError(t, err)
	assert.NotNil(t, c.limiter)
	assert.Equal(t, 5, c.maxRetries)

	// Zero delay disables client-side rate limiting.
	c, err = NewClient("t", time.Second, 4, 20, 0, 0)
	require.NoError(t, err)
	assert.Nil(t, c.limiter)

	_, err = NewClient("t", time.Second, 4, 20, -1, 0)
	require.Error(t, err)
	_, err = NewClient("t", time.Second, 4, 20, 0, -1)
	require.Error(t, err)
}

func newTestClient(srv *httptest.Server) *Client {
	return &Client{
		bearerToken:      "test-token",
		httpClient:       srv.Client(),
		concurrencyLimit: 4,
		paginatorLimit:   20,
		// Keep backoff tiny so retry tests stay fast.
		retryBaseDelay: time.Millisecond,
	}
}
