package backfill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"hockeytrack/internal/nhl"
	"hockeytrack/internal/store"
)

// fakeFeed serves a canned week-by-week schedule and per-game feeds, and
// records every request so tests can assert on ordering and pacing.
type fakeFeed struct {
	mu       sync.Mutex
	weeks    map[string]string  // date -> raw schedule JSON
	fail     map[string][]error // request key -> queued errors (popped per call)
	requests []string
}

func newFakeFeed() *fakeFeed {
	return &fakeFeed{weeks: map[string]string{}, fail: map[string][]error{}}
}

func (f *fakeFeed) record(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, key)
	if q := f.fail[key]; len(q) > 0 {
		f.fail[key] = q[1:]
		return q[0]
	}
	return nil
}

func (f *fakeFeed) Schedule(_ context.Context, date string) (*nhl.ScheduleResponse, []byte, error) {
	if err := f.record("schedule/" + date); err != nil {
		return nil, nil, err
	}
	body, ok := f.weeks[date]
	if !ok {
		return nil, nil, &nhl.StatusError{URL: date, Code: 404}
	}
	var s nhl.ScheduleResponse
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		return nil, nil, err
	}
	return &s, []byte(body), nil
}

// RawFeed accepts only real gamecenter path segments and records the
// play-by-play feed under the archive's short name.
func (f *fakeFeed) RawFeed(_ context.Context, gameID int64, feed string) ([]byte, error) {
	switch feed {
	case "play-by-play":
		feed = "pbp"
	case "boxscore", "landing":
	default:
		return nil, fmt.Errorf("unknown gamecenter feed %q", feed)
	}
	key := fmt.Sprintf("%s/%d", feed, gameID)
	if err := f.record(key); err != nil {
		return nil, err
	}
	return []byte(`{"feed":"` + key + `"}`), nil
}

func (f *fakeFeed) ShiftCharts(_ context.Context, gameID int64) ([]byte, error) {
	key := fmt.Sprintf("shifts/%d", gameID)
	if err := f.record(key); err != nil {
		return nil, err
	}
	return []byte(`{"feed":"` + key + `"}`), nil
}

func (f *fakeFeed) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

type game struct {
	id     int64
	season int64
	state  string
}

func week(next string, days map[string][]game) string {
	type g struct {
		ID                int64  `json:"id"`
		Season            int64  `json:"season"`
		GameType          int    `json:"gameType"`
		GameState         string `json:"gameState"`
		GameScheduleState string `json:"gameScheduleState"`
	}
	type day struct {
		Date  string `json:"date"`
		Games []g    `json:"games"`
	}
	var gw []day
	for date, games := range days {
		d := day{Date: date}
		for _, x := range games {
			d.Games = append(d.Games, g{ID: x.id, Season: x.season, GameType: 2, GameState: x.state, GameScheduleState: "OK"})
		}
		gw = append(gw, d)
	}
	body, _ := json.Marshal(map[string]any{"nextStartDate": next, "gameWeek": gw})
	return string(body)
}

type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slept = append(c.slept, d)
	c.now = c.now.Add(d)
	return nil
}

func deps(feed *fakeFeed, ar *store.FakeArchive, clock *fakeClock) Deps {
	return Deps{Feed: feed, Archive: ar, Now: clock.Now, Sleep: clock.Sleep}
}

// A modern season: two weeks of games, then a week that already belongs to
// the next season (which must end the walk without being fetched).
func modernSeason(feed *fakeFeed) {
	feed.weeks["2025-09-01"] = week("2025-09-08", map[string][]game{})
	feed.weeks["2025-09-08"] = week("2025-09-15", map[string][]game{
		"2025-09-08": {{2025010001, 20252026, "OFF"}},
		"2025-09-09": {{2025010002, 20252026, "OFF"}, {2025010003, 20252026, "FINAL"}},
	})
	feed.weeks["2025-09-15"] = week("2026-09-20", map[string][]game{
		"2025-09-16": {{2025010004, 20252026, "OFF"}},
		"2025-09-17": {{2025010005, 20252026, "PPD"}}, // never played: skipped
	})
	feed.weeks["2026-09-20"] = week("2026-09-27", map[string][]game{
		"2026-09-20": {{2026010001, 20262027, "OFF"}},
	})
}

