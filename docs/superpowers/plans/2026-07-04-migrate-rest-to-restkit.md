# Migrate Sponsr HTTP transport to restkit — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the hand-rolled HTTP-JSON transport in `pkg/sponsr` with `github.com/acidsailor/restkit`, keeping rate-limiting and 429 retry as a local `http.RoundTripper`.

**Architecture:** Extract today's rate-limit + 429/Retry-After logic into a `rateLimitRetryTransport` (an `http.RoundTripper`) in Task 1 — a pure refactor that keeps every existing test green. Then in Task 2, swap the manual request-build/decode/status/error code for `restkit.Do[T]`, inject auth via a `RequestHook`, and route the HTML `project_id` scrape through the same shared `*http.Client`.

**Tech Stack:** Go 1.26, `github.com/acidsailor/restkit` v0.1.2, `golang.org/x/time/rate`, `golang.org/x/sync/errgroup`, `net/http`, testify.

## Global Constraints

- Go **1.26**; prefer modern idioms (generics, `min`).
- Lines **≤80 chars** (golines); code must be gofumpt-clean. `task ci` fails on any fmt diff. Run `task lint` (mutating autofix) before committing.
- Every exported error path wraps the package sentinel `ErrSponsrClient`, via `errors.Join`.
- Logging is `log/slog` structured (key/value), never `fmt.Print`.
- restkit import path is lowercase `github.com/acidsailor/restkit` (the module declares lowercase, despite the GitHub org `AcidSailor`). Pin **v0.1.2**.
- `NewClient`'s signature is fixed (external caller `cmd/posts.go:25`): `NewClient(bearerToken string, timeout time.Duration, concurrencyLimit, paginatorLimit int, requestDelay time.Duration, maxRetries int) (*Client, error)`.
- Public methods `Projects`, `Posts`, `ProjectIDBySlug` keep their signatures (used by `cmd/posts.go`).
- Run tests with `task test` (`go test ./...`). Single package: `go test ./pkg/sponsr/`.

---

## File Structure

- `pkg/sponsr/transport.go` — **new.** `rateLimitRetryTransport` (`http.RoundTripper`) + `backoff` + `parseRetryAfter` + the `retryBaseDelay`/`retryMaxDelay` constants. Owns all throttling/retry behaviour.
- `pkg/sponsr/transport_test.go` — **new.** Unit tests for the transport: backoff table, 429 retry, exhaustion, context-cancel-during-backoff, rate-limiter spacing, body-rewind-on-retry.
- `pkg/sponsr/client.go` — **modified.** Task 1: extract transport, thin `doRequest`. Task 2: replace request/decode/error internals with restkit; add `bearerAuthHook`; migrate `ProjectIDBySlug` to raw `httpClient.Do`; delete `doRequest` and `PaginatedURL`.
- `pkg/sponsr/client_test.go` — **modified.** Rebuild `newTestClient` helper; drop transport-level tests (moved to `transport_test.go`); switch full-URL args to base-relative paths; add auth/query + exhaustion integration tests.
- `pkg/sponsr/api.go` — **modified.** Add `PostsPath`/`ProjectsPath` (relative to `ApiEndpoint`); remove now-unused `PostsEndpoint`/`ProjectsEndpoint`.
- `go.mod` / `go.sum` — **modified.** Add restkit + its OpenTelemetry transitive deps.

---

## Task 1: Extract rate-limit + 429 retry into an http.RoundTripper

Pure refactor. No restkit yet. The public API and all observable behaviour stay identical; the retry/backoff/limiter logic simply moves behind a transport. After this task the full suite is green with the logic relocated.

**Files:**
- Create: `pkg/sponsr/transport.go`
- Create: `pkg/sponsr/transport_test.go`
- Modify: `pkg/sponsr/client.go` (struct, `NewClient`, `doRequest`; remove `backoff`/`parseRetryAfter`/constants)
- Modify: `pkg/sponsr/client_test.go` (move transport tests out; rebuild `newTestClient`; rewrite `TestNewClient_RateLimiter`)

