package main

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"

	"github.com/acidsailor/sponsrdownloader/internal/configuration"
	"github.com/acidsailor/sponsrdownloader/internal/manager"
	"github.com/acidsailor/sponsrdownloader/pkg/sponsr"
	"golang.org/x/sync/errgroup"
)

type PostsCmd struct {
	WithVideo  bool   `help:"Download video"`
	WithFilter string `help:"Regex to filter posts by title"`
}

func (c *PostsCmd) Run(globals *configuration.Globals) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sponsrClient, err := sponsr.NewClient(
		globals.BearerToken,
		globals.Timeout,
		globals.ConcurrencyLimit,
		globals.PaginatorLimit,
	)
	if err != nil {
		return fmt.Errorf("could not create sponsr client: %w", err)
	}

	projectID, err := sponsrClient.ProjectIDBySlug(ctx, globals.ProjectSlug)
	if err != nil {
		return fmt.Errorf(
			"could not resolve project slug %q: %w",
			globals.ProjectSlug,
			err,
		)
	}

	projects, err := sponsrClient.Projects(ctx, projectID)
	if err != nil {
		return err
	}
	if len(projects) == 0 {
		return fmt.Errorf("no projects found for slug %q", globals.ProjectSlug)
	}
	projectTitle := projects[0].ProjectTitle

	slog.Info("getting all posts", "project", projectTitle)
	posts, err := sponsrClient.Posts(ctx, projectID)
	if err != nil {
		return err
	}
	if len(posts) == 0 {
		return fmt.Errorf("no posts found for project %q", projectTitle)
	}
	slog.Info("got posts", "count", len(posts), "project", projectTitle)

	if c.WithFilter != "" {
		re, err := regexp.Compile(c.WithFilter)
		if err != nil {
			return fmt.Errorf("invalid post filter regex: %w", err)
		}
		notMatching := func(p sponsr.Post) bool { return !re.MatchString(p.Title) }
		posts = slices.DeleteFunc(posts, notMatching)
		slog.Info("filtered posts", "count", len(posts), "filter", c.WithFilter)
	}

	// Close() is the sole shutdown path for the Playwright process — must remain deferred.
	downloadManager, err := manager.NewManager(*globals, projectTitle)
	if err != nil {
		return err
	}
	defer downloadManager.Close()

	var eg errgroup.Group
	eg.SetLimit(globals.ConcurrencyLimit)

	for _, post := range posts {
		eg.Go(func() error {
			if err := downloadManager.DownloadPDF(ctx, &post); err != nil {
				return err
			}
			if c.WithVideo && post.DurationVideo > 0 {
				return downloadManager.DownloadVideo(ctx, &post)
			}
			return nil
		})
	}

	return eg.Wait()
}