func TestRunArchivesEveryFinishedGame(t *testing.T) {
	feed := newFakeFeed()
	modernSeason(feed)
	ar := store.NewFakeArchive()
	clock := &fakeClock{now: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)}

	stats, err := Run(context.Background(), deps(feed, ar, clock), Config{}, 20252026)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Games != 4 || stats.Fetched != 16 || stats.Skipped != 0 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	for _, id := range []int64{2025010001, 2025010002, 2025010003, 2025010004} {
		for _, feed := range []string{"pbp", "boxscore", "landing", "shifts"} {
			date := map[int64]string{2025010001: "2025-09-08", 2025010002: "2025-09-09", 2025010003: "2025-09-09", 2025010004: "2025-09-16"}[id]
			if _, ok := ar.Objects[store.FinalKey(20252026, date, id, feed)]; !ok {
				t.Errorf("missing final object for game %d feed %s", id, feed)
			}
		}
	}
	if _, ok := ar.Objects[store.FinalKey(20252026, "2025-09-17", 2025010005, "pbp")]; ok {
		t.Error("postponed game must not be fetched")
	}
	if feed.count("pbp/2026") != 0 {
		t.Error("next season's games must not be fetched")
	}
	// Weeks with games are archived under the shared schedule namespace;
	// the empty first week and the next-season week are not.
	for _, d := range []string{"2025-09-08", "2025-09-15"} {
		if _, ok := ar.Objects[store.ScheduleKey(d)]; !ok {
			t.Errorf("week %s not archived", d)
		}
	}
	for _, d := range []string{"2025-09-01", "2026-09-20"} {
		if _, ok := ar.Objects[store.ScheduleKey(d)]; ok {
			t.Errorf("week %s should not be archived", d)
		}
	}
}

func TestRunSkipsShiftsBeforeShiftEra(t *testing.T) {
	feed := newFakeFeed()
	feed.weeks["1917-09-01"] = week("1917-12-19", map[string][]game{})
	feed.weeks["1917-12-19"] = week("1918-12-16", map[string][]game{
		"1917-12-19": {{1917020001, 19171918, "OFF"}},
	})
	feed.weeks["1918-12-16"] = week("", map[string][]game{
		"1918-12-21": {{1918020001, 19181919, "OFF"}},
	})
	ar := store.NewFakeArchive()
	clock := &fakeClock{now: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)}

	stats, err := Run(context.Background(), deps(feed, ar, clock), Config{}, 19171918)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Games != 1 || stats.Fetched != 3 {
		t.Fatalf("stats = %+v", stats)
	}
	if feed.count("shifts/") != 0 {
		t.Error("shift charts must not be requested before 2010-11")
	}
}

func TestRunResumesByListingArchive(t *testing.T) {
	feed := newFakeFeed()
	modernSeason(feed)
	ar := store.NewFakeArchive()
	// Two of the first game's feeds already exist from an earlier run.
	_ = ar.Put(context.Background(), store.FinalKey(20252026, "2025-09-08", 2025010001, "pbp"), []byte(`{}`))
	_ = ar.Put(context.Background(), store.FinalKey(20252026, "2025-09-08", 2025010001, "shifts"), []byte(`{}`))
	clock := &fakeClock{now: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)}

	stats, err := Run(context.Background(), deps(feed, ar, clock), Config{}, 20252026)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Skipped != 2 || stats.Fetched != 14 {
		t.Fatalf("stats = %+v", stats)
	}
	if feed.count("pbp/2025010001") != 0 || feed.count("shifts/2025010001") != 0 {
		t.Error("existing objects must not be re-fetched")
	}
	if string(ar.Objects[store.FinalKey(20252026, "2025-09-08", 2025010001, "pbp")]) != `{}` {
		t.Error("existing object was overwritten")
	}
}

func TestRunPacesRequests(t *testing.T) {
	feed := newFakeFeed()
	modernSeason(feed)
	clock := &fakeClock{now: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)}

	_, err := Run(context.Background(), deps(feed, store.NewFakeArchive(), clock), Config{Interval: 400 * time.Millisecond}, 20252026)
	if err != nil {
		t.Fatal(err)
	}
	// 4 schedule weeks + 16 game feeds = 20 requests; every one after the
	// first waits out the interval because the fake clock never advances
	// on its own.
	if n := len(feed.requests); n != 20 {
		t.Fatalf("requests = %d", n)
	}
	if n := len(clock.slept); n != 19 {
		t.Fatalf("sleeps = %d, want 19", n)
	}
	for _, d := range clock.slept {
		if d != 400*time.Millisecond {
			t.Fatalf("slept %v, want 400ms", d)
		}
	}
}

