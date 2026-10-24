package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/scottlaird/videoencoder/internal/config"
	"github.com/scottlaird/videoencoder/internal/publish"
	"github.com/scottlaird/videoencoder/internal/state"
)

// publishVideo copies r's stream, as recorded by its last render, to
// each publish.to target.
func publishVideo(ctx context.Context, r *config.Resolved, prune bool) error {
	if len(r.Publish.To) == 0 {
		return nil
	}
	m, err := state.Load(r.Outdir, r.Name)
	if err != nil {
		return err
	}
	if len(m.Stream) == 0 {
		return fmt.Errorf("nothing to publish: %s hasn't been rendered", r.Name)
	}
	for _, name := range r.Publish.To {
		t := r.Publish.Targets[name]
		if prune && !t.PerVideo {
			return fmt.Errorf("publish target %q: --prune needs {name} in its dir or prefix, so it can't delete other videos", name)
		}
		target, err := openTarget(t)
		if err != nil {
			return fmt.Errorf("publish target %q: %w", name, err)
		}
		rep, err := publish.Sync(ctx, r.Streamdir, m.Stream, target, prune)
		if err != nil {
			return err
		}
		slog.Info("Published", "video", r.Name, "target", target.String(),
			"uploaded", len(rep.Uploaded), "unchanged", len(rep.Skipped), "deleted", len(rep.Deleted))
	}
	return nil
}

func openTarget(t *config.PublishTarget) (publish.Target, error) {
	if t.Dir != "" {
		return publish.Dir{Root: t.Dir}, nil
	}
	return publish.NewS3(publish.S3Config{
		Endpoint: t.S3.Endpoint,
		Bucket:   t.S3.Bucket,
		Prefix:   t.S3.Prefix,
		Region:   t.S3.Region,
		Profile:  t.S3.Profile,
	})
}
