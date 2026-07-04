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

	"github.com/acidsailor/restkit"
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
		"/posts?project_id=1",
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
		"/posts?project_id=1",
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
		"/posts?project_id=1",
		1,
		20,
	)
	require.Error(t, err)
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

	client := newTestClientTransport(srv, newRateLimitRetryTransport(
		srv.Client().Transport, nil, 2, time.Millisecond,
	))

	_, err := GetObjects[Post](
		client,
		context.Background(),
		"/posts?project_id=1",
		1,
		20,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSponsrClient)
	assert.Contains(t, err.Error(), "429")
	// 1 initial attempt + 2 retries.
	assert.Equal(t, int32(3), calls.Load())
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
	client := newTestClientTransport(srv, newRateLimitRetryTransport(
		srv.Client().Transport,
		rate.NewLimiter(rate.Every(delay), 1),
		0,
		time.Millisecond,
	))

	_, err := GetObjectsAll[Post](
		client,
		context.Background(),
		"/posts?project_id=1",
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

func TestNewClient_RateLimiter(t *testing.T) {
	c, err := NewClient("t", time.Second, 4, 20, 250*time.Millisecond, 5)
	require.NoError(t, err)
	tr := c.httpClient.Transport.(*rateLimitRetryTransport)
	assert.NotNil(t, tr.limiter)
	assert.Equal(t, 5, tr.maxRetries)

	// Zero delay disables client-side rate limiting.
	c, err = NewClient("t", time.Second, 4, 20, 0, 0)
	require.NoError(t, err)
	tr = c.httpClient.Transport.(*rateLimitRetryTransport)
	assert.Nil(t, tr.limiter)

	_, err = NewClient("t", time.Second, 4, 20, -1, 0)
	require.Error(t, err)
	_, err = NewClient("t", time.Second, 4, 20, 0, -1)
	require.Error(t, err)
}

func newTestClient(srv *httptest.Server) *Client {
	return newTestClientTransport(srv, newRateLimitRetryTransport(
		srv.Client().Transport, nil, 0, time.Millisecond,
	))
}

func newTestClientTransport(
	srv *httptest.Server, tr *rateLimitRetryTransport,
) *Client {
	hc := &http.Client{Transport: tr}
	rk, err := restkit.New(
		srv.URL,
		restkit.WithName("sponsr"),
		restkit.WithHTTPClient(hc),
		restkit.WithHook(bearerAuthHook("test-token")),
	)
	if err != nil {
		panic(err)
	}
	return &Client{
		rk:               rk,
		httpClient:       hc,
		concurrencyLimit: 4,
		paginatorLimit:   20,
	}
}

func TestGetObjects_SendsAuthAndMergesQuery(t *testing.T) {
	var gotAuth, gotPage, gotLimit, gotProject string
	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			gotPage = r.URL.Query().Get("page")
			gotLimit = r.URL.Query().Get("limit")
			gotProject = r.URL.Query().Get("project_id")
			_ = json.NewEncoder(w).
				Encode(Objects[Post]{Total: 0, List: nil, Page: 1, Limit: 20})
		}),
	)
	defer srv.Close()

	_, err := GetObjects[Post](
		newTestClient(srv),
		context.Background(),
		"/posts?project_id=7",
		3,
		20,
	)
	require.NoError(t, err)
	assert.Equal(t, "Bearer test-token", gotAuth)
	assert.Equal(t, "3", gotPage)
	assert.Equal(t, "20", gotLimit)
	assert.Equal(t, "7", gotProject)
}
