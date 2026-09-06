// Replay drives the poller over one game and prints every event it
// publishes, against in-memory fakes. The game comes either from the S3
// archive (-game, any of the finished games the archive holds) or from a
// directory of recorded live snapshots (-dir).
//
// Usage:
//
//	replay -game 2024021299
//	replay -game 2024021299 -interval 30s
//	replay -dir path/to/snapshots/
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"hockeytrack/internal/nhl"
	"hockeytrack/internal/poller"
	"hockeytrack/internal/store"
	"hockeytrack/internal/synth"
)

type printingPub struct{ n int }

func (p *printingPub) Publish(_ context.Context, dt string, detail any) error {
	b, err := json.Marshal(map[string]any{"detailType": dt, "detail": detail})
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	p.n++
	return nil
}

func main() {
	game := flag.Int64("game", 0, "archived game id to synthesize")
	dir := flag.String("dir", "", "directory of recorded pbp snapshot JSON files")
	bucket := flag.String("bucket", os.Getenv("HOCKEYTRACK_RAW_BUCKET"), "raw archive bucket (default $HOCKEYTRACK_RAW_BUCKET)")
	cache := flag.String("cache", synth.DefaultCacheDir(), "directory for downloaded finals; empty disables caching")
	interval := flag.Duration("interval", 0, "group plays into one snapshot per interval of game time; 0 means one per play")
	flag.Parse()

	if (*game == 0) == (*dir == "") {
		fmt.Fprintln(os.Stderr, "usage: replay -game <id> | -dir <snapshot dir>")
		os.Exit(2)
	}

	ctx := context.Background()
	var feed poller.Feed
	var first nhl.PlayByPlay

	if *game != 0 {
		raw, err := loadFinal(ctx, *bucket, *cache, *game)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		snaps, err := synth.Snapshots(raw, synth.Options{Interval: *interval})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := json.Unmarshal(raw, &first); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "replaying %d (%s @ %s, %s) as %d snapshots\n",
			first.ID, first.AwayTeam.Abbrev, first.HomeTeam.Abbrev, first.GameDate, len(snaps))
		feed = synth.NewFeed(snaps)
	} else {
		f, p, err := loadDir(*dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		feed, first = f, p
	}

	gs := store.NewFakeGameStore()
	if err := gs.UpsertSchedule(ctx, store.GameRecord{
		GameID: first.ID, Season: first.Season, GameDate: first.GameDate,
		HomeAbbrev: first.HomeTeam.Abbrev, AwayAbbrev: first.AwayTeam.Abbrev,
		GameState: "FUT", StartTimeUTC: time.Now(),
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	pub := &printingPub{}
	outcome, err := poller.Run(ctx, poller.Deps{
		Feed: feed, Store: gs, Archive: store.NewFakeArchive(), Pub: pub,
		Now:   time.Now,
		Sleep: func(context.Context, time.Duration) error { return nil },
	}, poller.DefaultConfig(), first.ID, "replay", func() bool { return false })
	if err != nil {
		fmt.Fprintln(os.Stderr, "replay error:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "replay done: outcome=%d events=%d\n", outcome, pub.n)
	if outcome != poller.OutcomeFinal {
		os.Exit(1)
	}
}

// loadFinal reads the game's final feed, preferring the on-disk cache so a
// repeat replay needs no AWS credentials at all.
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

type dirFeed struct {
	bodies [][]byte
	i      int
}

func (f *dirFeed) PlayByPlay(_ context.Context, _ int64) (*nhl.PlayByPlay, []byte, error) {
	raw := f.bodies[f.i]
	if f.i < len(f.bodies)-1 {
		f.i++
	}
	var p nhl.PlayByPlay
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, nil, err
	}
	return &p, raw, nil
}

func (f *dirFeed) RawFeed(_ context.Context, _ int64, feed string) ([]byte, error) {
	return []byte(fmt.Sprintf(`{"replayStub":%q}`, feed)), nil
}

func (f *dirFeed) ShiftCharts(_ context.Context, _ int64) ([]byte, error) {
	return []byte(`{"replayStub":"shifts"}`), nil
}

func loadDir(dir string) (*dirFeed, nhl.PlayByPlay, error) {
	var first nhl.PlayByPlay
	entries, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(entries) == 0 {
		return nil, first, fmt.Errorf("no snapshots in %s", dir)
	}
	sort.Strings(entries)
	f := &dirFeed{}
	for _, e := range entries {
		b, err := os.ReadFile(e)
		if err != nil {
			return nil, first, err
		}
		f.bodies = append(f.bodies, b)
	}
	if err := json.Unmarshal(f.bodies[0], &first); err != nil {
		return nil, first, err
	}
	return f, first, nil
}
