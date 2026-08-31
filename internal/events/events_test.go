package events

import (
	"context"
	"encoding/json"
	"testing"
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