**Interfaces:**
- Produces:
  - `type rateLimitRetryTransport struct { base http.RoundTripper; limiter *rate.Limiter; maxRetries int; retryBaseDelay time.Duration }`
  - `func newRateLimitRetryTransport(base http.RoundTripper, limiter *rate.Limiter, maxRetries int, retryBaseDelay time.Duration) *rateLimitRetryTransport`
  - `func (t *rateLimitRetryTransport) RoundTrip(req *http.Request) (*http.Response, error)`
  - `func (t *rateLimitRetryTransport) backoff(h http.Header, attempt int) time.Duration`
  - `const retryBaseDelay = time.Second`, `const retryMaxDelay = 2 * time.Minute`
  - `Client` now holds `bearerToken string; httpClient *http.Client; concurrencyLimit int; paginatorLimit int` (fields `limiter`, `maxRetries`, `retryBaseDelay` removed).

---

- [ ] **Step 1: Create `pkg/sponsr/transport.go`**

```go
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
```

- [ ] **Step 2: Edit `pkg/sponsr/client.go` — remove the moved logic, wire the transport**

Delete from `client.go`: the `retryBaseDelay`/`retryMaxDelay` const block, the `backoff` method, the `parseRetryAfter` function, and the retry loop inside `doRequest` (all now live in `transport.go`).

Replace the `Client` struct (was lines ~36–48) with:

```go
type Client struct {
	bearerToken      string
	httpClient       *http.Client
	concurrencyLimit int
	paginatorLimit   int
}
```

In `NewClient`, keep all validation unchanged. Replace the construction tail (the `limiter` build + `return &Client{...}`) with:

```go
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
```

Replace the whole `doRequest` method with this thin version (rate-limit and retry now happen inside the transport):

```go
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
```

Now fix `client.go` imports: remove `"strconv"` **only if unused** (it is still used by `PaginatedURL` and `projectIDBySlugURL` — keep it), remove `"time"` **only if unused** (still used by `NewClient` timeout param — keep it). The imports that become unused after removing the retry loop are none beyond what moved; `io`, `slog`, `fmt`, `net/http`, `context` all remain used by the thin `doRequest`. Verify with the build in Step 4.

- [ ] **Step 3: Move transport tests into `pkg/sponsr/transport_test.go`; rebuild the client-test helper**

Create `pkg/sponsr/transport_test.go`:

```go
package sponsr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
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
```

Add the imports `"bytes"` and `"io"` to `transport_test.go`'s import block (used by the body-rewind test). Final import set: `bytes`, `context`, `io`, `net/http`, `net/http/httptest`, `sync/atomic`, `testing`, `time`, testify `assert`/`require`, `golang.org/x/time/rate`. (`rate` is used by the spacing test kept in `client_test.go`? No — see below; if `rate` ends up unused here, drop it. It is used by `TestGetObjectsAll_RateLimiterSpacesRequests`, which stays in `client_test.go`. So **remove `rate` from `transport_test.go` imports** — none of the tests above reference it.)

Now edit `pkg/sponsr/client_test.go`:

Delete these tests (moved to `transport_test.go`): `TestBackoff_RetryAfterSeconds`, `TestBackoff_RetryAfterHTTPDate`, `TestBackoff_RetryAfterFallbacks`, `TestBackoff_Exponential`, `TestBackoff_CapsLargeDelays`, `TestGetObjects_RetriesOn429`, `TestDoRequest_ContextCanceledDuringBackoff`.

Rewrite the `newTestClient` helper to build the shared client with the transport (still no restkit in Task 1):

```go
func newTestClient(srv *httptest.Server) *Client {
	return &Client{
		bearerToken: "test-token",
		httpClient: &http.Client{
			Transport: newRateLimitRetryTransport(
				srv.Client().Transport, nil, 0, time.Millisecond,
			),
		},
		concurrencyLimit: 4,
		paginatorLimit:   20,
	}
}
```

Rewrite `TestGetObjectsAll_RateLimiterSpacesRequests` to set the limiter on a fresh transport instead of `client.limiter`:

```go
	const delay = 25 * time.Millisecond
	client := &Client{
		bearerToken: "test-token",
		httpClient: &http.Client{
			Transport: newRateLimitRetryTransport(
				srv.Client().Transport,
				rate.NewLimiter(rate.Every(delay), 1),
				0,
				time.Millisecond,
			),
		},
		concurrencyLimit: 4,
		paginatorLimit:   20,
	}
```
(Leave the rest of that test — the timestamp collection and spacing assertion — unchanged.)

