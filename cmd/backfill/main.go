// Command backfill archives the final feeds of past seasons into the raw
// bucket. It runs anywhere with AWS credentials and network access; it does
// not touch DynamoDB, EventBridge or the scheduler.
//
//	backfill -seasons all              # every season the API lists, newest first
//	backfill -seasons 20252026         # one season
//	backfill -seasons 19171918-19421943 # inclusive range
//
// Runs are resumable: objects already in the bucket are skipped.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"hockeytrack/internal/backfill"
	"hockeytrack/internal/nhl"
	"hockeytrack/internal/store"
)

func main() {
	bucket := flag.String("bucket", os.Getenv("RAW_BUCKET"), "raw archive bucket (default $RAW_BUCKET)")
	seasonsFlag := flag.String("seasons", "all", `"all", one season ID (20252026), or an inclusive range (19171918-19421943)`)
	rps := flag.Float64("rps", 3, "maximum NHL API requests per second")
	flag.Parse()
	if *bucket == "" {
		fmt.Fprintln(os.Stderr, "backfill: -bucket or $RAW_BUCKET is required")
		os.Exit(2)
	}
	if *rps <= 0 {
		fmt.Fprintln(os.Stderr, "backfill: -rps must be positive")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		slog.Error("aws config", "err", err)
		os.Exit(1)
	}
	client := nhl.New()
	archive := store.NewS3Archive(s3.NewFromConfig(awsCfg), *bucket)

	seasons, err := selectSeasons(ctx, client, *seasonsFlag)
	if err != nil {
		slog.Error("select seasons", "err", err)
		os.Exit(2)
	}
	slog.Info("backfill plan", "bucket", *bucket, "seasons", len(seasons), "first", seasons[0], "last", seasons[len(seasons)-1], "rps", *rps)

	d := backfill.Deps{
		Feed: client, Archive: archive, Now: time.Now,
		Sleep: func(ctx context.Context, d time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
				return nil
			}
		},
	}
	cfg := backfill.Config{Interval: time.Duration(float64(time.Second) / *rps)}

	failed := 0
	for _, season := range seasons {
		started := time.Now()
		stats, err := backfill.Run(ctx, d, cfg, season)
		if ctx.Err() != nil {
			slog.Warn("backfill interrupted", "season", season, "stats", stats)
			os.Exit(130)
		}
		if err != nil {
			failed++
			slog.Error("season incomplete", "season", season, "stats", stats, "err", err)
			continue
		}
		slog.Info("season complete", "season", season, "stats", stats, "took", time.Since(started).Round(time.Second))
	}
	if failed > 0 {
		slog.Error("backfill finished with incomplete seasons; rerun to retry", "incomplete", failed)
		os.Exit(1)
	}
}

// selectSeasons resolves the -seasons flag against the API's season list and
// returns the chosen IDs newest first, so the most useful data lands first.
func selectSeasons(ctx context.Context, client *nhl.Client, spec string) ([]int64, error) {
	all, err := client.Seasons(ctx)
	if err != nil {
		return nil, err
	}
	known := map[int64]bool{}
	for _, s := range all {
		known[s] = true
	}

	var lo, hi int64
	switch {
	case spec == "all":
		lo, hi = 0, 1<<62
	case strings.Contains(spec, "-"):
		parts := strings.SplitN(spec, "-", 2)
		a, errA := strconv.ParseInt(parts[0], 10, 64)
		b, errB := strconv.ParseInt(parts[1], 10, 64)
		if errA != nil || errB != nil || a > b {
			return nil, fmt.Errorf("bad range %q", spec)
		}
		lo, hi = a, b
	default:
		s, err := strconv.ParseInt(spec, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bad season %q", spec)
		}
		if !known[s] {
			return nil, fmt.Errorf("season %d is not listed by the API", s)
		}
		lo, hi = s, s
	}

	var chosen []int64
	for _, s := range all {
		if s >= lo && s <= hi {
			chosen = append(chosen, s)
		}
	}
	if len(chosen) == 0 {
		return nil, fmt.Errorf("no seasons match %q", spec)
	}
	sort.Slice(chosen, func(i, j int) bool { return chosen[i] > chosen[j] })
	return chosen, nil
}
