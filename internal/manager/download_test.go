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
