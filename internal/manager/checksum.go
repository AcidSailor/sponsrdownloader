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
// one metadata file per downloaded file.
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

// metaPathFor maps an output file path to its metadata file path, e.g.
// dir/name.pdf -> dir/.checksums/name.pdf.json.
func metaPathFor(pathToFile string) string {
	dir := filepath.Dir(pathToFile)
	base := filepath.Base(pathToFile)
	return filepath.Join(dir, checksumDir, base+".json")
}

// fileMeta bundles one output file's path with what we record about it:
// the post's edit time (server change-signal) and the file's crc32c
// (integrity). The metadata is stored as JSON at pathToMeta, beside the
// output under .checksums/. pathToMeta is a *string so a fileMeta built
// only to read existing metadata can share a known path without
// recomputing it; nil means "derive it from pathToFile".
type fileMeta struct {
	UpdatedAt  time.Time `json:"updated_at"`
	CRC32      crc32c    `json:"crc32"`
	pathToFile string
	pathToMeta *string
}

// newFileMeta describes the output file at pathToFile as of updatedAt.
// The checksum is filled in later by record.
func newFileMeta(updatedAt time.Time, pathToFile string) *fileMeta {
	mp := metaPathFor(pathToFile)
	return &fileMeta{
		UpdatedAt:  updatedAt,
		pathToFile: pathToFile,
		pathToMeta: &mp,
	}
}

// metaPath returns fm's metadata file path, deriving it from pathToFile
// when pathToMeta is unset.
func (fm *fileMeta) metaPath() string {
	if fm.pathToMeta == nil {
		mp := metaPathFor(fm.pathToFile)
		fm.pathToMeta = &mp
	}
	return *fm.pathToMeta
}

// read loads fm's metadata file into fm, leaving fm's paths intact.
func (fm *fileMeta) read() error {
	data, err := os.ReadFile(fm.metaPath())
	if err != nil {
		return err
	}
	return json.Unmarshal(data, fm)
}

// write persists fm's metadata to fm.metaPath (beside the output file,
// under .checksums/), creating that directory as needed.
func (fm *fileMeta) write() error {
	path := fm.metaPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(fm)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// upToDate reports whether fm.pathToFile already holds a good, current
// copy for fm.UpdatedAt: the file exists, its recorded updated_at
// matches, and its crc32c matches the recorded checksum. A missing file
// or a missing metadata file yields (false, nil) so the caller
// re-downloads; other errors (unreadable file, corrupt metadata) are
// returned so the caller can log them. A zero UpdatedAt is treated as
// "no edit signal" and always yields false. The updated_at check runs
// before the file is hashed, so an edited post is settled without
// reading the file.
func (fm *fileMeta) upToDate() (bool, error) {
	if fm.UpdatedAt.IsZero() {
		return false, nil
	}
	// Share fm's metadata path so the on-disk record is found without
	// recomputing (or losing) it.
	stored := fileMeta{pathToMeta: fm.pathToMeta}
	if err := stored.read(); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if !stored.UpdatedAt.Equal(fm.UpdatedAt) {
		return false, nil
	}
	crc, err := fileCRC32(fm.pathToFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return crc == stored.CRC32, nil
}

// record hashes fm.pathToFile and writes fm (stamped with that
// checksum) as its metadata file. It reports whether a metadata file
// was written: false with a nil error when the output file is absent —
// an unavailable item is skipped by the download func and produces
// nothing to record. A zero-byte file is a download that failed without
// erroring and is reported so the caller can fail fast rather than
// caching garbage. The file is opened once for both the size check and
// the hash.
func (fm *fileMeta) record() (bool, error) {
	f, err := os.Open(fm.pathToFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if info.Size() == 0 {
		return false, errors.New("download produced an empty file")
	}

	crc, err := crcOf(f)
	if err != nil {
		return false, err
	}
	fm.CRC32 = crc
	if err := fm.write(); err != nil {
		return false, err
	}
	return true, nil
}
