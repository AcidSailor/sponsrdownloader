# Skip Already-Downloaded Files Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Skip re-downloading a post's PDF/video when an up-to-date, intact copy already exists on disk.

**Architecture:** Per-file JSON "sidecar" files under a `.checksums/` subdir record each output's `updated_at` (server edit-signal) and crc32c (local integrity). The exported `DownloadPDF`/`DownloadVideo` wrappers consult the sidecar before doing any Playwright/ffmpeg work and write a fresh sidecar after a successful download. No shared state, no mutex.

**Tech Stack:** Go 1.26, stdlib `hash/crc32` (Castagnoli), `encoding/json`, Playwright (unchanged), testify for tests.

## Global Constraints

- Go **1.26**; prefer modern idioms (generics, `min`, etc.).
- Lines **≤80 chars** — enforced by `golangci-lint fmt` (gofumpt + golines). `task ci` fails on any fmt diff.
- Logging via `log/slog` structured key/value pairs, never `fmt.Print`.
- Errors wrapped with `%w`; manager errors use the `ErrManager` sentinel.
- Tests use `github.com/stretchr/testify` (`assert`/`require`), table-driven where it fits existing style.
- Design source of truth: `docs/superpowers/specs/2026-07-04-skip-downloaded-files-design.md`.

---

### Task 1: Map `updated_at` onto `sponsr.Post`

**Files:**
- Modify: `pkg/sponsr/api.go` (add field + method to `Post`)
- Test: `pkg/sponsr/api_test.go` (add unmarshal test)

**Interfaces:**
- Consumes: nothing.
- Produces: `func (p *Post) UpdatedAt() time.Time` — the post's last-edit time, parsed from the `updated_at` JSON field. Later tasks and the `Downloadable` interface rely on this exact signature.

Naming follows the existing `Available bool` field + `IsAvailable() bool` method pattern: the struct field is `Updated`, the method is `UpdatedAt()`.

- [ ] **Step 1: Write the failing test**

Add to `pkg/sponsr/api_test.go`:

```go
func TestPostUpdatedAt(t *testing.T) {
	const body = `{"updated_at":"2026-06-29T18:01:55.000Z"}`
	var p Post
	require.NoError(t, json.Unmarshal([]byte(body), &p))

	want := time.Date(2026, 6, 29, 18, 1, 55, 0, time.UTC)
	assert.True(t, p.UpdatedAt().Equal(want), "got %s", p.UpdatedAt())
}
```

Add the needed imports to the test file's import block:

```go
import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/sponsr/ -run TestPostUpdatedAt -v`
Expected: FAIL — compile error, `p.UpdatedAt` undefined.

- [ ] **Step 3: Add the field and method**

In `pkg/sponsr/api.go`, add the field to the `Post` struct (below `Date`):

```go
type Post struct {
	ID            int       `json:"id"`
	ProjectID     int       `json:"project_id"`
	Date          time.Time `json:"date"`
	Updated       time.Time `json:"updated_at"`
	Title         string    `json:"title"`
	Available     bool      `json:"available"`
	DurationVideo int       `json:"duration_video"`
}
```

Add the method near `IsAvailable`:

```go
func (p *Post) UpdatedAt() time.Time {
	return p.Updated
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/sponsr/ -run TestPostUpdatedAt -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/sponsr/api.go pkg/sponsr/api_test.go
git commit -m "feat(sponsr): map updated_at onto Post"
```

---

### Task 2: Checksum + sidecar primitives

**Files:**
- Create: `internal/manager/checksum.go`
- Test: `internal/manager/checksum_test.go`

**Interfaces:**
- Consumes: nothing (Playwright-independent).
- Produces:
  - `type sidecar struct { UpdatedAt time.Time; CRC32 string }` with JSON tags `updated_at` / `crc32`.
  - `func fileCRC32(path string) (string, error)` — hex crc32c (Castagnoli) of the file's bytes, zero-padded to 8 chars.
  - `func sidecarPath(outputPath string) string` — maps `dir/name.pdf` → `dir/.checksums/name.pdf.json`.
  - `func readSidecar(outputPath string) (sidecar, error)` — reads+parses the sidecar.
  - `func writeSidecar(outputPath string, s sidecar) error` — atomically writes the sidecar (creating `.checksums/`).

