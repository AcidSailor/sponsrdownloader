package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
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

// crcOf returns the crc32c of everything read from r.
func crcOf(r io.Reader) (crc32c, error) {
	h := crc32.New(crcTable)
	if _, err := io.Copy(h, r); err != nil {
		return 0, err
	}
	return crc32c(h.Sum32()), nil
}

// fileCRC32 returns the crc32c of the file at path.
func fileCRC32(path string) (crc32c, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	return crcOf(f)
}

// sidecar records what we know about one downloaded file: the post's
// edit time (server change-signal) and the file's crc32c (integrity).
type sidecar struct {
	UpdatedAt time.Time `json:"updated_at"`
	CRC32     crc32c    `json:"crc32"`
}

// sidecarPath maps an output file path to its sidecar path, e.g.
// dir/name.pdf -> dir/.checksums/name.pdf.json.
func sidecarPath(outputPath string) string {
	dir := filepath.Dir(outputPath)
	base := filepath.Base(outputPath)
	return filepath.Join(dir, checksumDir, base+".json")
}

// read loads the sidecar for outputPath into s.
func (s *sidecar) read(outputPath string) error {
	data, err := os.ReadFile(sidecarPath(outputPath))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, s)
}

// write persists s as the sidecar for outputPath via a temp file plus
// atomic rename, so a reader never sees a partial sidecar. It creates
// the .checksums/ directory as needed.
func (s sidecar) write(outputPath string) error {
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

// isCurrent reports whether outputPath already holds a good, current
// copy for the receiver's UpdatedAt: the file exists, its sidecar
// updated_at matches, and its crc32c matches the recorded checksum. A
// missing file or a missing sidecar yields (false, nil) so the caller
// re-downloads; other errors (unreadable file, corrupt sidecar) are
// returned so the caller can log them. A zero UpdatedAt is treated as
// "no edit signal" and always yields false. The updated_at check runs
// before the file is hashed, so an edited post is settled without
// reading the file.
func (s sidecar) isCurrent(outputPath string) (bool, error) {
	if s.UpdatedAt.IsZero() {
		return false, nil
	}
	var stored sidecar
	if err := stored.read(outputPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if !stored.UpdatedAt.Equal(s.UpdatedAt) {
		return false, nil
	}
	crc, err := fileCRC32(outputPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return crc == stored.CRC32, nil
}

// record hashes the file at outputPath and writes s (stamped with that
// checksum) as its sidecar. A missing file is not an error — an
// unavailable item is skipped by the inner downloader and produces
// nothing to record. A zero-byte file is a download that failed
// without erroring and is reported so the caller can fail fast rather
// than caching garbage. The file is opened once for both the size
// check and the hash.
func (s sidecar) record(outputPath string) error {
	f, err := os.Open(outputPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return errors.New("download produced an empty file")
	}

	crc, err := crcOf(f)
	if err != nil {
		return err
	}
	s.CRC32 = crc
	return s.write(outputPath)
}