func TestRunRetriesThrottleAndRecordsMissing(t *testing.T) {
	feed := newFakeFeed()
	modernSeason(feed)
	// Throttled twice, then succeeds.
	feed.fail["boxscore/2025010002"] = []error{
		&nhl.StatusError{Code: 429}, &nhl.StatusError{Code: 503},
	}
	// Genuinely absent: no retry, no object, counted as missing.
	feed.fail["landing/2025010003"] = []error{&nhl.StatusError{Code: 404}}
	// Persistent failure exhausts retries and is reported.
	feed.fail["pbp/2025010004"] = []error{
		errors.New("net"), errors.New("net"), errors.New("net"), errors.New("net"), errors.New("net"),
	}
	ar := store.NewFakeArchive()
	clock := &fakeClock{now: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)}

	stats, err := Run(context.Background(), deps(feed, ar, clock), Config{}, 20252026)
	if err == nil {
		t.Fatal("expected error when a feed fails permanently")
	}
	if stats.Fetched != 14 || stats.Missing != 1 || stats.Failed != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if feed.count("boxscore/2025010002") != 3 {
		t.Errorf("throttled feed fetched %d times, want 3", feed.count("boxscore/2025010002"))
	}
	if feed.count("landing/2025010003") != 1 {
		t.Errorf("404 must not be retried, got %d requests", feed.count("landing/2025010003"))
	}
	if _, ok := ar.Objects[store.FinalKey(20252026, "2025-09-09", 2025010002, "boxscore")]; !ok {
		t.Error("retried feed should have been archived")
	}
	if _, ok := ar.Objects[store.FinalKey(20252026, "2025-09-16", 2025010004, "pbp")]; ok {
		t.Error("failed feed must not leave an object behind")
	}
	// Other feeds of the failed game are still archived.
	if _, ok := ar.Objects[store.FinalKey(20252026, "2025-09-16", 2025010004, "boxscore")]; !ok {
		t.Error("sibling feeds of a failed game should still be archived")
	}
	// Backoff sleeps grow: the throttle retry waits are longer than pacing.
	var backoffs int
	for _, d := range clock.slept {
		if d >= time.Second {
			backoffs++
		}
	}
	if backoffs < 2+4 {
		t.Errorf("expected at least 6 backoff sleeps, got %d", backoffs)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	feed := newFakeFeed()
	modernSeason(feed)
	ctx, cancel := context.WithCancel(context.Background())
	clock := &fakeClock{now: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)}
	d := deps(feed, store.NewFakeArchive(), clock)
	d.Sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}
	_, err := Run(ctx, d, Config{Interval: time.Second}, 20252026)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestStartDate(t *testing.T) {
	cases := map[int64]string{19171918: "1917-09-01", 20252026: "2025-09-01", 20192020: "2019-09-01"}
	for season, want := range cases {
		if got := StartDate(season); got != want {
			t.Errorf("StartDate(%d) = %s, want %s", season, got, want)
		}
	}
}

// A season whose schedule never links to a later season (a lockout year, or
// the current one): the walk must stop at the cap instead of running on.
func TestRunStopsAtEndCap(t *testing.T) {
	feed := newFakeFeed()
	feed.weeks["2003-09-01"] = week("2003-10-08", map[string][]game{})
	feed.weeks["2003-10-08"] = week("2004-06-07", map[string][]game{
		"2003-10-08": {{2003020001, 20032004, "OFF"}},
	})
	feed.weeks["2004-06-07"] = week("2005-01-03", map[string][]game{
		"2004-06-07": {{2003030417, 20032004, "OFF"}},
	})
	// 2005-01-03 is past the cap (2005-01-01) and must never be requested;
	// if it were, the fake would 404 and the run would fail.
	ar := store.NewFakeArchive()
	clock := &fakeClock{now: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)}

	stats, err := Run(context.Background(), deps(feed, ar, clock), Config{}, 20032004)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Weeks != 3 || stats.Games != 2 || stats.Fetched != 6 {
		t.Fatalf("stats = %+v", stats)
	}
	if feed.count("schedule/2005-01-03") != 0 {
		t.Error("week past the end cap was requested")
	}
}

