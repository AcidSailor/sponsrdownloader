package sponsr

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostUpdatedAt(t *testing.T) {
	const body = `{"updated_at":"2026-06-29T18:01:55.000Z"}`
	var p Post
	require.NoError(t, json.Unmarshal([]byte(body), &p))

	want := time.Date(2026, 6, 29, 18, 1, 55, 0, time.UTC)
	assert.True(t, p.UpdatedAt().Equal(want), "got %s", p.UpdatedAt())
}

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plain string unchanged",
			input: "Hello World",
			want:  "Hello World",
		},
		{
			name:  "removes unsafe chars",
			input: `file/name\with:all*of?"the<bad>chars|`,
			want:  "filenamewithallofthebadchars",
		},
		{
			name:  "collapses multiple spaces",
			input: "too   many    spaces",
			want:  "too many spaces",
		},
		{
			name:  "trims leading and trailing spaces",
			input: "  trimmed  ",
			want:  "trimmed",
		},
		{
			name:  "normalizes unicode whitespace",
			input: "non\u00a0breaking\u2009thin\u3000ideographic",
			want:  "non breaking thin ideographic",
		},
		{
			name:  "removes control characters",
			input: "clean\x00\x01\x1fme",
			want:  "cleanme",
		},
		{
			name:  "gopher in space",
			input: "🐹\u00a0Goes\u2009To\u3000Space:\u00a0the\u00a0*\u00a0final\u00a0?\u00a0frontier",
			want:  "🐹 Goes To Space the final frontier",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, sanitizeTitle(tt.input))
		})
	}
}

func TestFilenameBounded(t *testing.T) {
	// A real Crimson-style Cyrillic title: well under 255 runes but far
	// over 255 bytes, which previously overflowed NAME_MAX and made the
	// PDF and its .pdf.json checksum sidecar fail to write.
	longTitle := strings.Repeat("Судьбоносное совещание ", 20)
	p := Post{
		Date:  time.Date(2026, 1, 23, 0, 0, 0, 0, time.UTC),
		Title: longTitle,
	}

	name := p.Filename()

	// The name plus the longest suffix the manager appends must stay
	// within the filesystem limit.
	assert.LessOrEqual(t, len(name+".pdf.json"), nameMax)
	// Truncation must never split a rune.
	assert.True(t, utf8.ValidString(name), "filename must be valid UTF-8")
	// A cut mid-title must not leave a trailing space.
	assert.Equal(t, name, strings.TrimRight(name, " "))
	// The date prefix always survives.
	assert.True(t, strings.HasPrefix(name, "23-01-2026 - "))
}

func TestFilenameShortUnchanged(t *testing.T) {
	p := Post{
		Date:  time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Title: "про НПЗ и презентации",
	}
	assert.Equal(t, "01-07-2026 - про НПЗ и презентации", p.Filename())
}
