package sponsr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/acidsailor/restkit"
	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"
)

var reProjectID = regexp.MustCompile(`"project_id":\s*(\d+)`)

var ErrSponsrClient = errors.New("sponsr client")

type Client struct {
	rk *restkit.Client
	// httpClient is shared with rk (same transport + rate limiter); the HTML
	// scrape in projectIDBySlugURL uses it raw, bypassing restkit.
	httpClient       *http.Client
	concurrencyLimit int
	paginatorLimit   int
}

// bearerAuthHook returns a restkit RequestHook that attaches the bearer token
// to every request made through restkit (the JSON API calls); the raw HTML
// scrape in projectIDBySlugURL bypasses it.
func bearerAuthHook(token string) restkit.RequestHook {
	return func(r *http.Request) error {
		r.Header.Set("Authorization", "Bearer "+token)
		return nil
	}
}

func NewClient(
	bearerToken string, timeout time.Duration,
	concurrencyLimit, paginatorLimit int,
	requestDelay time.Duration, maxRetries int,
) (*Client, error) {
	if concurrencyLimit <= 0 {
		return nil, fmt.Errorf(
			"concurrencyLimit must be > 0, got %d",
			concurrencyLimit,
		)
	}
	if paginatorLimit <= 0 {
		return nil, fmt.Errorf(
			"paginatorLimit must be > 0, got %d",
			paginatorLimit,
		)
	}
	if requestDelay < 0 {
		return nil, fmt.Errorf(
			"requestDelay must be >= 0, got %s",
			requestDelay,
		)
	}
	if maxRetries < 0 {
		return nil, fmt.Errorf(
			"maxRetries must be >= 0, got %d",
			maxRetries,
		)
	}

	// A burst of 1 makes the limiter enforce a minimum spacing of requestDelay
	// between requests, even across the concurrent paginator goroutines.
	var limiter *rate.Limiter
	if requestDelay > 0 {
		limiter = rate.NewLimiter(rate.Every(requestDelay), 1)
	}

	transport := newRateLimitRetryTransport(
		http.DefaultTransport, limiter, maxRetries, defaultRetryBaseDelay,
	)
	httpClient := &http.Client{Timeout: timeout, Transport: transport}

	rk, err := restkit.New(
		ApiEndpoint,
		restkit.WithName("sponsr"),
		restkit.WithHTTPClient(httpClient),
		restkit.WithHook(bearerAuthHook(bearerToken)),
	)
	if err != nil {
		return nil, errors.Join(ErrSponsrClient, err)
	}

	return &Client{
		rk:               rk,
		httpClient:       httpClient,
		concurrencyLimit: concurrencyLimit,
		paginatorLimit:   paginatorLimit,
	}, nil
}

func CalculatePages(total, limit int) int {
	if limit <= 0 || total <= 0 {
		return 0
	}
	return (total-1)/limit + 1
}

func GetObjects[T any](
	s *Client, ctx context.Context, objectPath string,
	page, limit int,
) (*Objects[T], error) {
	objs, err := restkit.Do[Objects[T]](
		ctx, s.rk, http.MethodGet, objectPath, nil,
		restkit.WithQuery(url.Values{
			"page":  {strconv.Itoa(page)},
			"limit": {strconv.Itoa(limit)},
		}),
	)
	if err != nil {
		return nil, errors.Join(ErrSponsrClient, fmt.Errorf(
			"GET %s: %w", objectPath, err,
		))
	}
	return &objs, nil
}

func GetObjectsAll[T any](
	s *Client,
	ctx context.Context,
	objectPath string,
) ([]T, error) {
	// Fetch page 1 at full limit so its data is reused directly.
	firstPage, err := GetObjects[T](s, ctx, objectPath, 1, s.paginatorLimit)
	if err != nil {
		return nil, err
	}
	if firstPage == nil {
		return nil, fmt.Errorf(
			"%w: response is nil for %s",
			ErrSponsrClient,
			objectPath,
		)
	}

	pages := CalculatePages(firstPage.Total, s.paginatorLimit)
	objects := make([]T, 0, firstPage.Total)
	objects = append(objects, firstPage.List...)

	if pages <= 1 {
		return objects, nil
	}

	var mu sync.Mutex
	eg, ctx := errgroup.WithContext(ctx)
	eg.SetLimit(s.concurrencyLimit)

	for p := 2; p <= pages; p++ {
		eg.Go(func() error {
			resp, err := GetObjects[T](s, ctx, objectPath, p, s.paginatorLimit)
			if err != nil {
				return err
			}
			if resp == nil {
				return fmt.Errorf(
					"%w: response is nil for %s",
					ErrSponsrClient,
					objectPath,
				)
			}
			mu.Lock()
			defer mu.Unlock()
			objects = append(objects, resp.List...)
			return nil
		})
	}

	return objects, eg.Wait()
}

func (s *Client) ProjectIDBySlug(
	ctx context.Context,
	slug string,
) (int, error) {
	id, err := s.projectIDBySlugURL(ctx, ProjectPageURL(slug))
	if err != nil {
		return 0, errors.Join(ErrSponsrClient, &url.Error{
			Op:  http.MethodGet,
			URL: ProjectPageURL(slug),
			Err: err,
		})
	}
	return id, nil
}

func (s *Client) projectIDBySlugURL(
	ctx context.Context,
	pageURL string,
) (int, error) {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, pageURL, nil,
	)
	if err != nil {
		return 0, err
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			slog.Error("could not close response body", "error", closeErr)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("failed to read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf(
			"unexpected status %d: %s",
			resp.StatusCode,
			body,
		)
	}

	match := reProjectID.FindSubmatch(body)
	if match == nil {
		return 0, fmt.Errorf(
			"project_id not found on page %s (body length: %d)",
			pageURL,
			len(body),
		)
	}
	id, err := strconv.Atoi(string(match[1]))
	if err != nil {
		return 0, fmt.Errorf(
			"project_id value %q on page %s is not a valid integer: %w",
			string(match[1]),
			pageURL,
			err,
		)
	}
	return id, nil
}

func (s *Client) Projects(
	ctx context.Context,
	projectID int,
) ([]Project, error) {
	return GetObjectsAll[Project](
		s, ctx,
		fmt.Sprintf("%s?id=%d", ProjectsPath, projectID),
	)
}

func (s *Client) Posts(ctx context.Context, projectID int) ([]Post, error) {
	return GetObjectsAll[Post](
		s, ctx,
		fmt.Sprintf("%s?project_id=%d", PostsPath, projectID),
	)
}
