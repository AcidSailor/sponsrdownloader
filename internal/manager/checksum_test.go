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