- [ ] **Step 1: Write the failing tests**

Create `internal/manager/checksum_test.go`:

```go
package manager

import (
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileCRC32(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	require.NoError(t, os.WriteFile(path, []byte("hello"), 0o644))

	want := crc32.Checksum([]byte("hello"),
		crc32.MakeTable(crc32.Castagnoli))

	got, err := fileCRC32(path)
	require.NoError(t, err)
	assert.Equal(t, "9a71bb4c", got)
	assert.Equal(t, want, uint32(0x9a71bb4c))
}

func TestSidecarPath(t *testing.T) {
	got := sidecarPath(filepath.Join("out", "My Post.pdf"))
	want := filepath.Join("out", ".checksums", "My Post.pdf.json")
	assert.Equal(t, want, got)
}

func TestSidecarRoundTrip(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "post.pdf")
	in := sidecar{
		UpdatedAt: time.Date(2026, 6, 29, 18, 1, 55, 0, time.UTC),
		CRC32:     "1a2b3c4d",
	}
	require.NoError(t, writeSidecar(out, in))

	got, err := readSidecar(out)
	require.NoError(t, err)
	assert.True(t, got.UpdatedAt.Equal(in.UpdatedAt))
	assert.Equal(t, in.CRC32, got.CRC32)
}

func TestReadSidecarMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := readSidecar(filepath.Join(dir, "nope.pdf"))
	assert.Error(t, err)
}

func TestReadSidecarCorrupt(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "post.pdf")
	require.NoError(t,
		os.MkdirAll(filepath.Dir(sidecarPath(out)), 0o755))
	require.NoError(t,
		os.WriteFile(sidecarPath(out), []byte("{bad"), 0o644))

	_, err := readSidecar(out)
	assert.Error(t, err)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/manager/ -run 'CRC32|Sidecar' -v`
Expected: FAIL — compile error, undefined `fileCRC32`/`sidecar`/etc.

- [ ] **Step 3: Implement the primitives**

Create `internal/manager/checksum.go`:

```go
package manager

import (
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"time"
)

// checksumDir is the subfolder (inside the output folder) that holds
// one sidecar per downloaded file.
const checksumDir = ".checksums"

// crcTable is the Castagnoli (crc32c) table, hardware-accelerated on
// amd64 and arm64. Built once, reused for every checksum.
var crcTable = crc32.MakeTable(crc32.Castagnoli)

// sidecar records what we know about one downloaded file: the post's
// edit time (server change-signal) and the file's crc32c (integrity).
type sidecar struct {
	UpdatedAt time.Time `json:"updated_at"`
	CRC32     string    `json:"crc32"`
}

// fileCRC32 returns the hex crc32c of the file at path, zero-padded to
// eight characters.
func fileCRC32(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	h := crc32.New(crcTable)
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%08x", h.Sum32()), nil
}

// sidecarPath maps an output file path to its sidecar path, e.g.
// dir/name.pdf -> dir/.checksums/name.pdf.json.
func sidecarPath(outputPath string) string {
	dir := filepath.Dir(outputPath)
	base := filepath.Base(outputPath)
	return filepath.Join(dir, checksumDir, base+".json")
}

// readSidecar reads and parses the sidecar for outputPath.
func readSidecar(outputPath string) (sidecar, error) {
	var s sidecar
	data, err := os.ReadFile(sidecarPath(outputPath))
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, err
	}
	return s, nil
}

// writeSidecar atomically writes s as the sidecar for outputPath,
// creating the .checksums/ directory as needed.
func writeSidecar(outputPath string, s sidecar) error {
	path := sidecarPath(outputPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/manager/ -run 'CRC32|Sidecar' -v`
Expected: PASS (all five).

- [ ] **Step 5: Commit**

```bash
git add internal/manager/checksum.go internal/manager/checksum_test.go
git commit -m "feat(manager): crc32c + sidecar read/write primitives"
```

---

