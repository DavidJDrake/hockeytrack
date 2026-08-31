package schedsync

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"hockeytrack/internal/nhl"
	"hockeytrack/internal/store"
)

type fakeFeed struct{ body []byte }

func (f *fakeFeed) Schedule(_ context.Context, _ string) (*nhl.ScheduleResponse, []byte, error) {
	var s nhl.ScheduleResponse
	if err := json.Unmarshal(f.body, &s); err != nil {
		return nil, nil, err
	}
	return &s, f.body, nil
}

var syncNow = time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC)

func scheduleBody(t *testing.T, games ...map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"gameWeek": []map[string]any{{"date": "2026-01-15", "games": games}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func gameJSON(id int64, start time.Time, schedState string) map[string]any {
	return map[string]any{
		"id": id, "season": 20252026, "gameType": 2,
		"startTimeUTC": start.Format(time.RFC3339), "gameState": "FUT",
		"gameScheduleState": schedState,
		"venue":             map[string]any{"default": "Test Arena"},
		"awayTeam":          map[string]any{"id": 8, "abbrev": "MTL"},
		"homeTeam":          map[string]any{"id": 7, "abbrev": "BUF"},
	}
}

func testSync(t *testing.T, body []byte) (*store.FakeGameStore, *store.FakeArchive, *FakeScheduler) {
	t.Helper()
	gs := store.NewFakeGameStore()
	ar := store.NewFakeArchive()
	sched := NewFakeScheduler()
	d := Deps{
		Feed: &fakeFeed{body: body}, Store: gs, Archive: ar, Scheduler: sched,
		Now: func() time.Time { return syncNow },
	}
	if err := Sync(context.Background(), d, Config{PregameBuffer: 15 * time.Minute}, "2026-01-15"); err != nil {
		t.Fatal(err)
	}
	return gs, ar, sched
}

func TestSyncCreatesEntriesAndRecords(t *testing.T) {
	start := syncNow.Add(10 * time.Hour)
	gs, ar, sched := testSync(t, scheduleBody(t, gameJSON(101, start, "OK")))

	rec, _ := gs.Get(context.Background(), 101)
	if rec == nil {
		t.Fatal("game not recorded")
	}
	if rec.HomeAbbrev != "BUF" || rec.GameDate != "2026-01-15" || rec.ScheduleEntryName != "hockeytrack-game-101" {
		t.Errorf("record = %+v", rec)
	}
	e, ok := sched.Entries["hockeytrack-game-101"]
	if !ok {
		t.Fatal("no scheduler entry created")
	}
	if want := start.Add(-15 * time.Minute); !e.FireAt.Equal(want) {
		t.Errorf("fireAt = %v, want %v", e.FireAt, want)
	}
	if e.GameID != 101 {
		t.Errorf("entry gameID = %d", e.GameID)
	}
	if _, ok := ar.Objects["raw/schedule/2026-01-15.json"]; !ok {
		t.Error("raw schedule not archived")
	}
}

func TestSyncDeletesPostponedEntry(t *testing.T) {
	start := syncNow.Add(10 * time.Hour)
	// First sync: OK creates the entry. Second: PPD deletes it.
	gs := store.NewFakeGameStore()
	ar := store.NewFakeArchive()
	sched := NewFakeScheduler()
	d := Deps{Feed: &fakeFeed{body: scheduleBody(t, gameJSON(101, start, "OK"))}, Store: gs, Archive: ar, Scheduler: sched, Now: func() time.Time { return syncNow }}
	cfg := Config{PregameBuffer: 15 * time.Minute}
	if err := Sync(context.Background(), d, cfg, "2026-01-15"); err != nil {
		t.Fatal(err)
	}
	d.Feed = &fakeFeed{body: scheduleBody(t, gameJSON(101, start, "PPD"))}
	if err := Sync(context.Background(), d, cfg, "2026-01-15"); err != nil {
		t.Fatal(err)
	}
	if _, ok := sched.Entries["hockeytrack-game-101"]; ok {
		t.Error("postponed game's entry not deleted")
	}
}

func TestSyncSkipsStartedGames(t *testing.T) {
	past := syncNow.Add(-2 * time.Hour)
	_, _, sched := testSync(t, scheduleBody(t, gameJSON(102, past, "OK")))
	if _, ok := sched.Entries["hockeytrack-game-102"]; ok {
		t.Error("entry created for already-started game")
	}
}

func TestSyncRescheduleMovesEntry(t *testing.T) {
	start := syncNow.Add(10 * time.Hour)
	gs := store.NewFakeGameStore()
	sched := NewFakeScheduler()
	d := Deps{Feed: &fakeFeed{body: scheduleBody(t, gameJSON(101, start, "OK"))}, Store: gs, Archive: store.NewFakeArchive(), Scheduler: sched, Now: func() time.Time { return syncNow }}
	cfg := Config{PregameBuffer: 15 * time.Minute}
	Sync(context.Background(), d, cfg, "2026-01-15")
	moved := start.Add(3 * time.Hour)
	d.Feed = &fakeFeed{body: scheduleBody(t, gameJSON(101, moved, "OK"))}
	Sync(context.Background(), d, cfg, "2026-01-15")
	if got := sched.Entries["hockeytrack-game-101"].FireAt; !got.Equal(moved.Add(-15 * time.Minute)) {
		t.Errorf("fireAt = %v, want %v", got, moved.Add(-15*time.Minute))
	}
}
