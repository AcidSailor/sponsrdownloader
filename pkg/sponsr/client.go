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

// retryBaseDelay is the default exponential-backoff base assigned to
// Client.retryBaseDelay when the Sponsr API responds with 429 and provides
// no usable Retry-After header.
const retryBaseDelay = time.Second

// retryMaxDelay caps a single backoff wait. It bounds both very large
// Retry-After values and the exponential growth, which would otherwise
// overflow time.Duration at high retry counts and silently defeat backoff.
const retryMaxDelay = 2 * time.Minute

// maxBackoffShift caps the exponential shift so retryBaseDelay<<shift stays
// well within time.Duration's range regardless of maxRetries.
const maxBackoffShift = 30

type Client struct {
	bearerToken      string
	httpClient       *http.Client
	concurrencyLimit int
	paginatorLimit   int
	// limiter spaces out all outgoing API requests to avoid tripping the
	// server-side throttler; nil disables client-side rate limiting.
	limiter *rate.Limiter
	// maxRetries is the number of extra attempts made on HTTP 429 responses.
	maxRetries int
	// retryBaseDelay is the backoff base used when no Retry-After is provided.
	retryBaseDelay time.Duration
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

	return &Client{
		bearerToken:      bearerToken,
		httpClient:       &http.Client{Timeout: timeout},
		concurrencyLimit: concurrencyLimit,
		paginatorLimit:   paginatorLimit,
		limiter:          limiter,
		maxRetries:       maxRetries,
		retryBaseDelay:   retryBaseDelay,
	}, nil
}

// doRequest executes req, applying client-side rate limiting and retrying on
// HTTP 429 (Too Many Requests) with backoff that honors the Retry-After header.
// It returns the response body together with the final status code. The request
// must be idempotent and carry no body, since it is replayed on retry.
func (s *Client) doRequest(
	ctx context.Context,
	req *http.Request,
) (body []byte, status int, err error) {
	for attempt := 0; ; attempt++ {
		if s.limiter != nil {
			if err := s.limiter.Wait(ctx); err != nil {
				return nil, 0, err
			}
		}

		resp, err := s.httpClient.Do(req)
		if err != nil {
			return nil, 0, err
		}
		body, err = io.ReadAll(resp.Body)
		if closeErr := resp.Body.Close(); closeErr != nil {
			slog.Error("could not close response body", "error", closeErr)
		}
		if err != nil {
			return nil, resp.StatusCode, fmt.Errorf(
				"failed to read body: %w",
				err,
			)
		}

		if resp.StatusCode != http.StatusTooManyRequests {
			return body, resp.StatusCode, nil
		}
		if attempt >= s.maxRetries {
			slog.Warn(
				"sponsr rate limit retries exhausted, giving up",
				"url", req.URL.String(),
				"attempts", attempt+1,
			)
			return body, resp.StatusCode, nil
		}

		wait := s.backoff(resp.Header, attempt)
		slog.Warn(
			"rate limited by sponsr, backing off",
			"url", req.URL.String(),
			"attempt", attempt+1,
			"wait", wait,
		)
		select {
		case <-ctx.Done():
			return nil, resp.StatusCode, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// backoff returns how long to wait before retrying. It honors the server's
// Retry-After header when it specifies a usable delay (a non-negative
// delta-seconds or a future HTTP-date), otherwise it falls back to exponential
// backoff based on retryBaseDelay. A non-empty but unparseable Retry-After is
// logged and treated as absent. Every result is capped at retryMaxDelay.
func (s *Client) backoff(h http.Header, attempt int) time.Duration {
	if v := h.Get("Retry-After"); v != "" {
		if d, ok := parseRetryAfter(v); ok {
			return min(d, retryMaxDelay)
		}
		slog.Warn(
			"unparseable Retry-After header, using exponential backoff",
			"retry_after", v,
		)
	}

	shift := min(attempt, maxBackoffShift)
	return min(s.retryBaseDelay*time.Duration(1<<shift), retryMaxDelay)
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
