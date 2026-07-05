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
	assert.Equal(t, crc32c(want), got)
	assert.Equal(t, crc32c(0x9a71bb4c), got)
}

func TestMetaPath(t *testing.T) {
	fm := newFileMeta(time.Time{}, filepath.Join("out", "My Post.pdf"))
	want := filepath.Join("out", ".checksums", "My Post.pdf.json")
	assert.Equal(t, want, fm.metaPath())
}

func TestCRC32Unmarshal(t *testing.T) {
	t.Run("non-hex string -> error", func(t *testing.T) {
		var c crc32c
		assert.Error(t, c.UnmarshalJSON([]byte(`"zzzzzzzz"`)))
	})
	t.Run("json number -> error", func(t *testing.T) {
		var c crc32c
		assert.Error(t, c.UnmarshalJSON([]byte(`123`)))
	})
}

func TestFileMetaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "post.pdf")
	in := newFileMeta(
		time.Date(2026, 6, 29, 18, 1, 55, 0, time.UTC), out)
	in.CRC32 = 0x1a2b3c4d
	require.NoError(t, in.write())

	got := newFileMeta(time.Time{}, out)
	require.NoError(t, got.read())
	assert.True(t, got.UpdatedAt.Equal(in.UpdatedAt))
	assert.Equal(t, in.CRC32, got.CRC32)
}

// TestMetaJSONIsHex locks the on-disk format: crc32c serializes as
// eight-char lowercase hex, not as a decimal number.
func TestMetaJSONIsHex(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "post.pdf")
	fm := newFileMeta(
		time.Date(2026, 6, 29, 18, 1, 55, 0, time.UTC), out)
	fm.CRC32 = 0x1a2b3c4d
	require.NoError(t, fm.write())

	data, err := os.ReadFile(fm.metaPath())
	require.NoError(t, err)
	assert.Contains(t, string(data), `"crc32":"1a2b3c4d"`)
}

func TestReadMissing(t *testing.T) {
	dir := t.TempDir()
	fm := newFileMeta(time.Time{}, filepath.Join(dir, "nope.pdf"))
	assert.Error(t, fm.read())
}

func TestReadCorrupt(t *testing.T) {
	dir := t.TempDir()
	fm := newFileMeta(time.Time{}, filepath.Join(dir, "post.pdf"))
	require.NoError(t,
		os.MkdirAll(filepath.Dir(fm.metaPath()), 0o755))
	require.NoError(t,
		os.WriteFile(fm.metaPath(), []byte("{bad"), 0o644))

	assert.Error(t, fm.read())
}

// writeFileWithMeta writes an output file plus a metadata file recording
// a (possibly wrong) crc, and returns the output path.
func writeFileWithMeta(
	t *testing.T, dir, name, content string,
	updatedAt time.Time, crc crc32c,
) string {
	t.Helper()
	out := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(out, []byte(content), 0o644))
	fm := newFileMeta(updatedAt, out)
	fm.CRC32 = crc
	require.NoError(t, fm.write())
	return out
}

func TestFileMetaUpToDate(t *testing.T) {
	updated := time.Date(2026, 6, 29, 18, 1, 55, 0, time.UTC)
	later := updated.Add(time.Hour)

	t.Run("all match -> true", func(t *testing.T) {
		dir := t.TempDir()
		out := filepath.Join(dir, "post.pdf")
		require.NoError(t, os.WriteFile(out, []byte("data"), 0o644))
		crc, err := fileCRC32(out)
		require.NoError(t, err)
		out = writeFileWithMeta(t, dir, "post.pdf", "data",
			updated, crc)

		ok, err := newFileMeta(updated, out).upToDate()
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("file absent -> false", func(t *testing.T) {
		dir := t.TempDir()
		fm := newFileMeta(updated, filepath.Join(dir, "missing.pdf"))
		ok, err := fm.upToDate()
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("updated_at advanced -> false", func(t *testing.T) {
		dir := t.TempDir()
		out := filepath.Join(dir, "post.pdf")
		require.NoError(t, os.WriteFile(out, []byte("data"), 0o644))
		crc, err := fileCRC32(out)
		require.NoError(t, err)
		// Metadata crc is CORRECT; only the edit-time differs, so a
		// false result proves updated_at is checked first.
		out = writeFileWithMeta(t, dir, "post.pdf", "data",
			updated, crc)

		ok, err := newFileMeta(later, out).upToDate()
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("crc mismatch -> false", func(t *testing.T) {
		dir := t.TempDir()
		out := writeFileWithMeta(
			t, dir, "post.pdf", "data", updated, 0xdeadbeef)

		ok, err := newFileMeta(updated, out).upToDate()
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("missing metadata -> false", func(t *testing.T) {
		dir := t.TempDir()
		out := filepath.Join(dir, "post.pdf")
		require.NoError(t, os.WriteFile(out, []byte("data"), 0o644))

		ok, err := newFileMeta(updated, out).upToDate()
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("corrupt metadata -> error", func(t *testing.T) {
		dir := t.TempDir()
		out := filepath.Join(dir, "post.pdf")
		require.NoError(t, os.WriteFile(out, []byte("data"), 0o644))
		fm := newFileMeta(updated, out)
		require.NoError(t,
			os.MkdirAll(filepath.Dir(fm.metaPath()), 0o755))
		require.NoError(t,
			os.WriteFile(fm.metaPath(), []byte("{bad"), 0o644))

		ok, err := fm.upToDate()
		require.Error(t, err)
		assert.False(t, ok)
	})

	t.Run("zero updated_at -> false", func(t *testing.T) {
		dir := t.TempDir()
		out := filepath.Join(dir, "post.pdf")
		require.NoError(t, os.WriteFile(out, []byte("data"), 0o644))
		crc, err := fileCRC32(out)
		require.NoError(t, err)
		out = writeFileWithMeta(t, dir, "post.pdf", "data",
			time.Time{}, crc)

		ok, err := newFileMeta(time.Time{}, out).upToDate()
		require.NoError(t, err)
		assert.False(t, ok)
	})
}

func TestFileMetaRecord(t *testing.T) {
	updated := time.Date(2026, 6, 29, 18, 1, 55, 0, time.UTC)

	t.Run("real file -> writes matching metadata", func(t *testing.T) {
		dir := t.TempDir()
		out := filepath.Join(dir, "post.pdf")
		require.NoError(t, os.WriteFile(out, []byte("data"), 0o644))

		recorded, err := newFileMeta(updated, out).record()
		require.NoError(t, err)
		assert.True(t, recorded)

		wantCRC, err := fileCRC32(out)
		require.NoError(t, err)
		got := newFileMeta(time.Time{}, out)
		require.NoError(t, got.read())
		assert.True(t, got.UpdatedAt.Equal(updated))
		assert.Equal(t, wantCRC, got.CRC32)
	})

	t.Run("missing file -> not recorded, no error", func(t *testing.T) {
		dir := t.TempDir()
		fm := newFileMeta(updated, filepath.Join(dir, "gone.pdf"))
		recorded, err := fm.record()
		require.NoError(t, err)
		assert.False(t, recorded)
		assert.NoFileExists(t, fm.metaPath())
	})

	t.Run("empty file -> error, not recorded", func(t *testing.T) {
		dir := t.TempDir()
		out := filepath.Join(dir, "empty.pdf")
		require.NoError(t, os.WriteFile(out, nil, 0o644))
		fm := newFileMeta(updated, out)
		recorded, err := fm.record()
		assert.Error(t, err)
		assert.False(t, recorded)
		assert.NoFileExists(t, fm.metaPath())
	})
}
