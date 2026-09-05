package nhl

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestStandings(t *testing.T) {
	c := fixtureClient(t, map[string]string{"/v1/standings/now": "standings.json"})
	st, raw, err := c.Standings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Error("raw body is empty")
	}
	if len(st.Standings) != 32 {
		t.Fatalf("parsed %d rows, want 32", len(st.Standings))
	}
	r := st.Standings[0]
	if r.TeamAbbrev.Default != "COL" || r.TeamName.Default != "Colorado Avalanche" {
		t.Errorf("first row = %s %q, want COL Colorado Avalanche", r.TeamAbbrev.Default, r.TeamName.Default)
	}
	if r.ConferenceName != "Western" || r.DivisionName != "Central" || r.DivisionSequence != 1 {
		t.Errorf("placement = %s/%s #%d, want Western/Central #1", r.ConferenceName, r.DivisionName, r.DivisionSequence)
	}
	if r.GamesPlayed != 82 || r.Wins != 55 || r.Losses != 16 || r.OtLosses != 11 || r.Points != 121 {
		t.Errorf("record = %d-%d-%d (%d pts) in %d GP", r.Wins, r.Losses, r.OtLosses, r.Points, r.GamesPlayed)
	}
	if r.GoalFor != 302 || r.GoalAgainst != 203 {
		t.Errorf("GF/GA = %d/%d, want 302/203", r.GoalFor, r.GoalAgainst)
	}
	if got := r.Streak(); got != "W3" {
		t.Errorf("streak = %q, want W3", got)
	}
	if r.SeasonID != 20252026 || r.Date != "2026-04-17" {
		t.Errorf("season/date = %d/%s, want 20252026/2026-04-17", r.SeasonID, r.Date)
	}
}

func TestStreakEmptyBeforeFirstGame(t *testing.T) {
	if got := (StandingsRow{}).Streak(); got != "" {
		t.Errorf("zero-row streak = %q, want empty", got)
	}
	if got := (StandingsRow{StreakCode: "OT", StreakCount: 2}).Streak(); got != "OT2" {
		t.Errorf("streak = %q, want OT2", got)
	}
}

// The live endpoint answers /standings/now with a 307 to the dated
// document; the client must land on the body behind the redirect.
func TestStandingsFollowsRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/standings/now":
			http.Redirect(w, r, "/v1/standings/2026-04-17", http.StatusTemporaryRedirect)
		case "/v1/standings/2026-04-17":
			if r.Header.Get("User-Agent") != UserAgent {
				t.Errorf("User-Agent after redirect = %q", r.Header.Get("User-Agent"))
			}
			b, err := os.ReadFile(filepath.Join("testdata", "standings.json"))
			if err != nil {
				t.Fatal(err)
			}
			w.Write(b)
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c := New()
	c.BaseURL = srv.URL
	st, _, err := c.Standings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Standings) != 32 {
		t.Errorf("parsed %d rows through the redirect, want 32", len(st.Standings))
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

func TestScheduleNextStartDate(t *testing.T) {
	c := fixtureClient(t, map[string]string{"/v1/schedule/2026-01-15": "schedule.json"})
	sched, _, err := c.Schedule(context.Background(), "2026-01-15")
	if err != nil {
		t.Fatal(err)
	}
	if sched.NextStartDate != "2026-01-22" {
		t.Errorf("nextStartDate = %q, want 2026-01-22", sched.NextStartDate)
	}
}

func TestScheduleTeamNames(t *testing.T) {
	c := fixtureClient(t, map[string]string{"/v1/schedule/2026-01-15": "schedule.json"})
	sched, _, err := c.Schedule(context.Background(), "2026-01-15")
	if err != nil {
		t.Fatal(err)
	}
	g := sched.GameWeek[0].Games[0]
	if got := g.HomeTeam.Name(); got != "Buffalo Sabres" {
		t.Errorf("home name = %q, want Buffalo Sabres", got)
	}
	if got := g.AwayTeam.Name(); got != "Montréal Canadiens" {
		t.Errorf("away name = %q, want Montréal Canadiens", got)
	}
}

// sizedClient returns a client whose server streams a body of exactly n
// bytes without a Content-Length, so the limit is enforced on the read
// path rather than by the server announcing its size.
func sizedClient(t *testing.T, n int) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		chunk := bytes.Repeat([]byte(" "), 64<<10)
		for remaining := n; remaining > 0; {
			if remaining < len(chunk) {
				chunk = chunk[:remaining]
			}
			if _, err := w.Write(chunk); err != nil {
				return
			}
			remaining -= len(chunk)
		}
	}))
	t.Cleanup(srv.Close)
	c := New()
	c.BaseURL = srv.URL
	return c
}

func TestGetRejectsOversizedBody(t *testing.T) {
	c := sizedClient(t, MaxResponseBytes+1)
	_, err := c.RawFeed(context.Background(), 2025020001, "boxscore")
	if err == nil {
		t.Fatal("expected error for body over MaxResponseBytes, got nil")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d bytes", MaxResponseBytes)) {
		t.Errorf("error = %q, want mention of the %d byte limit", err, MaxResponseBytes)
	}
}

func TestGetAcceptsBodyAtLimit(t *testing.T) {
	c := sizedClient(t, MaxResponseBytes)
	body, err := c.RawFeed(context.Background(), 2025020001, "boxscore")
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != MaxResponseBytes {
		t.Errorf("len(body) = %d, want %d", len(body), MaxResponseBytes)
	}
}

func TestRequestsIdentifyThemselves(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Write([]byte(`{"gameWeek":[]}`))
	}))
	t.Cleanup(srv.Close)
	c := New()
	c.BaseURL = srv.URL
	if _, _, err := c.Schedule(context.Background(), "2026-01-15"); err != nil {
		t.Fatal(err)
	}
	if gotUA != UserAgent || !strings.Contains(gotUA, "github.com/DavidJDrake/hockeytrack") {
		t.Errorf("User-Agent = %q, want %q", gotUA, UserAgent)
	}
}