Rewrite `TestNewClient_RateLimiter` to inspect the transport instead of removed fields:

```go
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
```

Keep `TestGetObjects_429ExhaustsRetries` in `client_test.go` but update its helper usage — it needs a client whose transport has `maxRetries: 2`. Replace its client setup with:

```go
	client := &Client{
		bearerToken: "test-token",
		httpClient: &http.Client{
			Transport: newRateLimitRetryTransport(
				srv.Client().Transport, nil, 2, time.Millisecond,
			),
		},
		concurrencyLimit: 4,
		paginatorLimit:   20,
	}
```
(Its assertions — `require.Error`, `Contains "429"`, `calls == 3` — stay; the non-200 check in `getObjects` still turns the exhausted 429 into an error.)

- [ ] **Step 4: Build and run the suite — expect green**

Run: `go build ./... && go test ./pkg/sponsr/ -count=1`
Expected: PASS. If `go vet`/build reports an unused import in `client.go` or `client_test.go`, remove only the flagged import and re-run.

- [ ] **Step 5: Lint and commit**

Run: `task lint` (autofixes fmt), then `go test ./pkg/sponsr/ -count=1` again.
Expected: no fmt diff, tests PASS.

```bash
git add pkg/sponsr/transport.go pkg/sponsr/transport_test.go \
        pkg/sponsr/client.go pkg/sponsr/client_test.go
git commit -m "refactor(sponsr): extract rate-limit + 429 retry into a RoundTripper"
```

---

## Task 2: Replace the manual JSON transport with restkit

Swap the hand-rolled request build / decode / status / error code in the JSON path for `restkit.Do[T]`, inject auth via a `RequestHook`, and route the HTML `project_id` scrape through the same shared client. `doRequest` and `PaginatedURL` are deleted.

**Files:**
- Modify: `go.mod`, `go.sum` (add restkit)
- Modify: `pkg/sponsr/api.go` (add `PostsPath`/`ProjectsPath`; remove `PostsEndpoint`/`ProjectsEndpoint`)
- Modify: `pkg/sponsr/client.go` (struct, `NewClient`, `getObjects`, `GetObjects`, `Projects`, `Posts`, `projectIDBySlugURL`; add `bearerAuthHook`; delete `doRequest`, `PaginatedURL`)
- Modify: `pkg/sponsr/client_test.go` (rebuild helper on restkit; paths not URLs; add auth/query test; delete `TestPaginatedURL`)

**Interfaces:**
- Consumes (from Task 1): `newRateLimitRetryTransport`, `retryBaseDelay`, the `rateLimitRetryTransport` type.
- Consumes (restkit v0.1.2): `restkit.New(endpoint string, ...Option) (*Client, error)`, `restkit.Do[T any](ctx, *Client, method, path string, body any, hooks ...RequestHook) (T, error)`, `restkit.WithName`, `restkit.WithHTTPClient`, `restkit.WithHook`, `restkit.WithQuery(url.Values) RequestHook`, `restkit.RequestHook`.
- Produces:
  - `Client` now holds `rk *restkit.Client; httpClient *http.Client; concurrencyLimit int; paginatorLimit int` (`bearerToken` removed).
  - `func bearerAuthHook(token string) restkit.RequestHook`
  - `PostsPath = "/content/posts"`, `ProjectsPath = "/content/projects"` (relative to `ApiEndpoint`).
  - `GetObjects`/`GetObjectsAll`/`getObjects` take `objectPath` (a path relative to `ApiEndpoint`) rather than a full URL.

---

- [ ] **Step 1: Add the restkit dependency**

Run:
```bash
go get github.com/acidsailor/restkit@v0.1.2
go mod tidy
```
Expected: `go.mod` gains `github.com/acidsailor/restkit v0.1.2` and OpenTelemetry (`go.opentelemetry.io/...`) indirect requires; `go.sum` updated. Confirm with `go build ./...` (still compiles — nothing imports restkit yet).

