package main

import (
	"github.com/acidsailor/sponsrdownloader/internal/configuration"
	"github.com/alecthomas/kong"
)

type CLI struct {
	configuration.Globals

	Posts PostsCmd `cmd:"posts" help:"Download posts (use --with-video to include videos)"`
}

func main() {
	var cli CLI
	ctx := kong.Parse(
		&cli,
		kong.Name("sponsrdownloader"),
		kong.Description("Dowloader for Sponsr"),
	)
	ctx.FatalIfErrorf(cli.Validate())
	err := ctx.Run(&cli.Globals)
	ctx.FatalIfErrorf(err)
}
