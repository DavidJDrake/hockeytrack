package poller

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"hockeytrack/internal/nhl"
)

func fixturePBP(t *testing.T) *nhl.PlayByPlay {
	t.Helper()
	b, err := os.ReadFile("testdata/pbp.json")
	if err != nil {
		t.Fatal(err)
	}
	var p nhl.PlayByPlay
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

func TestBuildClockEvent(t *testing.T) {
	p := fixturePBP(t)
	p.GameState = "LIVE"
	p.Clock = nhl.Clock{TimeRemaining: "14:32", SecondsRemaining: 872, Running: true}
	p.SituationCode = "1451"
	at := time.Date(2025, 10, 7, 21, 30, 0, 0, time.UTC)

	e := BuildClockEvent(p, at)
	if e.GameID != 2025020001 || e.GameState != "LIVE" || e.Period != 3 || e.PeriodType != "REG" {
		t.Errorf("header = %+v", e)
	}
	if e.SecondsRemaining != 872 || e.TimeRemaining != "14:32" || !e.Running || e.InIntermission {
		t.Errorf("clock = %+v", e)
	}
	if e.SituationCode != "1451" || e.HomeTeam != "FLA" || e.AwayTeam != "CHI" {
		t.Errorf("teams/situation = %+v", e)
	}
	if e.Score["CHI"] != 2 || e.Score["FLA"] != 3 || e.Shots["CHI"] != 19 || e.Shots["FLA"] != 37 {
		t.Errorf("score/shots = %v / %v", e.Score, e.Shots)
	}
	if !e.ObservedAt.Equal(at) || e.SchemaVersion != 1 {
		t.Errorf("observedAt/schema = %v / %d", e.ObservedAt, e.SchemaVersion)
	}
}

func TestBuildRosterEventIsSortedAndComplete(t *testing.T) {
	p := fixturePBP(t)
	e := BuildRosterEvent(p)
	if e.GameID != 2025020001 || e.HomeTeam != "FLA" || e.AwayTeam != "CHI" {
		t.Errorf("header = %+v", e)
	}
	if len(e.Players) != len(p.RosterSpots) {
		t.Fatalf("players = %d, want %d", len(e.Players), len(p.RosterSpots))
	}
	for i := 1; i < len(e.Players); i++ {
		a, b := e.Players[i-1], e.Players[i]
		if a.Team > b.Team || (a.Team == b.Team && a.Number > b.Number) {
			t.Fatalf("players not sorted at %d: %+v then %+v", i, a, b)
		}
	}
	var marchand bool
	for _, pl := range e.Players {
		if pl.PlayerID == 8473419 {
			marchand = pl.Team == "FLA" && pl.Number == 63 && pl.Position == "L"
		}
	}
	if !marchand {
		t.Error("8473419 should be FLA #63 L")
	}
}

func TestRosterHashChangesWithRoster(t *testing.T) {
	p := fixturePBP(t)
	h1 := RosterHash(p)
	if h1 == "" || len(h1) != 64 {
		t.Fatalf("hash = %q", h1)
	}
	if RosterHash(p) != h1 {
		t.Error("hash not deterministic")
	}
	p.RosterSpots[0].SweaterNumber++
	if RosterHash(p) == h1 {
		t.Error("hash did not change when a number changed")
	}
	p.RosterSpots = nil
	if RosterHash(p) != "" {
		t.Error("empty roster must hash to empty string")
	}
}
