# Skip already-downloaded files

**Status:** approved design, ready for implementation planning
**Date:** 2026-07-04

## Problem

`Manager.DownloadPDF` and `Manager.DownloadVideo` write their output files
unconditionally on every run, re-rendering each post through headless Chromium
(PDF) and re-remuxing through ffmpeg (video) even when an identical, up-to-date
file already sits on disk. For a project with many posts this wastes large
amounts of time on work that was already done.

We want to skip a download when we already hold a good, current copy — while
still re-downloading when the local file is missing/corrupt **or** when the post
was edited on Sponsr.

## Two independent failure signals

The skip decision must guard against two *different* failures, so it tracks two
signals per file:

| Signal          | Catches                                   | Misses on its own                      |
| --------------- | ----------------------------------------- | -------------------------------------- |
| `updated_at`    | server-side edits (timestamp advances)    | local corruption / truncation          |
| crc32c checksum | truncated / corrupt local file            | server edits (file matches own hash)   |

Concretely:

- A **content-only edit** keeps the same filename, so the stale-but-intact local
  PDF would match its own stored crc32c and be wrongly skipped — only
  `updated_at` catches this.
- An **interrupted ffmpeg mp4** has the correct `updated_at` but is truncated —
  only crc32c catches this.

Note on PDF non-determinism: headless Chromium renders a *different* PDF (byte
for byte) every run. This does **not** break the skip logic, because we never
re-render to compare. crc32c is always computed over the file **already on
disk** and compared to the checksum recorded **when that exact file was
written** — a fresh render is never produced unless we have already decided to
download.

### API discovery

The Sponsr posts response (`GET /api/v2/content/posts`) carries fields we were
silently dropping, including **`updated_at`** (RFC3339, e.g.
`2026-06-29T18:01:55.000Z`). It genuinely tracks edits — observed a post with
`created_at 13:53:44` and `updated_at 18:01:55`. `ts` is a per-response "as of
now" timestamp and is ignored.

## Design

### Storage: per-file sidecars (lock-free)

No shared manifest, no mutex. Each output file gets a sibling sidecar under a
`.checksums/` subdirectory of the manager's output folder:

```
<output>/2026-07-02 - Some Post.pdf
<output>/.checksums/2026-07-02 - Some Post.pdf.json
<output>/2026-07-02 - Some Post.mp4
<output>/.checksums/2026-07-02 - Some Post.mp4.json
```

Each goroutine only ever reads/writes its **own** sidecar, so there is zero
shared mutable state — no synchronization primitive is needed, and a crash
mid-run leaves every finished file's sidecar intact. The `.checksums/`
subdirectory keeps the output folder visually clean.

Sidecar content:

```json
{
  "updated_at": "2026-06-29T18:01:55Z",
  "crc32": "1a2b3c4d"
}
```

- `updated_at` — the post's `updated_at`, stored as RFC3339.
- `crc32` — hex CRC-32 (Castagnoli / crc32c) of the output file bytes.

### Hash choice

`hash/crc32` with the **Castagnoli** table (`crc32.MakeTable(crc32.Castagnoli)`).
Stdlib, hardware-accelerated on amd64 and arm64, purpose-built for corruption
detection, zero dependencies. In practice disk read dominates, so the checksum
compute is effectively free relative to the re-download it prevents.

### Skip logic (cheap → expensive)

Placed inside the exported `DownloadPDF` / `DownloadVideo` wrappers, before any
Playwright/ffmpeg work. `cmd/posts.go` and the download inner functions are
untouched.

For a given output path and post:

1. If the output file does **not** exist → download.
2. Read the sidecar. Missing or unparseable → download.
3. If sidecar `updated_at` != post `updated_at` → download (**short-circuit: the
   file is not read/hashed** — the edit alone settles it).
4. Compute crc32c of the file on disk. If it != sidecar `crc32` → download.
5. Otherwise → **skip** (log `skipped, already downloaded`), return `nil`.

Ordering matters: the `updated_at` compare is a free string check that avoids
reading the file at all when the post was edited.

After a successful download, write the sidecar with the current `updated_at` and
the freshly computed crc32c.

### Interface change

`Downloadable` gains one method so the manager can read the change-signal
without depending on `sponsr` internals:

```go
type Downloadable interface {
    URL() string
    Filename() string
    IsAvailable() bool
    UpdatedAt() time.Time   // new
}
```

`*sponsr.Post` implements it by mapping the `updated_at` JSON field into a new
`time.Time` struct field (mirroring the existing `Date` handling). The
compile-time assertion `var _ Downloadable = (*sponsr.Post)(nil)` continues to
enforce this.

### Error handling

- Sidecar **read** failure (missing / unparseable / unreadable) → treat as "not
  downloaded", i.e. proceed to download. Never aborts the run.
- Sidecar **write** failure *after* a successful download → log a warning and
  continue; the file on disk is valid, worst case it re-downloads next run. Does
  not fail the run.
- Hard errors that warrant failing a single item (e.g. cannot stat the output
  directory) are wrapped with the existing `ErrManager` sentinel, consistent
  with the current code.

## Scope / non-goals

- Title edits change the derived filename; the newly named file has no matching
  sidecar and is downloaded fresh, leaving the old file orphaned. This matches
  existing behavior and is out of scope (no orphan cleanup).
- No `--verify` flag, no size fast-path, no single manifest — explicitly
  rejected during design in favor of the always-hash sidecar model.
- No new CLI flags. Skipping is the default behavior. (A future opt-out flag
  could be added but is not part of this work.)

## Testing

The sidecar + checksum logic lives in a Playwright-independent file so it is
unit-testable without a browser:

- crc32c of a small temp file (known vector).
- Sidecar round-trip: write then read returns the same struct.
- Read of a missing sidecar → "not downloaded" outcome.
- Read of a corrupt/unparseable sidecar → "not downloaded" outcome.
- Skip decision matrix:
  - file absent → download.
  - `updated_at` advanced → download (and assert the file is not hashed).
  - crc32c mismatch → download.
  - all match → skip.
- `sponsr.Post` unmarshals `updated_at` and `UpdatedAt()` returns it.
