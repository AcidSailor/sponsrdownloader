# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

CLI tool that downloads posts from [sponsr.ru](https://sponsr.ru) as PDFs (and optionally videos). It authenticates to the Sponsr API, enumerates a project's posts, then renders each post page to PDF via a headless Chromium (Playwright) and downloads videos through the Kinescope embed + `ffmpeg`.

## Commands

Tasks are defined in `taskfile.yaml` (requires [Task](https://taskfile.dev)):

- `task run -- <args>` — run the CLI (`go run ./cmd/ <args>`)
- `task test` — `go test -race ./...`
- `task lint` — format + lint **with autofix** (mutates files: `golangci-lint fmt` + `run --fix`)
- `task ci` — read-only fmt/lint verification, fail-fast (what CI runs; no mutation)
- `task check` — local composite: `lint` (mutating) then `test`
- `task build` — snapshot release build via `goreleaser release --snapshot --clean`
- `task release` — tagged release via `goreleaser release --clean` (CI)
- `task update` — pull latest `go-scaffolds` v1 template tooling (`uvx copier update --vcs-ref v1`)

Run a single test: `go test ./pkg/sponsr/ -run TestName`

Formatting is enforced by `golangci-lint fmt` using **gofumpt (extra-rules)** and **golines with `max-len: 80`** (see `.golangci.yaml`). Keep lines ≤80 chars; the CI `task ci` step fails on any fmt diff.

## Template (Copier)

This repo is adopted onto the [go-scaffolds](https://github.com/acidsailor/go-scaffolds) `go-cli` (kong) Copier template; `.copier-answers.yml` records the link. `task update` pulls the latest `v1` template *tooling* (taskfiles, `.golangci.yaml`, workflow stubs, dependabot, `.gitignore`) via a 3-way merge; business source, `go.mod`, and `README.md` are seed-once and never re-rendered. CI/release run through the template's **reusable** workflows (`go-ci.yml`/`go-release.yml@v1`), so `.github/workflows/{ci,release}.yml` are thin callers and the concrete commands live in this taskfile.

**Intentional local overrides** (diverge from the pristine template, preserved across updates as long as the template doesn't touch them): `Dockerfile.goreleaser` (bakes Playwright + ffmpeg), `.goreleaser.yaml` (`main: ./cmd/`), and the taskfile `run`/`check` tasks. If the template ever changes one of these, `copier update` surfaces a conflict to resolve by hand.

## Runtime dependencies

The compiled binary is not self-contained — it shells out to external tools at runtime:

- **ffmpeg** — must be on `PATH` (looked up in `manager.newManager`); video download fails without it.
- **Playwright Chromium** — auto-installed on first run via `playwright.Install` (needs internet). Launched with `--no-sandbox`.

The Docker image (`Dockerfile.goreleaser`) bundles both.

## Architecture

Data flows: **`cmd` (CLI) → `pkg/sponsr` (API) → `internal/manager` (render/download)**.

- **`cmd/`** — entrypoint. `main.go` wires the [kong](https://github.com/alecthomas/kong) CLI; `posts.go` holds `PostsCmd`, which orchestrates one run: create the sponsr client → resolve project → fetch posts → optional regex filter → download each via an `errgroup` bounded by `--concurrency-limit`. `version`/`commit`/`date` are ldflags-injected by goreleaser.

- **`internal/configuration/`** — `Globals` struct: all global flags/env vars (kong struct tags) plus a `Validate()` invoked in `main` before `Run`. Every flag has an `env` equivalent.

- **`pkg/sponsr/`** — HTTP client for the Sponsr JSON API (`api/v2`).
  - `GetObjectsAll[T]` is the generic paginator: fetches page 1, computes total pages, then fans out remaining pages concurrently (bounded by `concurrencyLimit`) into a mutex-guarded slice.
  - `doRequest` centralizes all requests: applies a `golang.org/x/time/rate` limiter (min spacing = `--request-delay`, burst 1, shared across goroutines) and retries HTTP 429 up to `--max-retries` with backoff that honors `Retry-After` (delta-seconds or HTTP-date), else exponential, capped at `retryMaxDelay`.
  - `ProjectIDBySlug` scrapes the numeric `project_id` out of the project's HTML page via regex (there is no slug→id API endpoint).
  - `api.go` holds the API types + endpoint constants, and `Post.Filename()`/`sanitizeTitle` (filesystem-safe naming: strips `/\:*?"<>|`, control chars, collapses whitespace).

- **`internal/manager/`** — owns the Playwright browser lifecycle and rendering.
  - `Manager` wraps a `BrowserContext` seeded with the session cookie. Consumes any `Downloadable` (`URL`/`Filename`/`IsAvailable`); `*sponsr.Post` satisfies it (compile-time assertion at top of `manager.go`).
  - `DownloadPDF` navigates to the post page, runs a JS auto-scroll to trigger lazy-loaded images, waits for network idle, then `page.PDF()`.
  - `DownloadVideo` loads the post, finds the embedded `kinescope.io` iframe, plays the `<video>`, sniffs the `.m3u8` URL off network requests, then runs `ffmpeg` (with a Kinescope `Referer` header, `-c copy`) under a separate `--ffmpeg-timeout`.
  - **`Close()` must stay deferred** — it is the sole shutdown path for the Playwright process, browser, and context.

## Conventions

- Errors are wrapped with `%w` and package sentinels (`ErrSponsrClient`, `ErrManager`), often via `errors.Join`. Preserve this when adding error paths.
- Logging is `log/slog` structured logging (key/value pairs), not `fmt.Print`.
- Go **1.26**; uses generics (`GetObjects[T]`) and builtins like `min`. Prefer modern Go idioms.
