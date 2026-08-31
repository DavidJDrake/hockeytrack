package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"hockeytrack/internal/events"
	"hockeytrack/internal/nhl"
	"hockeytrack/internal/store"
)

// scriptedFeed returns successive play-by-play snapshots, then repeats the last.
type scriptedFeed struct {
	snapshots [][]byte // marshaled PlayByPlay JSON bodies
	i         int
}

func (s *scriptedFeed) PlayByPlay(_ context.Context, _ int64) (*nhl.PlayByPlay, []byte, error) {
	raw := s.snapshots[s.i]
	if s.i < len(s.snapshots)-1 {
		s.i++
	}
	var p nhl.PlayByPlay
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, nil, err
	}
	return &p, raw, nil
}

func (s *scriptedFeed) RawFeed(_ context.Context, _ int64, feed string) ([]byte, error) {
	return []byte(fmt.Sprintf(`{"feed":%q}`, feed)), nil
}

func (s *scriptedFeed) ShiftCharts(_ context.Context, _ int64) ([]byte, error) {
	return []byte(`{"data":[]}`), nil
}

// truncatedSnapshot builds a snapshot of the fixture game with only the first
// n plays and the given gameState — simulating a live game in progress.
func truncatedSnapshot(t *testing.T, n int, state string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/pbp.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	plays := m["plays"].([]any)
	if n > len(plays) {
		n = len(plays)
	}
	m["plays"] = plays[:n]
	m["gameState"] = state
	out, _ := json.Marshal(m)
	return out
}

