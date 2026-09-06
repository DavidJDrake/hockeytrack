package events

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestPlayEventJSONShape(t *testing.T) {
	e := PlayEvent{
		SchemaVersion: SchemaVersion,
		GameID:        2026020123,
		Seq:           166,
		PlayType:      "goal",
		HomeTeam:      "TBL",
		AwayTeam:      "BOS",
		ActingTeam:    "TBL",
		ScoringTeam:   "TBL",
		Period:        2,
		TimeInPeriod:  "08:41",
		Score:         map[string]int{"TBL": 2, "BOS": 1},
		Raw:           json.RawMessage(`{"eventId":258}`),
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	for _, k := range []string{"schemaVersion", "gameId", "seq", "playType", "homeTeam", "awayTeam", "scoringTeam", "period", "timeInPeriod", "score", "raw"} {
		if _, ok := m[k]; !ok {
			t.Errorf("marshaled event missing key %q", k)
		}
	}
	if m["schemaVersion"].(float64) != 1 {
		t.Errorf("schemaVersion = %v, want 1", m["schemaVersion"])
	}
}

func TestNonScoringPlayOmitsScoringTeam(t *testing.T) {
	e := PlayEvent{SchemaVersion: 1, GameID: 1, Seq: 8, PlayType: "faceoff"}
	b, _ := json.Marshal(e)
	var m map[string]any
	json.Unmarshal(b, &m)
	if _, ok := m["scoringTeam"]; ok {
		t.Error("scoringTeam should be omitted when empty")
	}
}

func TestFakePublisherRecords(t *testing.T) {
	f := &FakePublisher{}
	if err := f.Publish(context.Background(), DTStatus, StatusEvent{SchemaVersion: 1, GameID: 5, GameState: "LIVE"}); err != nil {
		t.Fatal(err)
	}
	if len(f.Published) != 1 || f.Published[0].DetailType != DTStatus {
		t.Fatalf("published = %+v", f.Published)
	}
}

func TestClockAndRosterEventsSerialise(t *testing.T) {
	if DTClock != "nhl.game.clock" || DTRoster != "nhl.game.roster" {
		t.Fatalf("detail types = %q, %q", DTClock, DTRoster)
	}
	at := time.Date(2026, 10, 1, 23, 47, 12, 0, time.UTC)
	c := ClockEvent{
		SchemaVersion: SchemaVersion, GameID: 2026020001, GameState: "LIVE",
		Period: 2, PeriodType: "REG", SecondsRemaining: 872, TimeRemaining: "14:32",
		Running: true, SituationCode: "1451", HomeTeam: "NYR", AwayTeam: "TBL",
		Score: map[string]int{"TBL": 2, "NYR": 1}, Shots: map[string]int{"TBL": 17, "NYR": 22},
		ObservedAt: at,
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	for _, k := range []string{"schemaVersion", "gameId", "gameState", "period", "periodType", "secondsRemaining", "timeRemaining", "running", "inIntermission", "situationCode", "homeTeam", "awayTeam", "score", "shots", "observedAt"} {
		if _, ok := m[k]; !ok {
			t.Errorf("clock event missing json key %q", k)
		}
	}
	if m["observedAt"] != "2026-10-01T23:47:12Z" {
		t.Errorf("observedAt = %v", m["observedAt"])
	}

	r := RosterEvent{SchemaVersion: SchemaVersion, GameID: 2026020001, HomeTeam: "NYR", AwayTeam: "TBL",
		Players: []RosterPlayer{{PlayerID: 8478010, Team: "TBL", Number: 86, Position: "R"}}}
	b, _ = json.Marshal(r)
	json.Unmarshal(b, &m)
	players := m["players"].([]any)
	p := players[0].(map[string]any)
	if p["playerId"] != float64(8478010) || p["team"] != "TBL" || p["number"] != float64(86) || p["position"] != "R" {
		t.Errorf("roster player json = %v", p)
	}
}
