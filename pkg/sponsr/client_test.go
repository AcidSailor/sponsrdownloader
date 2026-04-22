package sponsr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func newTestClient(srv *httptest.Server) *Client {
	return &Client{
		bearerToken:      "test-token",
		httpClient:       srv.Client(),
		concurrencyLimit: 4,
		paginatorLimit:   20,
	}
}