### Task 3: Skip decision (`alreadyDownloaded`)

**Files:**
- Modify: `internal/manager/checksum.go` (add `alreadyDownloaded`)
- Test: `internal/manager/checksum_test.go` (add decision-matrix tests)

**Interfaces:**
- Consumes: `sidecar`, `fileCRC32`, `readSidecar`, `writeSidecar`, `sidecarPath` (Task 2).
- Produces: `func alreadyDownloaded(outputPath string, updatedAt time.Time) (bool, error)` — true only when the file exists AND its sidecar `updated_at` equals `updatedAt` AND its crc32c matches. Missing file / missing / stale / corrupt sidecar → `(false, nil)`. A file that exists but can't be hashed → `(false, err)`. Cheap→expensive: the `updated_at` compare short-circuits before the file is hashed.

- [ ] **Step 1: Write the failing tests**

Add to `internal/manager/checksum_test.go`:

```go
func writeFileWithSidecar(
	t *testing.T, dir, name, content string,
	updatedAt time.Time, crc string,
) string {
	t.Helper()
	out := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(out, []byte(content), 0o644))
	require.NoError(t, writeSidecar(out, sidecar{
		UpdatedAt: updatedAt,
		CRC32:     crc,
	}))
	return out
}

func TestAlreadyDownloaded(t *testing.T) {
	updated := time.Date(2026, 6, 29, 18, 1, 55, 0, time.UTC)
	later := updated.Add(time.Hour)

	t.Run("all match -> true", func(t *testing.T) {
		dir := t.TempDir()
		out := filepath.Join(dir, "post.pdf")
		require.NoError(t,
			os.WriteFile(out, []byte("data"), 0o644))
		crc, err := fileCRC32(out)
		require.NoError(t, err)
		require.NoError(t, writeSidecar(out,
			sidecar{UpdatedAt: updated, CRC32: crc}))

		ok, err := alreadyDownloaded(out, updated)
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("file absent -> false", func(t *testing.T) {
		dir := t.TempDir()
		ok, err := alreadyDownloaded(
			filepath.Join(dir, "missing.pdf"), updated)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("updated_at advanced -> false", func(t *testing.T) {
		dir := t.TempDir()
		out := filepath.Join(dir, "post.pdf")
		require.NoError(t,
			os.WriteFile(out, []byte("data"), 0o644))
		crc, err := fileCRC32(out)
		require.NoError(t, err)
		// Sidecar crc is CORRECT; only the edit-time differs, so a
		// false result proves updated_at is checked first.
		require.NoError(t, writeSidecar(out,
			sidecar{UpdatedAt: updated, CRC32: crc}))

		ok, err := alreadyDownloaded(out, later)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("crc mismatch -> false", func(t *testing.T) {
		dir := t.TempDir()
		out := writeFileWithSidecar(
			t, dir, "post.pdf", "data", updated, "deadbeef")

		ok, err := alreadyDownloaded(out, updated)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("missing sidecar -> false", func(t *testing.T) {
		dir := t.TempDir()
		out := filepath.Join(dir, "post.pdf")
		require.NoError(t,
			os.WriteFile(out, []byte("data"), 0o644))

		ok, err := alreadyDownloaded(out, updated)
		require.NoError(t, err)
		assert.False(t, ok)
	})
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/manager/ -run TestAlreadyDownloaded -v`
Expected: FAIL — compile error, undefined `alreadyDownloaded`.

- [ ] **Step 3: Implement `alreadyDownloaded`**

Append to `internal/manager/checksum.go`:

```go
// alreadyDownloaded reports whether outputPath already holds a good,
// current copy: it exists, its sidecar updated_at matches updatedAt,
// and its crc32c matches the recorded checksum. Any mismatch, a
// missing file, or a missing/corrupt sidecar yields false so the
// caller re-downloads. The updated_at check runs before the file is
// hashed, so an edited post is settled without reading the file.
func alreadyDownloaded(
	outputPath string,
	updatedAt time.Time,
) (bool, error) {
	if _, err := os.Stat(outputPath); err != nil {
		return false, nil
	}
	s, err := readSidecar(outputPath)
	if err != nil {
		return false, nil
	}
	if !s.UpdatedAt.Equal(updatedAt) {
		return false, nil
	}
	crc, err := fileCRC32(outputPath)
	if err != nil {
		return false, err
	}
	return crc == s.CRC32, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/manager/ -run TestAlreadyDownloaded -v`
