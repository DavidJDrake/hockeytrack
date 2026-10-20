// Command index builds the query API's index from the raw archive: one
// summary per finished game and one rollup per season. It is the same code
// the indexer Lambda runs, for history and for local work.
//
//	index -bucket my-raw-bucket -index-bucket my-index-bucket -seasons 20242025
//	index -dir ./mirror -index-dir ./index -seasons 20102011-20242025
//
// Runs are resumable: a game that already has a summary is skipped, and the
// rollup is rebuilt from every summary at the end of each season.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"hockeytrack/internal/lake"
	"hockeytrack/internal/store"
)

func main() {
	bucket := flag.String("bucket", os.Getenv("RAW_BUCKET"), "raw archive bucket (default $RAW_BUCKET)")
	indexBucket := flag.String("index-bucket", os.Getenv("INDEX_BUCKET"), "index bucket (default $INDEX_BUCKET)")
	dir := flag.String("dir", "", "read the raw archive from a local mirror instead of S3")
	indexDir := flag.String("index-dir", "", "write the index to a local directory instead of S3")
	seasonsFlag := flag.String("seasons", "", `one season (20252026) or an inclusive range (20102011-20242025); default the current season`)
	concurrency := flag.Int("concurrency", 16, "parallel reads")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var raw lake.Source
	var index lake.Store
	if *dir == "" || *indexDir == "" {
		if (*dir == "" && *bucket == "") || (*indexDir == "" && *indexBucket == "") {
			fmt.Fprintln(os.Stderr, "index: the raw archive (-bucket or -dir) and the index (-index-bucket or -index-dir) are required")
			os.Exit(2)
		}
		awsCfg, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			slog.Error("aws config", "err", err)
			os.Exit(1)
		}
		s3c := s3.NewFromConfig(awsCfg)
		if *dir == "" {
			raw = store.NewS3Archive(s3c, *bucket)
		}
		if *indexDir == "" {
			index = store.NewS3Archive(s3c, *indexBucket)
		}
	}
	if *dir != "" {
		raw = store.DirArchive{Root: *dir}
	}
	if *indexDir != "" {
		index = store.DirArchive{Root: *indexDir}
	}

	seasons, err := parseSeasons(*seasonsFlag, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "index:", err)
		os.Exit(2)
	}
	ix := &lake.Indexer{Raw: raw, Index: index, Now: time.Now, Concurrency: *concurrency}
	incomplete := 0
	for _, season := range seasons {
		started := time.Now()
		st, err := ix.SyncSeason(ctx, season)
		if ctx.Err() != nil {
			slog.Warn("interrupted", "season", season, "stats", st)
			os.Exit(130)
		}
		switch {
		case errors.Is(err, lake.ErrGamesFailed):
			incomplete++
			slog.Error("season indexed with failures; rerun to retry", "season", season, "stats", st)
		case err != nil:
			slog.Error("index aborted; rerun to resume", "season", season, "stats", st, "err", err)
			os.Exit(1)
		default:
			slog.Info("season indexed", "season", season, "stats", st, "took", time.Since(started).Round(time.Second))
		}
	}
	if incomplete > 0 {
		os.Exit(1)
	}
}

// parseSeasons resolves the -seasons flag: empty means the current season,
// one id means that season, and lo-hi means every season in the range,
// newest first so the most useful data lands first.
func parseSeasons(spec string, now time.Time) ([]int64, error) {
	if spec == "" {
		return []int64{lake.CurrentSeason(now)}, nil
	}
	parse := func(s string) (int64, error) {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 19171918 || n%10000 != n/10000+1 {
			return 0, fmt.Errorf("bad season %q (want e.g. 20252026)", s)
		}
		return n, nil
	}
	lo, hi := spec, spec
	if a, b, ok := strings.Cut(spec, "-"); ok {
		lo, hi = a, b
	}
	from, err := parse(lo)
	if err != nil {
		return nil, err
	}
	to, err := parse(hi)
	if err != nil {
		return nil, err
	}
	if from > to {
		return nil, fmt.Errorf("bad range %q", spec)
	}
	var out []int64
	for s := to; s >= from; s -= 10001 {
		out = append(out, s)
	}
	return out, nil
}
