package manager

import (
	"context"
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
	updatedAt time.Time
}

func (f fakeItem) URL() string          { return "http://example" }
func (f fakeItem) Filename() string     { return f.filename }
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

// A failing inner download is wrapped with the ErrManager sentinel and
// leaves no sidecar behind, so the next run retries.
func TestDownloadWrapsInnerErrorAndSkipsSidecar(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{outputPath: dir}
	item := fakeItem{
		filename:  "post",
		updatedAt: time.Date(2026, 6, 29, 18, 1, 55, 0, time.UTC),
	}

	wantErr := errors.New("boom")
	inner := func(context.Context, Downloadable) error { return wantErr }

	err := m.download(context.Background(), item, "pdf", "PDF", inner)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrManager)
	assert.ErrorIs(t, err, wantErr)
	assert.NoFileExists(t,
		sidecarPath(filepath.Join(dir, "post.pdf")))
}

// When inner produces no file (e.g. an unavailable item), no sidecar is
// written and download still succeeds.
func TestDownloadWritesNoSidecarWhenNoFileProduced(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{outputPath: dir}
	item := fakeItem{
		filename:  "post",
		updatedAt: time.Date(2026, 6, 29, 18, 1, 55, 0, time.UTC),
	}

	inner := func(context.Context, Downloadable) error { return nil }

	require.NoError(t,
		m.download(context.Background(), item, "pdf", "PDF", inner))
	assert.NoFileExists(t,
		sidecarPath(filepath.Join(dir, "post.pdf")))
}

// A zero updated_at is not a trustworthy skip signal, so every run
// re-downloads even though an intact file and sidecar exist.
func TestDownloadNeverSkipsWithoutUpdatedAt(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{outputPath: dir}
	item := fakeItem{filename: "post"} // zero updatedAt

	calls := 0
	inner := func(_ context.Context, it Downloadable) error {
		calls++
		return os.WriteFile(
			filepath.Join(dir, it.Filename()+".pdf"),
			[]byte("data"), 0o644)
	}
	ctx := context.Background()

	require.NoError(t, m.download(ctx, item, "pdf", "PDF", inner))
	require.NoError(t, m.download(ctx, item, "pdf", "PDF", inner))
	assert.Equal(t, 2, calls)
}
