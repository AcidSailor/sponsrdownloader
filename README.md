# sponsrdownloader

A CLI tool for downloading posts from [Sponsr](https://sponsr.ru) as PDFs, with optional video download.

## Installation

### Docker (recommended)

The Docker image bundles all dependencies (Chromium, ffmpeg) — no extra setup needed:

```sh
docker pull ghcr.io/acidsailor/sponsrdownloader:latest
```

### Binary

Download the latest binary for your platform from the [releases page](https://github.com/acidsailor/sponsrdownloader/releases).

The binary requires two additional dependencies:

- **ffmpeg** — install via your system package manager (e.g. `brew install ffmpeg`, `apt install ffmpeg`)
- **Playwright Chromium** — installed automatically on first run (requires internet access). On minimal Linux systems without GUI libraries, Chromium may still fail to launch — use Docker in that case.

## Usage

```sh
sponsrdownloader --bearer-token <token> --session-cookie-value <value> --project-slug <slug> posts
```

Download posts including videos:

```sh
sponsrdownloader ... posts --with-video
```

Filter posts by title (regex):

```sh
sponsrdownloader ... posts --with-filter "episode [0-9]+"
```

### Docker

```sh
docker run --rm \
  -e BEARER_TOKEN=<token> \
  -e SESSION_COOKIE_VALUE=<value> \
  -e PROJECT_SLUG=<slug> \
  -v ./downloads:/home/appuser \
  ghcr.io/acidsailor/sponsrdownloader:latest posts
```

## Obtaining credentials

1. Log in to [sponsr.ru](https://sponsr.ru) in your browser
2. Open DevTools (`F12`) and go to the **Network** tab
3. Refresh the page
4. In the filter box, type `api/v2/content` to narrow down requests
5. Click on any request in the list
6. In the **Headers** section:
   - Find the `Authorization` header — strip the `Bearer ` prefix, the remaining value is your `BEARER_TOKEN`
   - Find the `Cookie` header — look for `SESS=...`, strip the `SESS=` prefix, the remaining value is your `SESSION_COOKIE_VALUE`

## Configuration

All flags can be set via environment variables.

| Flag                     | Env                    | Required | Default | Description                                                                     |
| ------------------------ | ---------------------- | -------- | ------- | ------------------------------------------------------------------------------- |
| `--bearer-token`         | `BEARER_TOKEN`         | yes      |         | Bearer token for Sponsr API                                                     |
| `--session-cookie-value` | `SESSION_COOKIE_VALUE` | yes      |         | Session cookie value                                                            |
| `--project-slug`         | `PROJECT_SLUG`         | yes      |         | Project slug — the part after `sponsr.ru/` in the project URL (e.g. `greenpig`) |
| `--session-cookie-name`  | `SESSION_COOKIE_NAME`  |          | `SESS`  | Session cookie name                                                             |
| `--concurrency-limit`    | `CONCURRENCY_LIMIT`    |          | `10`    | Max concurrent downloads                                                        |
| `--timeout`              | `TIMEOUT`              |          | `30s`   | HTTP request timeout                                                            |
| `--paginator-limit`      | `PAGINATOR_LIMIT`      |          | `20`    | API paginator limit                                                             |
| `--ffmpeg-timeout`       | `FFMPEG_TIMEOUT`       |          | `2h`    | Timeout for ffmpeg video download                                               |

## Commands

### `posts`

Downloads all posts as PDFs into a directory named after the project.

| Flag                    | Description                            |
| ----------------------- | -------------------------------------- |
| `--with-video`          | Also download videos                   |
| `--with-filter <regex>` | Only download posts matching the regex |
