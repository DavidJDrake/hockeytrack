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
