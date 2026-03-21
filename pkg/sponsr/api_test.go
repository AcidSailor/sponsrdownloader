package sponsr

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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
