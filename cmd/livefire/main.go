// Livefire replays an archived game onto the real EventBridge bus, so the
// website, the notification path and the scoreboard can be exercised out of
// season.
//
// It is safe by construction. Events carry the source hockeytrack.synthetic,
// which no notification rule matches — every rule in terraform/notifications.tf
// pins hockeytrack.poller. The game id is the real id plus 9,000,000,000, so
// it is eleven digits and cannot collide with a real game. The game store and
// the archive are in-memory fakes, so no DynamoDB row and no S3 object is
// written and there is nothing to clean up afterwards.
//
// -as-poller publishes under the real source instead, which does reach the
// notification rules. That is the one flag that can send a text message.
//
// Usage:
//
//	livefire -game 2024021299 -bus hockeytrack -speed 60
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"hockeytrack/internal/events"
	"hockeytrack/internal/nhl"
	"hockeytrack/internal/poller"
	"hockeytrack/internal/store"
	"hockeytrack/internal/synth"
)

// syntheticOffset lifts a ten-digit NHL game id into eleven digits, where no
// real game can ever be.
const syntheticOffset = 9_000_000_000

func main() {
	game := flag.Int64("game", 0, "archived game id to replay (required)")
	bus := flag.String("bus", "hockeytrack", "EventBridge bus name")
	bucket := flag.String("bucket", os.Getenv("HOCKEYTRACK_RAW_BUCKET"), "raw archive bucket (default $HOCKEYTRACK_RAW_BUCKET)")
	cache := flag.String("cache", synth.DefaultCacheDir(), "directory for downloaded finals")
	speed := flag.Float64("speed", 60, "game-time multiplier; 1 is real time")
	interval := flag.Duration("interval", 30*time.Second, "game time between snapshots")
	capWait := flag.Duration("cap", 5*time.Second, "longest wall-clock wait between snapshots")
	asPoller := flag.Bool("as-poller", false, "publish under the real poller source; THIS CAN SEND NOTIFICATIONS")
	dry := flag.Bool("dry-run", false, "print events instead of publishing them")
	flag.Parse()

	if *game == 0 {
		fmt.Fprintln(os.Stderr, "usage: livefire -game <id> [-speed 60] [-dry-run]")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	source := events.SourceSynthetic
	if *asPoller {
		source = events.Source
		fmt.Fprintf(os.Stderr,
			"WARNING: publishing as %q. Notification rules WILL match these events\nand subscribers may receive email or SMS. Ctrl-C within 10 seconds to abort.\n",
			source)
		select {
		case <-time.After(10 * time.Second):
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "aborted")
			return
		}
	}

	raw, err := loadFinal(ctx, *bucket, *cache, *game)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	syntheticID := *game + syntheticOffset
	snaps, err := synth.Snapshots(raw, synth.Options{Interval: *interval, GameID: syntheticID})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var first nhl.PlayByPlay
	if err := json.Unmarshal(raw, &first); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	pub, err := publisher(ctx, *bus, source, *dry)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	feed := synth.NewFeed(snaps)
	feed.Before = synth.Pacer(*speed, *capWait, sleep)

	fmt.Fprintf(os.Stderr, "livefire: %s @ %s (%s), real id %d, publishing as %d under %q, %d snapshots at %.0fx\n",
		first.AwayTeam.Abbrev, first.HomeTeam.Abbrev, first.GameDate, *game, syntheticID, source, len(snaps), *speed)

	gs := store.NewFakeGameStore()
	if err := gs.UpsertSchedule(ctx, store.GameRecord{
		GameID: syntheticID, Season: first.Season, GameDate: first.GameDate,
		HomeAbbrev: first.HomeTeam.Abbrev, AwayAbbrev: first.AwayTeam.Abbrev,
		GameState: "FUT", StartTimeUTC: time.Now(),
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	outcome, err := poller.Run(ctx, poller.Deps{
		Feed: feed, Store: gs, Archive: store.NewFakeArchive(), Pub: pub,
		Now: time.Now, Sleep: sleep,
	}, replayConfig(), syntheticID, "livefire", func() bool { return false })
	if err != nil {
		fmt.Fprintln(os.Stderr, "livefire error:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "livefire done: outcome=%d\n", outcome)
}

// replayConfig hands all pacing to synth.Pacer. The poller's own poll
// intervals are wall-clock sleeps that know nothing about -speed, so leaving
// them at their defaults would pin every run to snapshot_count x LiveInterval
// no matter how fast the caller asked for. Zeroed here, the Feed's Before
// hook is the single thing that decides how fast a replayed game runs.
func replayConfig() poller.Config {
	cfg := poller.DefaultConfig()
	cfg.LiveInterval = 0
	cfg.PregameInterval = 0
	return cfg
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type printingPub struct{}

func (printingPub) Publish(_ context.Context, dt string, detail any) error {
	b, err := json.Marshal(map[string]any{"detailType": dt, "detail": detail})
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func publisher(ctx context.Context, bus, source string, dry bool) (events.Publisher, error) {
	if dry {
		return printingPub{}, nil
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	return events.NewEventBridgePublisherWithSource(eventbridge.NewFromConfig(cfg), bus, source), nil
}

func loadFinal(ctx context.Context, bucket, cache string, game int64) ([]byte, error) {
	if cache != "" {
		if b, err := os.ReadFile(filepath.Join(cache, fmt.Sprintf("%d.json", game))); err == nil {
			return b, nil
		}
	}
	if bucket == "" {
		return nil, fmt.Errorf("game %d is not cached and -bucket is empty; pass -bucket or set HOCKEYTRACK_RAW_BUCKET", game)
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	return synth.FinalPBP(ctx, store.NewS3Archive(s3.NewFromConfig(cfg), bucket), game, cache)
}