// Walking 2020-21 from September 2020 passes through the 2019-20 bubble
// playoffs: those games belong to the earlier season and must be neither
// fetched nor have their week archived.
func TestRunIgnoresPriorSeasonGamesInWindow(t *testing.T) {
	feed := newFakeFeed()
	feed.weeks["2020-09-01"] = week("2020-09-21", map[string][]game{})
	feed.weeks["2020-09-21"] = week("2021-01-11", map[string][]game{
		"2020-09-26": {{2019030415, 20192020, "OFF"}},
	})
	feed.weeks["2021-01-11"] = week("2021-10-11", map[string][]game{
		"2021-01-13": {{2020020001, 20202021, "OFF"}},
	})
	feed.weeks["2021-10-11"] = week("2021-10-18", map[string][]game{
		"2021-10-12": {{2021020001, 20212022, "OFF"}},
	})
	ar := store.NewFakeArchive()
	clock := &fakeClock{now: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)}

	stats, err := Run(context.Background(), deps(feed, ar, clock), Config{}, 20202021)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Games != 1 || stats.Fetched != 4 {
		t.Fatalf("stats = %+v", stats)
	}
	if feed.count("pbp/2019") != 0 || feed.count("pbp/2021") != 0 {
		t.Error("games from other seasons were fetched")
	}
	if _, ok := ar.Objects[store.ScheduleKey("2020-09-21")]; ok {
		t.Error("bubble-playoff week belongs to 2019-20 and must not be archived by a 2020-21 run")
	}
	if _, ok := ar.Objects[store.ScheduleKey("2021-01-11")]; !ok {
		t.Error("2020-21 week not archived")
	}
}

// A schedule week whose nextStartDate does not advance must end the walk
// rather than loop forever.
func TestRunStopsWhenLinkDoesNotAdvance(t *testing.T) {
	feed := newFakeFeed()
	feed.weeks["2025-09-01"] = week("2025-09-01", map[string][]game{
		"2025-09-01": {{2025010001, 20252026, "OFF"}},
	})
	clock := &fakeClock{now: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)}

	stats, err := Run(context.Background(), deps(feed, store.NewFakeArchive(), clock), Config{}, 20252026)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Weeks != 1 || feed.count("schedule/") != 1 {
		t.Fatalf("stats = %+v, schedule requests = %d", stats, feed.count("schedule/"))
	}
}

// A schedule week that stays unreachable aborts the walk with an error that
// is NOT ErrFeedsFailed, and it gets the larger schedule retry budget.
func TestRunScheduleOutageAbortsWalk(t *testing.T) {
	feed := newFakeFeed()
	modernSeason(feed)
	var outage []error
	for i := 0; i < scheduleAttempts; i++ {
		outage = append(outage, &nhl.StatusError{Code: 503})
	}
	feed.fail["schedule/2025-09-08"] = outage
	clock := &fakeClock{now: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)}

	stats, err := Run(context.Background(), deps(feed, store.NewFakeArchive(), clock), Config{}, 20252026)
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, ErrFeedsFailed) {
		t.Error("a walk failure must not be reported as ErrFeedsFailed")
	}
	if n := feed.count("schedule/2025-09-08"); n != scheduleAttempts {
		t.Errorf("schedule attempts = %d, want %d", n, scheduleAttempts)
	}
	if stats.Games != 0 || feed.count("pbp/") != 0 {
		t.Error("no games should be fetched when the walk aborts")
	}
}

// Feed failures, by contrast, finish the walk and are reported as
// ErrFeedsFailed so the CLI can move on to the next season.
func TestRunFeedFailuresAreErrFeedsFailed(t *testing.T) {
	feed := newFakeFeed()
	modernSeason(feed)
	feed.fail["pbp/2025010004"] = []error{
		errors.New("net"), errors.New("net"), errors.New("net"), errors.New("net"), errors.New("net"),
	}
	clock := &fakeClock{now: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)}

	_, err := Run(context.Background(), deps(feed, store.NewFakeArchive(), clock), Config{}, 20252026)
	if !errors.Is(err, ErrFeedsFailed) {
		t.Fatalf("err = %v, want ErrFeedsFailed", err)
	}
}

// Resume must ignore the poller's snapshot keys and only count final/ ones.
func TestRunResumeIgnoresSnapshotKeys(t *testing.T) {
	feed := newFakeFeed()
	modernSeason(feed)
	ar := store.NewFakeArchive()
	_ = ar.Put(context.Background(), store.SnapshotKey(20252026, "2025-09-08", 2025010001, "pbp", time.Now()), []byte(`{}`))
	clock := &fakeClock{now: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)}

	stats, err := Run(context.Background(), deps(feed, ar, clock), Config{}, 20252026)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Skipped != 0 || stats.Fetched != 16 {
		t.Fatalf("stats = %+v", stats)
	}
}
