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

// fileMeta is the metadata sidecar for one downloaded file: the post's
// edit time (server change-signal) and the file's crc32c (integrity).
// path is the output file it describes; the sidecar itself lives beside
// that file under .checksums/ and is not serialized.
type fileMeta struct {
	UpdatedAt time.Time `json:"updated_at"`
	CRC32     crc32c    `json:"crc32"`
	path      string
}

// newFileMeta describes the output file at outputPath as of updatedAt.
// The checksum is filled in later by record.
func newFileMeta(updatedAt time.Time, outputPath string) *fileMeta {
	return &fileMeta{UpdatedAt: updatedAt, path: outputPath}
}

// sidecarPath maps the output file path to its sidecar path, e.g.
// dir/name.pdf -> dir/.checksums/name.pdf.json.
func (fm *fileMeta) sidecarPath() string {
	dir := filepath.Dir(fm.path)
	base := filepath.Base(fm.path)
	return filepath.Join(dir, checksumDir, base+".json")
}

// read loads the sidecar for fm.path into fm, leaving fm.path intact.
func (fm *fileMeta) read() error {
	data, err := os.ReadFile(fm.sidecarPath())
	if err != nil {
		return err
	}
	return json.Unmarshal(data, fm)
}

// write persists fm as the sidecar for fm.path via a temp file plus
// atomic rename, so a reader never sees a partial sidecar. It creates
// the .checksums/ directory as needed.
func (fm *fileMeta) write() error {
	path := fm.sidecarPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(fm)
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

// upToDate reports whether fm.path already holds a good, current copy
// for fm.UpdatedAt: the file exists, its sidecar updated_at matches, and
// its crc32c matches the recorded checksum. A missing file or a missing
// sidecar yields (false, nil) so the caller re-downloads; other errors
// (unreadable file, corrupt sidecar) are returned so the caller can log
// them. A zero UpdatedAt is treated as "no edit signal" and always
// yields false. The updated_at check runs before the file is hashed, so
// an edited post is settled without reading the file.
func (fm *fileMeta) upToDate() (bool, error) {
	if fm.UpdatedAt.IsZero() {
		return false, nil
	}
	stored := fileMeta{path: fm.path}
	if err := stored.read(); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if !stored.UpdatedAt.Equal(fm.UpdatedAt) {
		return false, nil
	}
	crc, err := fileCRC32(fm.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return crc == stored.CRC32, nil
}

// record hashes fm.path and writes fm (stamped with that checksum) as
// its sidecar. A missing file is not an error — an unavailable item is
// skipped by the download func and produces nothing to record. A
// zero-byte file is a download that failed without erroring and is
// reported so the caller can fail fast rather than caching garbage. The
// file is opened once for both the size check and the hash.
func (fm *fileMeta) record() error {
	f, err := os.Open(fm.path)
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
	fm.CRC32 = crc
	return fm.write()
}