func testDeps(feed Feed) (Deps, *store.FakeGameStore, *store.FakeArchive, *events.FakePublisher) {
	gs := store.NewFakeGameStore()
	ar := store.NewFakeArchive()
	pub := &events.FakePublisher{}
	// Advancing clock: successive snapshots must get distinct S3 keys.
	now := time.Date(2026, 1, 15, 23, 0, 0, 0, time.UTC)
	tick := func() time.Time { now = now.Add(time.Second); return now }
	gs.Now = tick
	d := Deps{
		Feed: feed, Store: gs, Archive: ar, Pub: pub,
		Now:   tick,
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	return d, gs, ar, pub
}

func seedGame(t *testing.T, gs *store.FakeGameStore) {
	t.Helper()
	err := gs.UpsertSchedule(context.Background(), store.GameRecord{
		GameID: 2025020001, Season: 20252026, GameDate: "2025-10-07",
		HomeAbbrev: "FLA", AwayAbbrev: "CHI", GameState: "FUT",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunToFinal(t *testing.T) {
	feed := &scriptedFeed{snapshots: [][]byte{
		truncatedSnapshot(t, 10, "LIVE"),
		truncatedSnapshot(t, 25, "LIVE"),
		truncatedSnapshot(t, 0, "OFF"), // 0 means all plays: adjust helper call below
	}}
	// Final snapshot carries every play.
	feed.snapshots[2] = truncatedSnapshot(t, 1<<30, "OFF")

	d, gs, ar, pub := testDeps(feed)
	seedGame(t, gs)

	out, err := Run(context.Background(), d, DefaultConfig(), 2025020001, "link1", func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if out != OutcomeFinal {
		t.Fatalf("outcome = %v, want OutcomeFinal", out)
	}

	// Every play was emitted exactly once, in order.
	var seqs []int64
	var finals, statuses int
	for _, e := range pub.Published {
		switch e.DetailType {
		case events.DTPlay:
			seqs = append(seqs, e.Detail.(events.PlayEvent).Seq)
		case events.DTFinal:
			finals++
		case events.DTStatus:
			statuses++
		}
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("duplicate or out-of-order seq at %d: %d then %d", i, seqs[i-1], seqs[i])
		}
	}
	if finals != 1 {
		t.Errorf("final events = %d, want 1", finals)
	}
	if statuses < 2 { // "" -> LIVE, LIVE -> OFF (from seeded FUT state: FUT->LIVE, LIVE->OFF)
		t.Errorf("status events = %d, want >= 2", statuses)
	}

	// Archive: pbp snapshots for each distinct body + final sweep objects.
	var pbpSnaps, finalObjs int
	for k := range ar.Objects {
		switch {
		case strings.Contains(k, "/pbp/"):
			pbpSnaps++
		case strings.Contains(k, "/final/"):
			finalObjs++
		}
	}
	if pbpSnaps != 3 {
		t.Errorf("pbp snapshots = %d, want 3 (one per distinct body)", pbpSnaps)
	}
	if finalObjs != 4 { // pbp, boxscore, landing, shifts
		t.Errorf("final objects = %d, want 4", finalObjs)
	}

	rec, _ := gs.Get(context.Background(), 2025020001)
	if !rec.Done {
		t.Error("game not marked done")
	}
}

func TestRunHandsOff(t *testing.T) {
	feed := &scriptedFeed{snapshots: [][]byte{truncatedSnapshot(t, 10, "LIVE")}}
	d, gs, _, _ := testDeps(feed)
	seedGame(t, gs)

	calls := 0
	handOff := func() bool { calls++; return calls > 2 }
	out, err := Run(context.Background(), d, DefaultConfig(), 2025020001, "link1", handOff)
	if err != nil || out != OutcomeHandOff {
		t.Fatalf("outcome=%v err=%v, want OutcomeHandOff", out, err)
	}
	rec, _ := gs.Get(context.Background(), 2025020001)
	if rec.ChainCount != 1 {
		t.Errorf("chainCount = %d, want 1", rec.ChainCount)
	}
	if rec.LastPlaySortOrder == 0 {
		t.Error("high-water mark not persisted before handoff")
	}
	// Lease released: a new link can acquire immediately.
	if ok, _ := gs.AcquireLease(context.Background(), 2025020001, "link2", time.Now().Add(time.Minute)); !ok {
		t.Error("next link cannot acquire lease after handoff")
	}
}

func TestSecondLinkResumesWithoutDuplicates(t *testing.T) {
	// Link 1 sees 10 plays then hands off; link 2 sees all plays and finishes.
	feed1 := &scriptedFeed{snapshots: [][]byte{truncatedSnapshot(t, 10, "LIVE")}}
	d, gs, _, pub := testDeps(feed1)
	seedGame(t, gs)
	calls := 0
	Run(context.Background(), d, DefaultConfig(), 2025020001, "link1", func() bool { calls++; return calls > 1 })
	firstBatch := len(pub.Published)

	d.Feed = &scriptedFeed{snapshots: [][]byte{truncatedSnapshot(t, 1<<30, "OFF")}}
	out, err := Run(context.Background(), d, DefaultConfig(), 2025020001, "link2", func() bool { return false })
	if err != nil || out != OutcomeFinal {
		t.Fatalf("link2 outcome=%v err=%v", out, err)
	}
	// No seq published by link2 may repeat one from link1.
	seen := map[int64]bool{}
	for i, e := range pub.Published {
		if e.DetailType != events.DTPlay {
			continue
		}
		seq := e.Detail.(events.PlayEvent).Seq
		if seen[seq] {
			t.Fatalf("seq %d duplicated (event %d, firstBatch=%d)", seq, i, firstBatch)
		}
		seen[seq] = true
	}
}

func TestMaxChainsGoesStale(t *testing.T) {
	feed := &scriptedFeed{snapshots: [][]byte{truncatedSnapshot(t, 10, "LIVE")}}
	d, gs, _, pub := testDeps(feed)
	seedGame(t, gs)
	gs.UpdatePollerState(context.Background(), 2025020001, store.PollerState{ChainCount: 30, GameState: "LIVE"})

	out, err := Run(context.Background(), d, DefaultConfig(), 2025020001, "link31", func() bool { return false })
	if err != nil || out != OutcomeStale {
		t.Fatalf("outcome=%v err=%v, want OutcomeStale", out, err)
	}
	var alerts int
	for _, e := range pub.Published {
		if e.DetailType == events.DTAlert {
			alerts++
		}
	}
	if alerts != 1 {
		t.Errorf("alert events = %d, want 1", alerts)
	}
}

func TestAlreadyDoneAndLeaseHeld(t *testing.T) {
	feed := &scriptedFeed{snapshots: [][]byte{truncatedSnapshot(t, 10, "LIVE")}}
	d, gs, _, _ := testDeps(feed)
	seedGame(t, gs)

	gs.AcquireLease(context.Background(), 2025020001, "other", time.Date(2026, 1, 15, 23, 5, 0, 0, time.UTC))
	if out, _ := Run(context.Background(), d, DefaultConfig(), 2025020001, "me", func() bool { return false }); out != OutcomeLeaseHeld {
		t.Errorf("outcome = %v, want OutcomeLeaseHeld", out)
	}

	gs.UpdatePollerState(context.Background(), 2025020001, store.PollerState{Done: true, GameState: "OFF"})
	if out, _ := Run(context.Background(), d, DefaultConfig(), 2025020001, "me2", func() bool { return false }); out != OutcomeAlreadyDone {
		t.Errorf("outcome = %v, want OutcomeAlreadyDone", out)
	}

	if out, _ := Run(context.Background(), d, DefaultConfig(), 999, "me3", func() bool { return false }); out != OutcomeNotScheduled {
		t.Errorf("outcome = %v, want OutcomeNotScheduled", out)
	}
}
