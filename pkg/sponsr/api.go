package sponsr

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	Domain      = "sponsr.ru"
	Endpoint    = "https://" + Domain
	ApiEndpoint = Endpoint + "/api/v2"
	// PostsPath and ProjectsPath are relative to ApiEndpoint (the restkit
	// client's base URL).
	PostsPath    = "/content/posts"
	ProjectsPath = "/content/projects"
)

type Objects[T any] struct {
	Total int `json:"total"`
	List  []T `json:"list"`
	Page  int `json:"page"`
	Limit int `json:"limit"`
}

type Posts Objects[Post]

type Projects Objects[Project]

type Post struct {
	ID            int       `json:"id"`
	ProjectID     int       `json:"project_id"`
	Date          time.Time `json:"date"`
	Updated       time.Time `json:"updated_at"`
	Title         string    `json:"title"`
	Available     bool      `json:"available"`
	DurationVideo int       `json:"duration_video"`
}

func (p *Post) String() string {
	return p.Title
}

func (p *Post) URL() string {
	return fmt.Sprintf(
		"%s/%d/%d",
		Endpoint,
		p.ProjectID,
		p.ID,
	)
}

func (p *Post) IsAvailable() bool {
	return p.Available
}

func (p *Post) UpdatedAt() time.Time {
	return p.Updated
}

var reMultiSpace = regexp.MustCompile(`\s{2,}`)

func sanitizeTitle(s string) string {
	// normalize all unicode whitespace to regular space
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, s)
	// remove characters unsafe on any OS: / \ : * ? " < > |
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\:*?"<>|`, r) {
			return -1
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	// collapse multiple spaces, trim
	s = reMultiSpace.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	return s
}

// nameMax is the maximum length, in bytes, of a single path component
// on the filesystems we target (ext4, APFS, HFS+ all cap a name at 255
// bytes). maxSuffixBytes is the longest extension chain the manager
// appends to a Filename() result: the checksum sidecar is
// "<name>.pdf.json" / "<name>.mp4.json" (9 bytes), which is longer than
// the ".pdf"/".mp4" output file itself. Reserving that headroom here
// keeps every derived path within the limit. This matters for UTF-8
// titles: Cyrillic runs ~2 bytes/rune, so a ~120-rune title already
// overflows even though it is well under 255 characters.
const (
	nameMax          = 255
	maxSuffixBytes   = len(".pdf.json")
	maxFilenameBytes = nameMax - maxSuffixBytes
)

// truncateToBytes returns the longest prefix of s that fits in n bytes
// without splitting a UTF-8 rune, with any trailing space trimmed so a
// cut mid-title does not leave a dangling separator.
func truncateToBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Back up off any continuation bytes so the prefix ends on a rune
	// boundary (s[:n] is valid iff s[n] starts a rune or n == len(s)).
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return strings.TrimRight(s[:n], " ")
}

func (p *Post) Filename() string {
	name := fmt.Sprintf(
		"%s - %s",
		p.Date.Format("02-01-2006"),
		sanitizeTitle(p.Title),
	)
	return truncateToBytes(name, maxFilenameBytes)
}

type Project struct {
	ID           int    `json:"id"`
	ProjectTitle string `json:"project_title"`
}

func (p *Project) String() string {
	return p.ProjectTitle
}

func ProjectPageURL(slug string) string {
	return fmt.Sprintf("%s/%s/", Endpoint, slug)
}
