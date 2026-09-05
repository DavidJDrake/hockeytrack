// Command analyze flattens archived games into CSV for ad-hoc analysis.
//
// It reads the final/ objects the poller and backfill write
// (raw/{season}/{date}/{gameId}/final/{pbp,boxscore,landing,shifts}.json),
// from the raw bucket or from a local mirror of it, and writes games.csv,
// plays.csv and shifts.csv to -out.
//
//	analyze -bucket my-raw-bucket -season 20252026 -out ./csv
//	analyze -dir ./mirror -out ./csv         # after: aws s3 sync s3://my-raw-bucket/raw ./mirror/raw
//
// Games whose pbp.json is missing or unreadable are logged and skipped; the
// exit status is 1 if any were.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"hockeytrack/internal/analyze"
	"hockeytrack/internal/store"
)

func main() {
	bucket := flag.String("bucket", os.Getenv("RAW_BUCKET"), "read from this raw archive bucket (default $RAW_BUCKET)")
	dir := flag.String("dir", "", "read from a local directory mirroring the bucket layout instead of S3")
	out := flag.String("out", ".", "directory to write games.csv, plays.csv and shifts.csv into")
	season := flag.Int64("season", 0, "only this season (e.g. 20252026); default every season")
	date := flag.String("date", "", "only this game date (YYYY-MM-DD); requires -season")
	flag.Parse()

	if (*bucket == "") == (*dir == "") {
		fmt.Fprintln(os.Stderr, "analyze: exactly one of -bucket (or $RAW_BUCKET) and -dir is required")
		os.Exit(2)
	}
	if *date != "" && *season == 0 {
		fmt.Fprintln(os.Stderr, "analyze: -date requires -season")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var src analyze.Source
	opts := analyze.Options{Season: *season, Date: *date}
	if *dir != "" {
		src = analyze.DirSource{Root: *dir}
	} else {
		awsCfg, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			slog.Error("aws config", "err", err)
			os.Exit(1)
		}
		src = store.NewS3Archive(s3.NewFromConfig(awsCfg), *bucket)
		// Narrow the listing as far as the filters allow; a whole bucket
		// lists every snapshot of every live game too.
		opts.Prefix = "raw/"
		if *season != 0 {
			opts.Prefix += fmt.Sprintf("%d/", *season)
			if *date != "" {
				opts.Prefix += *date + "/"
			}
		}
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		slog.Error("create output dir", "err", err)
		os.Exit(1)
	}
	var w analyze.Writers
	var files []*os.File
	for _, f := range []struct {
		name string
		dst  *io.Writer
	}{{"games.csv", &w.Games}, {"plays.csv", &w.Plays}, {"shifts.csv", &w.Shifts}} {
		fh, err := os.Create(filepath.Join(*out, f.name))
		if err != nil {
			slog.Error("create output", "file", f.name, "err", err)
			os.Exit(1)
		}
		files = append(files, fh)
		*f.dst = fh
	}

	stats, err := analyze.Run(ctx, src, opts, w)
	for _, fh := range files {
		if cerr := fh.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	switch {
	case errors.Is(err, analyze.ErrGamesFailed):
		slog.Error("finished with unreadable games", "stats", stats)
		os.Exit(1)
	case ctx.Err() != nil:
		slog.Warn("interrupted", "stats", stats)
		os.Exit(130)
	case err != nil:
		slog.Error("analyze failed", "stats", stats, "err", err)
		os.Exit(1)
	}
	slog.Info("done", "out", *out, "stats", stats)
}
