package main

import (
	"fmt"

	"github.com/acidsailor/sponsrdownloader/internal/configuration"
	"github.com/alecthomas/kong"
)

var (
	version = "none"
	commit  = "none"
	date    = "none"
)

type CLI struct {
	configuration.Globals

	Posts   PostsCmd         `cmd:"posts" help:"Download posts (use --with-video to include videos)"`
	Version kong.VersionFlag `            help:"Print version information and quit"                  short:"v"`
}

func main() {
	var cli CLI
	ctx := kong.Parse(
		&cli,
		kong.Name("sponsrdownloader"),
		kong.Description("Dowloader for Sponsr"),
		kong.Vars{
			"version": fmt.Sprintf(
				"%s commit %s date %s",
				version,
				commit,
				date,
			),
		},
	)
	ctx.FatalIfErrorf(cli.Validate())
	err := ctx.Run(&cli.Globals)
	ctx.FatalIfErrorf(err)
}
