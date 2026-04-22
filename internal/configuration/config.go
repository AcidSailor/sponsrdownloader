package configuration

import (
	"fmt"
	"time"
)

type Globals struct {
	BearerToken        string        `help:"Bearer token for Sponsr API."           env:"BEARER_TOKEN"         required:"true"`
	SessionCookieName  string        `help:"Session cookie name."                   env:"SESSION_COOKIE_NAME"                  default:"SESS"`
	SessionCookieValue string        `help:"Session cookie value."                  env:"SESSION_COOKIE_VALUE" required:"true"`
	ProjectSlug        string        `help:"Sponsr project slug (e.g. 'greenpig')." env:"PROJECT_SLUG"         required:"true"`
	ConcurrencyLimit   int           `help:"Max concurrent downloads."              env:"CONCURRENCY_LIMIT"                    default:"10"`
	Timeout            time.Duration `help:"HTTP request timeout."                  env:"TIMEOUT"                              default:"30s"`
	PaginatorLimit     int           `help:"Paginator limit."                       env:"PAGINATOR_LIMIT"                      default:"20"`
	FFmpegTimeout      time.Duration `help:"Timeout for ffmpeg video download."     env:"FFMPEG_TIMEOUT"                       default:"2h"`
}

func (g *Globals) Validate() error {
	if g.ConcurrencyLimit <= 0 {
		return fmt.Errorf(
			"concurrency-limit must be > 0, got %d",
			g.ConcurrencyLimit,
		)
	}
	if g.PaginatorLimit <= 0 {
		return fmt.Errorf(
			"paginator-limit must be > 0, got %d",
			g.PaginatorLimit,
		)
	}
	if g.Timeout <= 0 {
		return fmt.Errorf("timeout must be > 0, got %s", g.Timeout)
	}
	if g.FFmpegTimeout <= 0 {
		return fmt.Errorf("ffmpeg-timeout must be > 0, got %s", g.FFmpegTimeout)
	}
	return nil
}
