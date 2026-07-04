package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// checksumDir is the subfolder (inside the output folder) that holds
// one sidecar per downloaded file.
const checksumDir = ".checksums"

// crcTable is the Castagnoli (crc32c) table. Go's crc32 update uses the
// CPU CRC instructions when available (SSE4.2 on amd64, the CRC
// extension on arm64) and falls back to software otherwise. Built once,
// reused for every checksum.
var crcTable = crc32.MakeTable(crc32.Castagnoli)

// crc32c is a crc32 Castagnoli checksum. In memory it is the raw
// uint32, so checksums are compared as integers; on disk (JSON) it is
// the eight-char lowercase hex form. Keeping the hex format in this
// one place stops a stray format string from silently breaking every
// comparison.
type crc32c uint32

func (c crc32c) MarshalJSON() ([]byte, error) {
	return json.Marshal(fmt.Sprintf("%08x", uint32(c)))
}

func (c *crc32c) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return err
	}
	*c = crc32c(v)
	return nil
}

// sidecar records what we know about one downloaded file: the post's
// edit time (server change-signal) and the file's crc32c (integrity).
type sidecar struct {
	UpdatedAt time.Time `json:"updated_at"`
	CRC32     crc32c    `json:"crc32"`
}

// fileCRC32 returns the crc32c of the file at path.
func fileCRC32(path string) (crc32c, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()

	h := crc32.New(crcTable)
	if _, err := io.Copy(h, f); err != nil {
		return 0, err
	}
	return crc32c(h.Sum32()), nil
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

// writeSidecar writes s as the sidecar for outputPath via a temp file
// plus atomic rename, so a reader never sees a partial sidecar. It
// creates the .checksums/ directory as needed.
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
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// alreadyDownloaded reports whether outputPath already holds a good,
// current copy: it exists, its sidecar updated_at matches updatedAt,
// and its crc32c matches the recorded checksum. A missing file or a
// missing sidecar yields (false, nil) so the caller re-downloads;
// other errors (unreadable file, corrupt sidecar) are returned so the
// caller can log them. A zero updatedAt is treated as "no edit signal"
// and always yields false. The updated_at check runs before the file
// is hashed, so an edited post is settled without reading the file.
func alreadyDownloaded(
	outputPath string,
	updatedAt time.Time,
) (bool, error) {
	if updatedAt.IsZero() {
		return false, nil
	}
	if _, err := os.Stat(outputPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	s, err := readSidecar(outputPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
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

// recordChecksum hashes the file at outputPath and writes its sidecar,
// stamping it with updatedAt. It records only when a real file was
// produced: a missing file (unavailable item skipped by the inner
// downloader) or a zero-byte file (a download that failed without
// erroring) is left unrecorded so the next run retries. Every failure
// is logged and swallowed — the download itself already succeeded.
func recordChecksum(
	logger *slog.Logger,
	outputPath string,
	updatedAt time.Time,
) {
	info, err := os.Stat(outputPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return
	case err != nil:
		logger.Warn("could not stat download to checksum", "error", err)
		return
	case info.Size() == 0:
		logger.Warn("download produced an empty file; not recording")
		return
	}

	crc, err := fileCRC32(outputPath)
	if err != nil {
		logger.Warn("could not checksum download", "error", err)
		return
	}
	if err := writeSidecar(outputPath, sidecar{
		UpdatedAt: updatedAt,
		CRC32:     crc,
	}); err != nil {
		logger.Warn("could not write checksum", "error", err)
	}
}