- [ ] **Step 2: Edit `pkg/sponsr/api.go` — add relative paths**

Replace the endpoint const block:

```go
const (
	Domain           = "sponsr.ru"
	Endpoint         = "https://" + Domain
	ApiEndpoint      = Endpoint + "/api/v2"
	PostsEndpoint    = ApiEndpoint + "/content/posts"
	ProjectsEndpoint = ApiEndpoint + "/content/projects"
)
```
with:

```go
const (
	Domain      = "sponsr.ru"
	Endpoint    = "https://" + Domain
	ApiEndpoint = Endpoint + "/api/v2"
	// PostsPath and ProjectsPath are relative to ApiEndpoint (the restkit
	// client's base URL).
	PostsPath    = "/content/posts"
	ProjectsPath = "/content/projects"
)
```

- [ ] **Step 3: Rewrite the JSON path and auth in `pkg/sponsr/client.go`**

Update imports: add `"github.com/acidsailor/restkit"`. After this task `context`, `errors`, `fmt`, `io`, `net/http`, `net/url`, `regexp`, `slog`, `strconv`, `sync`, `time`, `errgroup`, `rate`, `restkit` are all used (verify in Step 6). Remove `doRequest` and `PaginatedURL` entirely.

Replace the `Client` struct with:

```go
type Client struct {
	rk               *restkit.Client
	httpClient       *http.Client // shared; used raw for the HTML scrape
	concurrencyLimit int
	paginatorLimit   int
}
```

Add the auth hook constructor (place near `NewClient`):

```go
// bearerAuthHook returns a restkit RequestHook that attaches the bearer token
// to every request.
func bearerAuthHook(token string) restkit.RequestHook {
	return func(r *http.Request) error {
		r.Header.Set("Authorization", "Bearer "+token)
		return nil
	}
}
```

Replace the construction tail of `NewClient` (the transport build + `return &Client{...}` from Task 1) with:

```go
	var limiter *rate.Limiter
	if requestDelay > 0 {
		limiter = rate.NewLimiter(rate.Every(requestDelay), 1)
	}

	transport := newRateLimitRetryTransport(
		http.DefaultTransport, limiter, maxRetries, retryBaseDelay,
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
```

Replace `GetObjects` and `getObjects` with:

```go
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
```
(The former unexported `getObjects` helper is folded in; delete it.)

Update `GetObjectsAll` to rename its `objectURL` parameter to `objectPath` (behaviour unchanged; it just forwards to `GetObjects`). The error-nil checks and errgroup fan-out stay exactly as they are.

Replace `Projects` and `Posts` with path-based versions:

```go
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
```

- [ ] **Step 4: Migrate `projectIDBySlugURL` to the raw shared client**

`ProjectIDBySlug` (the exported wrapper) is unchanged. Replace `projectIDBySlugURL`'s body — which called the deleted `doRequest` — with a direct `httpClient.Do` (it still benefits from the transport's rate-limit/retry, and reads raw HTML, which restkit cannot):

