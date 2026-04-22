package sponsr

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

const (
	Domain           = "sponsr.ru"
	Endpoint         = "https://" + Domain
	ApiEndpoint      = Endpoint + "/api/v2"
	PostsEndpoint    = ApiEndpoint + "/content/posts"
	ProjectsEndpoint = ApiEndpoint + "/content/projects"
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

func (p *Post) Filename() string {
	return fmt.Sprintf(
		"%s - %s",
		p.Date.Format("02-01-2006"),
		sanitizeTitle(p.Title),
	)
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
