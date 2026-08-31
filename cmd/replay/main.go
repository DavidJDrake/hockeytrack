// Replay pushes a directory of recorded play-by-play snapshots through the
// full poller path against in-memory fakes, printing every published event.
// Usage: go run ./cmd/replay -game path/to/snapshots/
// Snapshot files are replayed in lexical order; the last must be a final state.
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

	"hockeytrack/internal/nhl"
	"hockeytrack/internal/poller"
	"hockeytrack/internal/store"
)

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
	dir := flag.String("game", "", "directory of pbp snapshot JSON files")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "usage: replay -game <snapshot dir>")
		os.Exit(2)
	}
	entries, err := filepath.Glob(filepath.Join(*dir, "*.json"))
	if err != nil || len(entries) == 0 {
		fmt.Fprintf(os.Stderr, "no snapshots in %s\n", *dir)
		os.Exit(2)
	}
	sort.Strings(entries)
	feed := &dirFeed{}
	for _, e := range entries {
		b, err := os.ReadFile(e)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		feed.bodies = append(feed.bodies, b)
	}
	var first nhl.PlayByPlay
	json.Unmarshal(feed.bodies[0], &first)

	gs := store.NewFakeGameStore()
	gs.UpsertSchedule(context.Background(), store.GameRecord{
		GameID: first.ID, Season: first.Season, GameDate: first.GameDate,
		HomeAbbrev: first.HomeTeam.Abbrev, AwayAbbrev: first.AwayTeam.Abbrev,
		GameState: "FUT", StartTimeUTC: time.Now(),
	})
	pub := &printingPub{}
	deps := poller.Deps{
		Feed: feed, Store: gs, Archive: store.NewFakeArchive(), Pub: pub,
		Now:   time.Now,
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	outcome, err := poller.Run(context.Background(), deps, poller.DefaultConfig(), first.ID, "replay", func() bool { return false })
	if err != nil {
		fmt.Fprintln(os.Stderr, "replay error:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "replay done: outcome=%d events=%d\n", outcome, pub.n)
	if outcome != poller.OutcomeFinal {
		os.Exit(1)
	}
}
