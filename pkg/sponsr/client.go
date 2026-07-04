package sponsr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"
)

var reProjectID = regexp.MustCompile(`"project_id":\s*(\d+)`)

var ErrSponsrClient = errors.New("sponsr client")

type Client struct {
	bearerToken      string
	httpClient       *http.Client
	concurrencyLimit int
	paginatorLimit   int
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
		http.DefaultTransport, limiter, maxRetries, retryBaseDelay,
	)

	return &Client{
		bearerToken:      bearerToken,
		httpClient:       &http.Client{Timeout: timeout, Transport: transport},
		concurrencyLimit: concurrencyLimit,
		paginatorLimit:   paginatorLimit,
	}, nil
}

// doRequest sends req through the shared client (which rate-limits and retries
// 429 via its transport) and returns the response body and status. The request
// must be idempotent, since the transport may replay it.
func (s *Client) doRequest(
	ctx context.Context,
	req *http.Request,
) (body []byte, status int, err error) {
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			slog.Error("could not close response body", "error", closeErr)
		}
	}()

	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf(
			"failed to read body: %w",
			err,
		)
	}
	return body, resp.StatusCode, nil
}

func PaginatedURL(objectURL string, page, limit int) string {
	sep := "&"
	if !strings.Contains(objectURL, "?") {
		sep = "?"
	}
	return fmt.Sprintf(
		"%s%s%s", objectURL, sep,
		url.Values{
			"page":  {strconv.Itoa(page)},
			"limit": {strconv.Itoa(limit)},
		}.Encode(),
	)
}

func CalculatePages(total, limit int) int {
	if limit <= 0 || total <= 0 {
		return 0
	}
	return (total-1)/limit + 1
}

func GetObjects[T any](
	s *Client, ctx context.Context, objectURL string,
	page, limit int,
) (*Objects[T], error) {
	objects, err := getObjects[T](s, ctx, objectURL, page, limit)
	if err != nil {
		return nil, errors.Join(ErrSponsrClient, &url.Error{
			Op:  http.MethodGet,
			URL: objectURL,
			Err: err,
		})
	}
	return objects, nil
}

func getObjects[T any](
	s *Client, ctx context.Context, objectURL string,
	page, limit int,
) (*Objects[T], error) {
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		PaginatedURL(objectURL, page, limit),
		nil,
	)
	if err != nil {
		return nil, err
	}

	for k, v := range map[string]string{
		"Accept":        "application/json",
		"Authorization": "Bearer " + s.bearerToken,
	} {
		req.Header.Set(k, v)
	}

	body, status, err := s.doRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%d: %s", status, body)
	}

	var object Objects[T]
	if err := json.Unmarshal(body, &object); err != nil {
		return nil, fmt.Errorf("failed to unmarshal body: %w", err)
	}

	return &object, nil
}

func GetObjectsAll[T any](
	s *Client,
	ctx context.Context,
	objectURL string,
) ([]T, error) {
	// Fetch page 1 at full limit so its data is reused directly.
	firstPage, err := GetObjects[T](s, ctx, objectURL, 1, s.paginatorLimit)
	if err != nil {
		return nil, err
	}
	if firstPage == nil {
		return nil, fmt.Errorf(
			"%w: response is nil for %s",
			ErrSponsrClient,
			objectURL,
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
			resp, err := GetObjects[T](s, ctx, objectURL, p, s.paginatorLimit)
			if err != nil {
				return err
			}
			if resp == nil {
				return fmt.Errorf(
					"%w: response is nil for %s",
					ErrSponsrClient,
					objectURL,
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return 0, err
	}
	body, status, err := s.doRequest(ctx, req)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("unexpected status %d: %s", status, body)
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
		fmt.Sprintf("%s?id=%d", ProjectsEndpoint, projectID),
	)
}

func (s *Client) Posts(ctx context.Context, projectID int) ([]Post, error) {
	return GetObjectsAll[Post](
		s, ctx,
		fmt.Sprintf("%s?project_id=%d", PostsEndpoint, projectID),
	)
}
