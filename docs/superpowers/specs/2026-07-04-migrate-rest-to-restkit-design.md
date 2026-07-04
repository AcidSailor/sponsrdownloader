# Migrate the Sponsr HTTP transport to restkit

**Date:** 2026-07-04
**Status:** Approved (design)

## Problem

`pkg/sponsr/client.go` hand-rolls a full HTTP-JSON transport layer: request
building, auth header injection, sending, status checking, JSON decoding, and
error typing. `github.com/acidsailor/restkit` (v0.1.2) already owns exactly this
"repetitive parts of talking to a JSON API" surface via its generic `Do[T]`,
`RequestHook`, `WithQuery`/`Values`, and typed errors. We want to delegate that
core to restkit so the sponsr package keeps only what is genuinely
Sponsr-specific: pagination, the HTML `project_id` scrape, and the API's
throttling behaviour.

## What restkit already provides (and replaces)

| Current sponsr code | restkit replacement |
| --- | --- |
| `http.NewRequestWithContext` + manual header set | `restkit.Do[T]` builds the request, sets `Accept` |
| `Authorization: Bearer` header block | client-wide `RequestHook` via `WithHook` |
| `json.Unmarshal(body, &object)` | `restkit.Do[Objects[T]]` decodes into `T` |
| status-code check + `fmt.Errorf("%d: %s", ...)` | `*restkit.ResponseError` (status + raw body) |
| `PaginatedURL` string-building | `restkit.WithQuery` + `restkit.NewValues` |
| `&http.Client{Timeout: timeout}` | `restkit.WithHTTPClient` |

## What restkit does NOT provide (stays in sponsr)

restkit is intentionally minimal and JSON-only. Two concerns have no home in it
and remain in the sponsr package:

1. **Rate limiting + 429/`Retry-After` retry.** restkit issues one round-trip
   and turns any non-2xx into a `*ResponseError`. Today's `doRequest` loop,
   `backoff`, and `parseRetryAfter` implement client-side spacing and 429
   backoff. **Decision:** relocate this logic behind an `http.RoundTripper`.
   Rate-limiting and retry are transport concerns; a `RoundTripper` is the
   idiomatic seam, and restkit accepts a custom transport via
   `WithHTTPClient`. This keeps restkit unchanged (single repo, single PR) and
   is trivially promotable into restkit later if it proves reusable.

2. **The HTML `project_id` scrape (`ProjectIDBySlug`).** It targets a different
   base URL (`https://sponsr.ru/<slug>/`, not `.../api/v2`) and returns HTML,
   not JSON, so `Do[T]` cannot serve it regardless of base URL. It keeps a thin
   raw `httpClient.Do`, reusing the *same* `*http.Client` so it still benefits
   from the shared rate-limit/retry transport.

## Architecture

Data flow after migration:

```
NewClient
  └─ rate.Limiter (nil when requestDelay == 0)
       └─ rateLimitRetryTransport (http.RoundTripper)   ← new: transport.go
            └─ http.DefaultTransport
  └─ *http.Client{ Timeout, Transport: rateLimitRetryTransport }
       ├─ restkit.New(ApiEndpoint, WithName("sponsr"),
       │              WithHTTPClient(hc), WithHook(bearerAuth))   ← JSON path
       └─ (raw) hc.Do(...)  for the HTML project page             ← HTML path
```

The single `*http.Client` (and thus the single limiter + retry policy) is
shared by both the restkit JSON path and the raw HTML path, and across all
paginator goroutines — preserving the current shared-limiter semantics.

### New file: `pkg/sponsr/transport.go`

A `rateLimitRetryTransport` that wraps a base `http.RoundTripper`:

```go
type rateLimitRetryTransport struct {
    base           http.RoundTripper // http.DefaultTransport
    limiter        *rate.Limiter     // nil disables spacing
    maxRetries     int
    retryBaseDelay time.Duration
}

func (t *rateLimitRetryTransport) RoundTrip(req *http.Request) (*http.Response, error)
```

`RoundTrip` absorbs today's `doRequest` control flow, `backoff`, and
`parseRetryAfter` **verbatim in behaviour**:

- `limiter.Wait(req.Context())` before each attempt (when non-nil).
- On a response whose status is not 429, return it as-is.
- On 429 with retries remaining, compute the wait via `backoff(resp.Header,
  attempt)` (Retry-After delta-seconds / HTTP-date, else capped exponential),
  drain+close the 429 body, rewind the request body via `req.GetBody` when
  present, and retry.
