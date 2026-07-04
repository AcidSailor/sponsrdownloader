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