```go
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
```
(`ProjectIDBySlug` continues to wrap this method's error with `errors.Join(ErrSponsrClient, &url.Error{...})` — leave it unchanged.)

- [ ] **Step 5: Update `pkg/sponsr/client_test.go` for the restkit-based client**

Add import `"github.com/acidsailor/restkit"`.

Delete `TestPaginatedURL` (the function is gone).

Rebuild `newTestClient` on top of restkit (base = test server URL):

```go
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
```

Update `TestNewClient_RateLimiter` — the transport now lives behind restkit, but `c.httpClient` still points at the *same* shared transport, so the existing assertions from Task 1 still hold. Keep it as written in Task 1 (`c.httpClient.Transport.(*rateLimitRetryTransport)`).

Switch every full-URL argument to a base-relative path (restkit prepends `srv.URL`):
- In `TestGetObjects`, `TestGetObjectsAll_Pagination`, `TestGetObjects_HTTPError`, `TestGetObjectsAll_RateLimiterSpacesRequests`, and `TestGetObjects_429ExhaustsRetries`, replace `srv.URL+"/posts?project_id=1"` with `"/posts?project_id=1"`.
- `TestProjectIDBySlug` / `_NotFound` call `projectIDBySlugURL(ctx, srv.URL+"/")` — these use the raw `httpClient`, **not** restkit, so keep the full `srv.URL+"/"`.

Rebuild the limiter-spacing and 429-exhaustion clients on the new helper:

```go
	// TestGetObjectsAll_RateLimiterSpacesRequests:
	const delay = 25 * time.Millisecond
	client := newTestClientTransport(srv, newRateLimitRetryTransport(
		srv.Client().Transport,
		rate.NewLimiter(rate.Every(delay), 1),
		0,
		time.Millisecond,
	))
```

```go
	// TestGetObjects_429ExhaustsRetries:
	client := newTestClientTransport(srv, newRateLimitRetryTransport(
		srv.Client().Transport, nil, 2, time.Millisecond,
	))
```
Its assertions stay, but add `assert.ErrorIs(t, err, ErrSponsrClient)` after the existing `require.Error`. The `Contains "429"` assertion still holds — restkit's `*ResponseError.Error()` is `"sponsr: status 429, body: ..."`.

`TestGetObjectsAll_Pagination` sets `client.paginatorLimit = limit` — still valid (field retained). Keep it, but build `client` via `newTestClient(srv)` first, then set `client.paginatorLimit = limit`.

Add a new test proving the auth hook fires and query params merge without clobbering `project_id`:

```go
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
```

- [ ] **Step 6: Build and run the full suite**

Run: `go build ./... && go test ./... -count=1`
Expected: PASS. Common fixups if the build complains:
- unused import in `client.go` → remove only the flagged one.
- `go test ./cmd/...` still compiles because `NewClient`, `Projects`, `Posts`, `ProjectIDBySlug` signatures are unchanged.

- [ ] **Step 7: Full check + commit**

Run: `task check` (mutating lint + `go test ./...`).
Expected: no fmt diff, all tests PASS.

```bash
git add go.mod go.sum pkg/sponsr/api.go pkg/sponsr/client.go \
        pkg/sponsr/client_test.go
git commit -m "feat(sponsr): migrate JSON transport to restkit"
```

---

## Risks & Notes

- **restkit sends a JSON body on GET.** `restkit.Do` marshals a nil body to `null` and attaches it as the request body even for GETs. Against `httptest` this is inert, and the transport rewinds it correctly on retry (covered by `TestTransport_RewindsBodyOnRetry`). If the real sponsr API or its CDN rejects GETs carrying a body, that surfaces as a non-2xx `*ResponseError` — the fallback would be to extend restkit with a bodyless-GET path (out of scope here). Flag if manual smoke-testing against the live API fails.
- **OpenTelemetry dependency weight** is accepted (see spec). Inert at runtime with no configured tracer.
- **HTML scrape is untraced.** `s.httpClient` (used raw for `projectIDBySlugURL`) is not otel-wrapped, unlike restkit's internal copy. Acceptable — tracing isn't configured anyway.
- **No CLI/manager changes.** `cmd/posts.go` and `internal/manager` are untouched.

## Self-Review

- **Spec coverage:** transport extraction (Task 1) ✓; restkit `Do`/`WithHook`/`WithQuery`/`WithHTTPClient`/`WithName` (Task 2 Step 3) ✓; typed-error wrapping via `ErrSponsrClient` (GetObjects, NewClient, ProjectIDBySlug) ✓; HTML scrape on shared client (Task 2 Step 4) ✓; endpoint→path constants (Step 2) ✓; dependency add (Step 1) ✓; tests for auth/query/retry/exhaustion/pacing/cancel/rewind ✓.
- **Placeholder scan:** none — every code step is complete.
- **Type consistency:** `newRateLimitRetryTransport(base, limiter, maxRetries, retryBaseDelay)` used identically in `transport.go`, `NewClient`, and all test helpers; `rateLimitRetryTransport` fields (`base`,`limiter`,`maxRetries`,`retryBaseDelay`) consistent; `bearerAuthHook(token)` signature consistent across `NewClient` and test helper; `PostsPath`/`ProjectsPath` used in `Posts`/`Projects`; `restkit.Do[Objects[T]]` return handled as value `objs` then `&objs`.
```
