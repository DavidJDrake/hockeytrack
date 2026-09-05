package schedsync

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// weekFeed serves distinct bodies per requested week-start date and records
// which dates were requested, so multi-week syncs can be asserted on.
type weekFeed struct {
	weeks     map[string][]byte
	requested []string
}

func (f *weekFeed) Schedule(_ context.Context, date string) (*nhl.ScheduleResponse, []byte, error) {
	f.requested = append(f.requested, date)
	body, ok := f.weeks[date]
	if !ok {
		body = []byte(`{"gameWeek":[]}`)
	}
	var s nhl.ScheduleResponse
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, nil, err
	}
	return &s, body, nil
}

func weekBody(t *testing.T, date, next string, games ...map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"nextStartDate": next,
		"gameWeek":      []map[string]any{{"date": date, "games": games}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSyncFollowsNextStartDateAcrossWeeks(t *testing.T) {
	feed := &weekFeed{weeks: map[string][]byte{
		"2026-01-15": weekBody(t, "2026-01-15", "2026-01-22", gameJSON(201, syncNow.Add(10*time.Hour), "OK")),
		"2026-01-22": weekBody(t, "2026-01-22", "2026-01-29"), // empty week mid-season
		"2026-01-29": weekBody(t, "2026-01-29", "2026-02-05", gameJSON(202, syncNow.Add(15*24*time.Hour), "OK")),
		"2026-02-05": weekBody(t, "2026-02-05", ""), // end of published schedule
	}}
	gs := store.NewFakeGameStore()
	ar := store.NewFakeArchive()
	sched := NewFakeScheduler()
	d := Deps{Feed: feed, Store: gs, Archive: ar, Scheduler: sched, Now: func() time.Time { return syncNow }}
	if err := Sync(context.Background(), d, Config{PregameBuffer: 15 * time.Minute, Horizon: 300 * 24 * time.Hour}, "2026-01-15"); err != nil {
		t.Fatal(err)
	}
	if len(feed.requested) != 4 {
		t.Errorf("requested weeks = %v, want 4 chained weeks", feed.requested)
	}
	for _, id := range []int64{201, 202} {
		if rec, _ := gs.Get(context.Background(), id); rec == nil {
			t.Errorf("game %d not recorded", id)
		}
		if _, ok := sched.Entries[EntryName(id)]; !ok {
			t.Errorf("no scheduler entry for game %d", id)
		}
	}
	// Archive: the first week always, later weeks only when they hold games.
	for key, want := range map[string]bool{
		"raw/schedule/2026-01-15.json": true, "raw/schedule/2026-01-22.json": false,
		"raw/schedule/2026-01-29.json": true, "raw/schedule/2026-02-05.json": false,
	} {
		if _, ok := ar.Objects[key]; ok != want {
			t.Errorf("archive %s present=%v, want %v", key, ok, want)
		}
	}
}

func TestSyncStopsAtHorizon(t *testing.T) {
	feed := &weekFeed{weeks: map[string][]byte{
		"2026-01-15": weekBody(t, "2026-01-15", "2026-01-22", gameJSON(201, syncNow.Add(10*time.Hour), "OK")),
		"2026-01-22": weekBody(t, "2026-01-22", "2026-01-29"),
		"2026-01-29": weekBody(t, "2026-01-29", "2026-02-05", gameJSON(202, syncNow.Add(15*24*time.Hour), "OK")),
	}}
	gs := store.NewFakeGameStore()
	d := Deps{Feed: feed, Store: gs, Archive: store.NewFakeArchive(), Scheduler: NewFakeScheduler(), Now: func() time.Time { return syncNow }}
	// 10-day horizon from 01-15 reaches the 01-22 week but not 01-29.
	if err := Sync(context.Background(), d, Config{PregameBuffer: 15 * time.Minute, Horizon: 10 * 24 * time.Hour}, "2026-01-15"); err != nil {
		t.Fatal(err)
	}
	if len(feed.requested) != 2 {
		t.Errorf("requested weeks = %v, want 2 (horizon)", feed.requested)
	}
	if rec, _ := gs.Get(context.Background(), 202); rec != nil {
		t.Error("game beyond horizon was recorded")
	}
}

func TestSyncStopsWhenNextDateDoesNotAdvance(t *testing.T) {
	feed := &weekFeed{weeks: map[string][]byte{
		"2026-01-15": weekBody(t, "2026-01-15", "2026-01-15"), // pathological self-link
	}}
	d := Deps{Feed: feed, Store: store.NewFakeGameStore(), Archive: store.NewFakeArchive(), Scheduler: NewFakeScheduler(), Now: func() time.Time { return syncNow }}
	if err := Sync(context.Background(), d, Config{PregameBuffer: 15 * time.Minute, Horizon: 300 * 24 * time.Hour}, "2026-01-15"); err != nil {
		t.Fatal(err)
	}
	if len(feed.requested) != 1 {
		t.Errorf("requested weeks = %v, want exactly 1 (no infinite loop)", feed.requested)
	}
}

func TestSyncPublishesSiteSchedule(t *testing.T) {
	feed := &weekFeed{weeks: map[string][]byte{
		"2026-01-15": weekBody(t, "2026-01-15", "2026-01-22", gameJSON(201, syncNow.Add(10*time.Hour), "OK")),
		"2026-01-22": weekBody(t, "2026-01-22", "", gameJSON(202, syncNow.Add(8*24*time.Hour), "OK")),
	}}
	site := store.NewFakeArchive()
	d := Deps{Feed: feed, Store: store.NewFakeGameStore(), Archive: store.NewFakeArchive(), Scheduler: NewFakeScheduler(), Site: site, Now: func() time.Time { return syncNow }}
	if err := Sync(context.Background(), d, Config{PregameBuffer: 15 * time.Minute, Horizon: 300 * 24 * time.Hour}, "2026-01-15"); err != nil {
		t.Fatal(err)
	}
	body, ok := site.Objects["data/schedule.json"]
	if !ok {
		t.Fatal("data/schedule.json not published to the site archive")
	}
	var pub SitePayload
	if err := json.Unmarshal(body, &pub); err != nil {
		t.Fatalf("site payload is not valid JSON: %v", err)
	}
	if !pub.GeneratedAt.Equal(syncNow) {
		t.Errorf("generatedAt = %v, want %v", pub.GeneratedAt, syncNow)
	}
	if len(pub.Games) != 2 {
		t.Fatalf("published %d games, want 2 (across both weeks)", len(pub.Games))
	}
	if pub.Games[0].ID != 201 || pub.Games[1].ID != 202 {
		t.Errorf("games not in start-time order: %+v", pub.Games)
	}
	if pub.Teams["BUF"] == "" || pub.Teams["MTL"] == "" {
		t.Errorf("teams map incomplete: %v", pub.Teams)
	}
}

// fakeStandings serves a body for Standings and counts calls, so a sync can
// be asserted to fetch the table exactly once.
type fakeStandings struct {
	body  []byte
	err   error
	calls int
}

func (f *fakeStandings) Standings(_ context.Context) (*nhl.StandingsResponse, []byte, error) {
	f.calls++
	if f.err != nil {
		return nil, nil, f.err
	}
	var s nhl.StandingsResponse
	if err := json.Unmarshal(f.body, &s); err != nil {
		return nil, nil, err
	}
	return &s, f.body, nil
}

func standingsFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "nhl", "testdata", "standings.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSyncArchivesAndPublishesStandings(t *testing.T) {
	feed := &weekFeed{weeks: map[string][]byte{
		"2026-01-15": weekBody(t, "2026-01-15", "2026-01-22", gameJSON(201, syncNow.Add(10*time.Hour), "OK")),
		"2026-01-22": weekBody(t, "2026-01-22", ""),
	}}
	standings := &fakeStandings{body: standingsFixture(t)}
	site := store.NewFakeArchive()
	ar := store.NewFakeArchive()
	d := Deps{Feed: feed, Standings: standings, Store: store.NewFakeGameStore(), Archive: ar, Scheduler: NewFakeScheduler(), Site: site, Now: func() time.Time { return syncNow }}
	if err := Sync(context.Background(), d, Config{PregameBuffer: 15 * time.Minute, Horizon: 300 * 24 * time.Hour}, "2026-01-15"); err != nil {
		t.Fatal(err)
	}
	if standings.calls != 1 {
		t.Errorf("standings fetched %d times, want exactly 1 per run", standings.calls)
	}
	raw, ok := ar.Objects["raw/standings/2026-01-15.json"]
	if !ok {
		t.Fatal("raw standings not archived under raw/standings/{run date}.json")
	}
	if string(raw) != string(standings.body) {
		t.Error("archived standings are not the untouched API body")
	}
	if _, ok := site.Objects["data/schedule.json"]; !ok {
		t.Error("schedule document missing; standings must not displace it")
	}
	body, ok := site.Objects["data/standings.json"]
	if !ok {
		t.Fatal("data/standings.json not published to the site archive")
	}
	var pub StandingsPayload
	if err := json.Unmarshal(body, &pub); err != nil {
		t.Fatalf("standings payload is not valid JSON: %v", err)
	}
	if !pub.GeneratedAt.Equal(syncNow) {
		t.Errorf("generatedAt = %v, want %v", pub.GeneratedAt, syncNow)
	}
	if pub.Season != 20252026 || pub.Date != "2026-04-17" {
		t.Errorf("season/date = %d/%s, want the standings' own 20252026/2026-04-17", pub.Season, pub.Date)
	}
	if len(pub.Teams) != 32 {
		t.Fatalf("published %d teams, want 32", len(pub.Teams))
	}
	// Ordered by conference, division, then the NHL's own division rank.
	first := pub.Teams[0]
	if first.Conference != "Eastern" || first.Division != "Atlantic" || first.Rank != 1 {
		t.Errorf("first team = %+v, want Eastern/Atlantic #1", first)
	}
	for i := 1; i < len(pub.Teams); i++ {
		a, b := pub.Teams[i-1], pub.Teams[i]
		if a.Conference == b.Conference && a.Division == b.Division && a.Rank >= b.Rank {
			t.Errorf("teams %d,%d out of division order: %+v then %+v", i-1, i, a, b)
		}
	}
	col := pub.Teams[slices.IndexFunc(pub.Teams, func(x StandingsTeam) bool { return x.Abbrev == "COL" })]
	want := StandingsTeam{Conference: "Western", Division: "Central", Abbrev: "COL", Name: "Colorado Avalanche", Rank: 1, GP: 82, W: 55, L: 16, OTL: 11, PTS: 121, GF: 302, GA: 203, Streak: "W3"}
	if col != want {
		t.Errorf("COL = %+v\n   want %+v", col, want)
	}
	// Nothing beyond the trimmed fields leaks into the site document.
	var loose map[string]any
	json.Unmarshal(body, &loose)
	if row := loose["teams"].([]any)[0].(map[string]any); len(row) != 13 {
		t.Errorf("published row has %d fields, want 13: %v", len(row), row)
	}
}

func TestSyncStandingsBeforeFirstGame(t *testing.T) {
	// Opening morning: every team 0-0-0 with no streak. Must still publish.
	rows := []map[string]any{}
	for i, ab := range []string{"TBL", "FLA"} {
		rows = append(rows, map[string]any{
			"date": "2026-10-01", "seasonId": 20262027,
			"conferenceName": "Eastern", "divisionName": "Atlantic", "divisionSequence": i + 1,
			"teamAbbrev": map[string]any{"default": ab}, "teamName": map[string]any{"default": ab + " FC"},
			"gamesPlayed": 0, "wins": 0, "losses": 0, "otLosses": 0, "points": 0, "goalFor": 0, "goalAgainst": 0,
			"streakCode": "", "streakCount": 0,
		})
	}
	body, _ := json.Marshal(map[string]any{"standings": rows})
	site := store.NewFakeArchive()
	d := Deps{Feed: &fakeFeed{body: scheduleBody(t)}, Standings: &fakeStandings{body: body}, Store: store.NewFakeGameStore(), Archive: store.NewFakeArchive(), Scheduler: NewFakeScheduler(), Site: site, Now: func() time.Time { return syncNow }}
	if err := Sync(context.Background(), d, Config{PregameBuffer: 15 * time.Minute}, "2026-10-01"); err != nil {
		t.Fatal(err)
	}
	var pub StandingsPayload
	if err := json.Unmarshal(site.Objects["data/standings.json"], &pub); err != nil {
		t.Fatal(err)
	}
	if pub.Season != 20262027 || len(pub.Teams) != 2 || pub.Teams[0].Streak != "" || pub.Teams[0].PTS != 0 {
		t.Errorf("preseason payload = %+v", pub)
	}
}

func TestSyncStandingsFailureIsReportedAfterScheduleWork(t *testing.T) {
	start := syncNow.Add(10 * time.Hour)
	site := store.NewFakeArchive()
	sched := NewFakeScheduler()
	d := Deps{Feed: &fakeFeed{body: scheduleBody(t, gameJSON(101, start, "OK"))}, Standings: &fakeStandings{err: errors.New("upstream 503")}, Store: store.NewFakeGameStore(), Archive: store.NewFakeArchive(), Scheduler: sched, Site: site, Now: func() time.Time { return syncNow }}
	err := Sync(context.Background(), d, Config{PregameBuffer: 15 * time.Minute}, "2026-01-15")
	if err == nil || !strings.Contains(err.Error(), "upstream 503") {
		t.Fatalf("err = %v, want the standings failure surfaced", err)
	}
	if _, ok := sched.Entries["hockeytrack-game-101"]; !ok {
		t.Error("game not armed; standings must run after scheduler reconciliation")
	}
	if _, ok := site.Objects["data/schedule.json"]; !ok {
		t.Error("schedule not published; standings must run after the schedule document")
	}
	if _, ok := site.Objects["data/standings.json"]; ok {
		t.Error("a failed fetch must not publish standings")
	}
}

func TestSyncWithoutStandingsFeedSkipsStandings(t *testing.T) {
	feed := &weekFeed{weeks: map[string][]byte{"2026-01-15": weekBody(t, "2026-01-15", "")}}
	site := store.NewFakeArchive()
	ar := store.NewFakeArchive()
	d := Deps{Feed: feed, Store: store.NewFakeGameStore(), Archive: ar, Scheduler: NewFakeScheduler(), Site: site, Now: func() time.Time { return syncNow }}
	if err := Sync(context.Background(), d, Config{PregameBuffer: 15 * time.Minute}, "2026-01-15"); err != nil {
		t.Fatal(err)
	}
	if _, ok := site.Objects["data/standings.json"]; ok {
		t.Error("standings published with no feed configured")
	}
	if keys, _ := ar.List(context.Background(), "raw/standings/"); len(keys) != 0 {
		t.Errorf("standings archived with no feed configured: %v", keys)
	}
}

func TestSyncWithoutSiteIsUnchanged(t *testing.T) {
	// Site is optional: nil must not be dereferenced.
	_, ar, _ := testSync(t, scheduleBody(t, gameJSON(101, syncNow.Add(10*time.Hour), "OK")))
	if _, ok := ar.Objects["data/schedule.json"]; ok {
		t.Error("site payload written to the raw archive")
	}
}
