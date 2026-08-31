package nhl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// serveFixture returns a test server that serves the named testdata file
// for any request path, and a client pointed at it.
func fixtureClient(t *testing.T, files map[string]string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := files[r.URL.Path]
		if !ok {
			t.Errorf("unexpected request path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		b, err := os.ReadFile(filepath.Join("testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	c := New()
	c.BaseURL = srv.URL
	c.StatsBaseURL = srv.URL
	return c
}

func TestSchedule(t *testing.T) {
	c := fixtureClient(t, map[string]string{"/v1/schedule/2026-01-15": "schedule.json"})
	sched, raw, err := c.Schedule(context.Background(), "2026-01-15")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Error("raw body is empty")
	}
	if len(sched.GameWeek) == 0 {
		t.Fatal("no gameWeek days")
	}
	day := sched.GameWeek[0]
	if day.Date != "2026-01-15" {
		t.Errorf("date = %q, want 2026-01-15", day.Date)
	}
	g := day.Games[0]
	if g.ID != 2025020740 {
		t.Errorf("game id = %d, want 2025020740", g.ID)
	}
	if g.HomeTeam.Abbrev != "BUF" || g.AwayTeam.Abbrev != "MTL" {
		t.Errorf("teams = %s@%s, want MTL@BUF", g.AwayTeam.Abbrev, g.HomeTeam.Abbrev)
	}
	if g.StartTimeUTC.IsZero() {
		t.Error("startTimeUTC not parsed")
	}
	if g.GameScheduleState != "OK" {
		t.Errorf("gameScheduleState = %q, want OK", g.GameScheduleState)
	}
}

func TestPlayByPlay(t *testing.T) {
	c := fixtureClient(t, map[string]string{"/v1/gamecenter/2025020001/play-by-play": "pbp.json"})
	pbp, raw, err := c.PlayByPlay(context.Background(), 2025020001)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Error("raw body is empty")
	}
	if pbp.ID != 2025020001 || pbp.GameState != "OFF" {
		t.Errorf("id=%d state=%q, want 2025020001 OFF", pbp.ID, pbp.GameState)
	}
	if pbp.HomeTeam.Abbrev != "FLA" || pbp.AwayTeam.Abbrev != "CHI" {
		t.Errorf("teams = %s@%s, want CHI@FLA", pbp.AwayTeam.Abbrev, pbp.HomeTeam.Abbrev)
	}
	if len(pbp.Plays) < 100 {
		t.Fatalf("only %d plays parsed", len(pbp.Plays))
	}
	// Find the first goal and check details parsing + raw retention.
	var goal *Play
	for i := range pbp.Plays {
		if pbp.Plays[i].TypeDescKey == "goal" {
			goal = &pbp.Plays[i]
			break
		}
	}
	if goal == nil {
		t.Fatal("no goal play found in fixture")
	}
	if goal.SortOrder == 0 {
		t.Error("goal sortOrder not parsed")
	}
	d := goal.ParsedDetails()
	if d.EventOwnerTeamID != 16 { // CHI scored first in this game
		t.Errorf("eventOwnerTeamId = %d, want 16", d.EventOwnerTeamID)
	}
	if d.AwayScore == nil || *d.AwayScore != 1 {
		t.Error("awayScore not parsed as 1")
	}
	if len(goal.Raw) == 0 {
		t.Error("play Raw not retained")
	}
}

func TestRawFeedAndShifts(t *testing.T) {
	c := fixtureClient(t, map[string]string{
		"/v1/gamecenter/2025020001/boxscore": "boxscore.json",
		"/stats/rest/en/shiftcharts":         "boxscore.json", // any JSON body works; we only check passthrough
	})
	box, err := c.RawFeed(context.Background(), 2025020001, "boxscore")
	if err != nil {
		t.Fatal(err)
	}
	if len(box) == 0 {
		t.Error("boxscore body empty")
	}
	shifts, err := c.ShiftCharts(context.Background(), 2025020001)
	if err != nil {
		t.Fatal(err)
	}
	if len(shifts) == 0 {
		t.Error("shifts body empty")
	}
}