- On 429 with retries exhausted, log the existing `slog.Warn` ("rate limit
  retries exhausted") and return the final 429 response — restkit then maps it
  to a `*ResponseError`. (Behaviour change: exhausted 429 now surfaces as a
  typed error to callers instead of the raw body; the wrapping still carries
  `ErrSponsrClient`.)
- Respect `ctx.Done()` during the backoff sleep, as today.

`backoff` and `parseRetryAfter` move into this file unchanged. `retryBaseDelay`
and `retryMaxDelay` constants move with them.

Replay safety: sponsr issues only GETs, and restkit builds requests with a
`*bytes.Reader` body, so `http.NewRequestWithContext` populates `req.GetBody`;
the transport rewinds via `GetBody` before each retry. The HTML GET has a nil
body and is likewise replay-safe.

### `pkg/sponsr/client.go` changes

`Client` struct becomes:

```go
type Client struct {
    rk               *restkit.Client
    httpClient       *http.Client // shared; used raw for the HTML scrape
    concurrencyLimit int
    paginatorLimit   int
}
```

Removed fields: `bearerToken`, `limiter`, `maxRetries`, `retryBaseDelay` (all
now live inside the transport / hook).

`NewClient` keeps its current signature and all validation
(`concurrencyLimit`, `paginatorLimit`, `requestDelay`, `maxRetries` bounds). It:

1. Builds the limiter (nil when `requestDelay == 0`), as today.
2. Builds `rateLimitRetryTransport{base: http.DefaultTransport, limiter,
   maxRetries, retryBaseDelay}`.
3. Builds `hc := &http.Client{Timeout: timeout, Transport: transport}`.
4. Builds the bearer-auth hook:
   ```go
   auth := func(r *http.Request) error {
       r.Header.Set("Authorization", "Bearer "+bearerToken)
       return nil
   }
   ```
5. `rk, err := restkit.New(ApiEndpoint, restkit.WithName("sponsr"),
   restkit.WithHTTPClient(hc), restkit.WithHook(auth))` — propagate any
   `*ConfigError` wrapped in `ErrSponsrClient`.

`getObjects[T]` collapses to:

```go
objs, err := restkit.Do[Objects[T]](
    ctx, s.rk, http.MethodGet, objectPath,
    nil,
    restkit.WithQuery(url.Values{
        "page":  {strconv.Itoa(page)},
        "limit": {strconv.Itoa(limit)},
    }),
)
```

where `objectPath` is the path **relative to** `ApiEndpoint` (e.g.
`/content/posts?project_id=123`). See "Endpoint constants" below.

`GetObjects[T]` keeps wrapping failures with `ErrSponsrClient` via
`errors.Join`, now joining restkit's typed error rather than a hand-built
`*url.Error`.

Unchanged: `GetObjectsAll[T]`, `CalculatePages`, the errgroup fan-out,
`Projects`, `Posts`. `PaginatedURL` is deleted (superseded by `WithQuery`);
`doRequest` is deleted (superseded by the transport).

`ProjectIDBySlug` / `projectIDBySlugURL`: unchanged regex/parse logic, but the
raw request now goes through `s.httpClient.Do` (the shared, rate-limited
client) instead of the removed `doRequest`. Status/`Retry-After` handling for
this path is provided by the transport; the method keeps its own
non-200 check and `ErrSponsrClient` wrapping.

### Endpoint constants (`api.go`)

restkit is constructed with base `ApiEndpoint`, so JSON paths must be relative
to it. Introduce path constants alongside the existing absolute URLs:

```go
PostsPath    = "/content/posts"    // relative to ApiEndpoint
ProjectsPath = "/content/projects"
```

`Projects`/`Posts` pass `PostsPath`/`ProjectsPath` (plus their `?id=`/
`?project_id=` query, or move those into `WithQuery`). The absolute
`PostsEndpoint`/`ProjectsEndpoint` constants may be removed if no longer
referenced; `Endpoint`/`ApiEndpoint`/`ProjectPageURL` remain (the HTML path and
base URL still need them). Exact query construction (inline `?id=` vs
`WithQuery`) is an implementation detail to settle during coding, kept
behaviourally identical.

## Dependency impact

Adding restkit pulls `go.opentelemetry.io/contrib/.../otelhttp` and the
OpenTelemetry API/SDK into `go.mod`. The CLI configures no tracer, so these run
against global no-op providers (inert at runtime) — but they are real new
dependencies. Accepted as the cost of removing the hand-rolled transport.
`golang.org/x/time/rate` and `golang.org/x/sync` remain (limiter + errgroup).

## Error handling

- Public sentinel `ErrSponsrClient` is preserved on every exported error path,
  via `errors.Join`, now joining `*restkit.RequestError` / `*restkit.ResponseError`
  instead of `*url.Error`.
- restkit's typed errors remain matchable with `errors.As` for callers that
  want status codes, but the sponsr package's public contract stays "wrapped in
  `ErrSponsrClient`".
- The one intentional behaviour change: a 429 that exhausts all retries now
  yields a typed `*ResponseError` (status 429) rather than the previous
  best-effort return of the raw body with a nil error.

## Testing

`client_test.go` and `api_test.go` are updated, not rewritten:

- Existing `httptest.Server`-based tests still exercise the full stack; they
  drive `NewClient` → restkit → transport → test server. Assertions on returned
  data and on `errors.Is(err, ErrSponsrClient)` are preserved.
- Retry/`Retry-After`/backoff tests move to target the transport (or continue
  through the public client). `parseRetryAfter`/`backoff` unit tests move with
  the functions into `transport_test.go` (or stay in `client_test.go` if the
  functions stay package-private and tests are simplest there).
- Add a test asserting the `Authorization: Bearer` hook fires and that
  `page`/`limit` query params arrive on the server (previously covered by
  `PaginatedURL` tests + request inspection).
- Add a test that an exhausted-retry 429 surfaces as `ErrSponsrClient` (the
  behaviour change above).
- `task check` (mutating lint + `go test ./...`) must pass; lines ≤80 chars
  (golines), gofumpt-clean.

## Out of scope

- Modifying the restkit repo (no retry/raw features added there for now).
- Configuring an OpenTelemetry exporter/tracer in the CLI.
- Any change to `internal/manager`, the CLI, or download behaviour.
```