Expected: PASS (all five subtests).

- [ ] **Step 5: Commit**

```bash
git add internal/manager/checksum.go internal/manager/checksum_test.go
git commit -m "feat(manager): add already-downloaded skip decision"
```

---

### Task 4: Wire skip + sidecar-write into the download wrappers

**Files:**
- Modify: `internal/manager/manager.go` (extend `Downloadable`, add shared `download` helper, rewrite `DownloadPDF`/`DownloadVideo`)
- Test: `internal/manager/download_test.go` (new — Playwright-free helper test)

**Interfaces:**
- Consumes: `alreadyDownloaded`, `fileCRC32`, `writeSidecar`, `sidecar` (Tasks 2–3); `(*sponsr.Post).UpdatedAt()` (Task 1); the existing inner methods `downloadPDF`/`downloadVideo` (unchanged).
- Produces: `Downloadable` now includes `UpdatedAt() time.Time`; a private `func (m *Manager) download(ctx, item, ext, label string, inner func(context.Context, Downloadable) error) error` that skips, downloads, then records the sidecar.

- [ ] **Step 1: Write the failing test**

Create `internal/manager/download_test.go`:

```go
package manager

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeItem struct {
	filename  string
	updatedAt time.Time
}

func (f fakeItem) URL() string        { return "http://example" }
func (f fakeItem) Filename() string   { return f.filename }
func (f fakeItem) IsAvailable() bool  { return true }
func (f fakeItem) UpdatedAt() time.Time { return f.updatedAt }

func TestDownloadSkipsIntactCurrentFile(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{projectTitle: dir}
	item := fakeItem{
		filename:  "post",
		updatedAt: time.Date(2026, 6, 29, 18, 1, 55, 0, time.UTC),
	}

	calls := 0
	inner := func(_ context.Context, it Downloadable) error {
		calls++
		return os.WriteFile(
			filepath.Join(dir, it.Filename()+".pdf"),
			[]byte("data"), 0o644)
	}

	ctx := context.Background()

	// First run: downloads and writes a sidecar.
	require.NoError(t, m.download(ctx, item, "pdf", "PDF", inner))
	assert.Equal(t, 1, calls)
	assert.FileExists(t,
		sidecarPath(filepath.Join(dir, "post.pdf")))

	// Second run: intact + current -> skipped, inner not called.
	require.NoError(t, m.download(ctx, item, "pdf", "PDF", inner))
	assert.Equal(t, 1, calls)

	// Edited post: updated_at advances -> downloads again.
	item.updatedAt = item.updatedAt.Add(time.Hour)
	require.NoError(t, m.download(ctx, item, "pdf", "PDF", inner))
	assert.Equal(t, 2, calls)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/manager/ -run TestDownloadSkips -v`
Expected: FAIL — compile error, `m.download` undefined / `fakeItem` not accepted as `Downloadable` until the interface gains `UpdatedAt()`.

- [ ] **Step 3: Extend the interface**

In `internal/manager/manager.go`, add `UpdatedAt()` to `Downloadable`:

```go
type Downloadable interface {
	URL() string
	Filename() string
	IsAvailable() bool
	UpdatedAt() time.Time
}
```

(`time` is already imported in this file. The existing
`var _ Downloadable = (*sponsr.Post)(nil)` assertion now also enforces
`UpdatedAt()`, satisfied by Task 1.)

- [ ] **Step 4: Add the shared `download` helper**

In `internal/manager/manager.go`, add this method (place it just above
`DownloadPDF`):

