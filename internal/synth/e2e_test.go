package synth_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"hockeytrack/internal/events"
	"hockeytrack/internal/nhl"
	"hockeytrack/internal/poller"
	"hockeytrack/internal/store"
	"hockeytrack/internal/synth"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// runGame drives the real poller over a synthesized game and returns
// everything it published.
func runGame(t *testing.T, name string, opts synth.Options) []events.PublishedEvent {
	t.Helper()
	raw := readFixture(t, name)
	snaps, err := synth.Snapshots(raw, opts)
	if err != nil {
		t.Fatal(err)
	}
	var first nhl.PlayByPlay
	if err := jsonUnmarshal(raw, &first); err != nil {
		t.Fatal(err)
	}
	gs := store.NewFakeGameStore()
	if err := gs.UpsertSchedule(context.Background(), store.GameRecord{
		GameID: snaps[0].PBP.ID, Season: first.Season, GameDate: first.GameDate,
		HomeAbbrev: first.HomeTeam.Abbrev, AwayAbbrev: first.AwayTeam.Abbrev,
		GameState: "FUT", StartTimeUTC: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	pub := &events.FakePublisher{}
	outcome, err := poller.Run(context.Background(), poller.Deps{
		Feed: synth.NewFeed(snaps), Store: gs, Archive: store.NewFakeArchive(), Pub: pub,
		Now:   time.Now,
		Sleep: func(context.Context, time.Duration) error { return nil },
	}, poller.DefaultConfig(), snaps[0].PBP.ID, "test", func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if outcome != poller.OutcomeFinal {
		t.Fatalf("outcome = %v, want OutcomeFinal", outcome)
	}
	return pub.Published
}

func countByType(evs []events.PublishedEvent) map[string]int {
	m := map[string]int{}
	for _, e := range evs {
		m[e.DetailType]++
	}
	return m
}

func TestPollerReachesFinalOverASynthesizedGame(t *testing.T) {
	got := countByType(runGame(t, "2025020001", synth.Options{Interval: 30 * time.Second}))
	for _, dt := range []string{events.DTStatus, events.DTRoster, events.DTClock, events.DTPlay, events.DTFinal} {
		if got[dt] == 0 {
			t.Errorf("no %s events published; got %v", dt, got)
		}
	}
	if got[events.DTPlay] != 361 {
		t.Errorf("play events = %d, want 361 (one per play, no duplicates)", got[events.DTPlay])
	}
	if got[events.DTFinal] != 1 {
		t.Errorf("final events = %d, want 1", got[events.DTFinal])
	}
	// The roster never changes in a real game, so it is published once.
	if got[events.DTRoster] != 1 {
		t.Errorf("roster events = %d, want 1", got[events.DTRoster])
	}
}

func TestPollerHandlesTheOldestTier(t *testing.T) {
	got := countByType(runGame(t, "1917020001", synth.Options{}))
	if got[events.DTPlay] != 19 {
		t.Errorf("play events = %d, want 19", got[events.DTPlay])
	}
	if got[events.DTFinal] != 1 {
		t.Errorf("final events = %d, want 1", got[events.DTFinal])
	}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
