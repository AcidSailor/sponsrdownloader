package manager

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeItem struct {
	filename  string
	fullTitle string
	updatedAt time.Time
}

func (f fakeItem) URL() string      { return "http://example" }
func (f fakeItem) Filename() string { return f.filename }

// FullTitle falls back to filename so existing tests that only set
// filename still report a sensible title.
func (f fakeItem) FullTitle() string {
	if f.fullTitle != "" {
		return f.fullTitle
	}
	return f.filename
}
func (f fakeItem) IsAvailable() bool    { return true }
func (f fakeItem) UpdatedAt() time.Time { return f.updatedAt }

func TestDownloadSkipsIntactCurrentFile(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{outputPath: dir}
	item := fakeItem{
		filename:  "post",
		updatedAt: time.Date(2026, 6, 29, 18, 1, 55, 0, time.UTC),
	}

	calls := 0
	inner := func(_ context.Context, _ Downloadable, path string) error {
		calls++
		return os.WriteFile(path, []byte("data"), 0o644)
	}

	ctx := context.Background()

	// First run: downloads and writes a metadata.
	require.NoError(t, m.download(ctx, item, downloadReq{
		ext:          "pdf",
		downloadFunc: inner,
	}))
	assert.Equal(t, 1, calls)
	assert.FileExists(t,
		newFileMeta(time.Time{},
			filepath.Join(dir, "post.pdf")).metaPath())

	// Second run: intact + current -> skipped, inner not called.
	require.NoError(t, m.download(ctx, item, downloadReq{
		ext:          "pdf",
		downloadFunc: inner,
	}))
	assert.Equal(t, 1, calls)

	// Edited post: updated_at advances -> downloads again.
	item.updatedAt = item.updatedAt.Add(time.Hour)
	require.NoError(t, m.download(ctx, item, downloadReq{
		ext:          "pdf",
		downloadFunc: inner,
	}))
	assert.Equal(t, 2, calls)
}

// A failing inner download is wrapped with the ErrManager sentinel and
// leaves no metadata behind, so the next run retries.
func TestDownloadWrapsInnerErrorAndSkipsMeta(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{outputPath: dir}
	item := fakeItem{
		filename:  "post",
		updatedAt: time.Date(2026, 6, 29, 18, 1, 55, 0, time.UTC),
	}

	wantErr := errors.New("boom")
	inner := func(context.Context, Downloadable, string) error {
		return wantErr
	}

	err := m.download(context.Background(), item, downloadReq{
		ext:          "pdf",
		downloadFunc: inner,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrManager)
	assert.ErrorIs(t, err, wantErr)
	assert.NoFileExists(t,
		newFileMeta(time.Time{},
			filepath.Join(dir, "post.pdf")).metaPath())
}

// When inner produces no file (e.g. an unavailable item), no metadata is
// written and download still succeeds.
func TestDownloadWritesNoMetaWhenNoFileProduced(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{outputPath: dir}
	item := fakeItem{
		filename:  "post",
		updatedAt: time.Date(2026, 6, 29, 18, 1, 55, 0, time.UTC),
	}

	inner := func(context.Context, Downloadable, string) error { return nil }

	require.NoError(t,
		m.download(context.Background(), item, downloadReq{
			ext:          "pdf",
			downloadFunc: inner,
		}))
	assert.NoFileExists(t,
		newFileMeta(time.Time{},
			filepath.Join(dir, "post.pdf")).metaPath())
}

// A zero updated_at is not a trustworthy skip signal, so every run
// re-downloads even though an intact file and metadata exist.
func TestDownloadNeverSkipsWithoutUpdatedAt(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{outputPath: dir}
	item := fakeItem{filename: "post"} // zero updatedAt

	calls := 0
	inner := func(_ context.Context, _ Downloadable, path string) error {
		calls++
		return os.WriteFile(path, []byte("data"), 0o644)
	}
	ctx := context.Background()

	require.NoError(t, m.download(ctx, item, downloadReq{
		ext:          "pdf",
		downloadFunc: inner,
	}))
	require.NoError(t, m.download(ctx, item, downloadReq{
		ext:          "pdf",
		downloadFunc: inner,
	}))
	assert.Equal(t, 2, calls)
}

// A download func that reports success but writes an empty file must
// fail fast (wrapped with ErrManager) rather than caching garbage.
func TestDownloadFailsOnEmptyOutput(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{outputPath: dir}
	item := fakeItem{
		filename:  "post",
		updatedAt: time.Date(2026, 6, 29, 18, 1, 55, 0, time.UTC),
	}

	inner := func(_ context.Context, _ Downloadable, path string) error {
		return os.WriteFile(path, nil, 0o644)
	}

	err := m.download(context.Background(), item, downloadReq{
		ext:          "pdf",
		downloadFunc: inner,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrManager)
	assert.NoFileExists(t,
		newFileMeta(time.Time{},
			filepath.Join(dir, "post.pdf")).metaPath())
}

// The full, untruncated title is recorded in the sidecar JSON even when
// the on-disk filename is a length-capped stand-in, so a truncated file
// can be traced back to its post.
func TestDownloadRecordsFullTitle(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{outputPath: dir}
	item := fakeItem{
		filename:  "23-01-2026 - truncated stand-in",
		fullTitle: "23-01-2026 - the full untruncated post title",
		updatedAt: time.Date(2026, 1, 23, 0, 0, 0, 0, time.UTC),
	}

	inner := func(_ context.Context, _ Downloadable, path string) error {
		return os.WriteFile(path, []byte("data"), 0o644)
	}
	require.NoError(t, m.download(context.Background(), item, downloadReq{
		ext:          "pdf",
		downloadFunc: inner,
	}))

	metaPath := newFileMeta(time.Time{},
		filepath.Join(dir, item.filename+".pdf")).metaPath()
	data, err := os.ReadFile(metaPath)
	require.NoError(t, err)
	var got fileMeta
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, item.fullTitle, got.Title)
}

// Corrupt existing metadata must not cause a skip: upToDate returns an
// error, download logs it and proceeds to re-download.
func TestDownloadReDownloadsOnCorruptMeta(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{outputPath: dir}
	item := fakeItem{
		filename:  "post",
		updatedAt: time.Date(2026, 6, 29, 18, 1, 55, 0, time.UTC),
	}
	out := filepath.Join(dir, "post.pdf")
	require.NoError(t, os.WriteFile(out, []byte("data"), 0o644))
	fm := newFileMeta(time.Time{}, out)
	require.NoError(t, os.MkdirAll(filepath.Dir(fm.metaPath()), 0o755))
	require.NoError(t, os.WriteFile(fm.metaPath(), []byte("{bad"), 0o644))

	calls := 0
	inner := func(_ context.Context, _ Downloadable, path string) error {
		calls++
		return os.WriteFile(path, []byte("data"), 0o644)
	}

	require.NoError(t, m.download(context.Background(), item, downloadReq{
		ext:          "pdf",
		downloadFunc: inner,
	}))
	assert.Equal(t, 1, calls)
}