```go
// download runs the skip-check, the inner downloader, and the sidecar
// write for one item. ext is the output extension ("pdf"/"mp4") and
// label is the human noun used in logs/errors ("PDF"/"video").
func (m *Manager) download(
	ctx context.Context,
	item Downloadable,
	ext, label string,
	inner func(context.Context, Downloadable) error,
) error {
	outputPath := filepath.Join(
		m.projectTitle, item.Filename()+"."+ext)
	logger := slog.With("filename", item.Filename())

	done, err := alreadyDownloaded(outputPath, item.UpdatedAt())
	if err != nil {
		logger.Warn("could not check existing download", "error", err)
	}
	if done {
		logger.Info("skipped, already downloaded")
		return nil
	}

	if err := inner(ctx, item); err != nil {
		return fmt.Errorf(
			"%w: %s %q: %w", ErrManager, label, item.Filename(), err)
	}

	// Record the checksum only when a file was actually produced;
	// unavailable items are skipped by inner and write nothing.
	if _, statErr := os.Stat(outputPath); statErr == nil {
		crc, crcErr := fileCRC32(outputPath)
		if crcErr != nil {
			logger.Warn("could not checksum download", "error", crcErr)
		} else if wErr := writeSidecar(outputPath, sidecar{
			UpdatedAt: item.UpdatedAt(),
			CRC32:     crc,
		}); wErr != nil {
			logger.Warn("could not write checksum", "error", wErr)
		}
	}

	logger.Info("downloaded " + label)
	return nil
}
```

- [ ] **Step 5: Rewrite the two public wrappers**

Replace the existing `DownloadPDF` and `DownloadVideo` methods in
`internal/manager/manager.go` with:

```go
func (m *Manager) DownloadPDF(
	ctx context.Context,
	item Downloadable,
) error {
	return m.download(ctx, item, "pdf", "PDF", m.downloadPDF)
}

func (m *Manager) DownloadVideo(
	ctx context.Context,
	item Downloadable,
) error {
	return m.download(ctx, item, "mp4", "video", m.downloadVideo)
}
```

The inner `downloadPDF`/`downloadVideo` methods and their file-path
construction stay exactly as they are.

- [ ] **Step 6: Run the new test to verify it passes**

Run: `go test ./internal/manager/ -run TestDownloadSkips -v`
Expected: PASS.

- [ ] **Step 7: Run the full suite + fmt/lint**

Run: `go test ./...`
Expected: PASS (all packages).

Run: `task ci`
Expected: no fmt diff, lint clean. If `task ci` reports fmt issues, run
`task lint` (autofix), re-inspect, and re-run `task ci`.

- [ ] **Step 8: Commit**

```bash
git add internal/manager/manager.go internal/manager/download_test.go
git commit -m "feat(manager): skip re-download of intact, current files"
```

---

## Self-Review

**Spec coverage:**
- Two-signal skip (`updated_at` + crc32c) → Task 3 `alreadyDownloaded`. ✓
- crc32c Castagnoli, stdlib → Task 2 `fileCRC32`/`crcTable`. ✓
- Per-file sidecars under `.checksums/`, atomic write → Task 2 `sidecarPath`/`writeSidecar`. ✓
- Cheap→expensive ordering (updated_at before hashing) → Task 3, asserted by "updated_at advanced -> false" with a correct crc. ✓
- `updated_at` from API onto `Post` → Task 1. ✓
- `Downloadable.UpdatedAt()` interface addition → Task 4. ✓
- Skip in exported wrappers, inners untouched, `cmd` untouched → Task 4. ✓
- Error handling: sidecar read failure → download; write failure → warn+continue → Task 3 (`false,nil`) + Task 4 (warn branches). ✓
- Unavailable items write no sidecar → Task 4 `os.Stat` guard. ✓
- Tests are Playwright-free → Tasks 2–4 all use temp dirs / fakes. ✓

**Placeholder scan:** No TBD/TODO; every code step has complete code. ✓

**Type consistency:** `sidecar{UpdatedAt, CRC32}`, `fileCRC32`, `sidecarPath`, `readSidecar`, `writeSidecar`, `alreadyDownloaded`, `download(ctx,item,ext,label,inner)`, `UpdatedAt() time.Time` are used identically across tasks. `Updated` field vs `UpdatedAt()` method distinction is intentional (mirrors `Available`/`IsAvailable()`). ✓
