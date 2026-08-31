package poller

import (
	"encoding/json"
	"os"
	"testing"

	"hockeytrack/internal/nhl"
)

func loadPBP(t *testing.T) *nhl.PlayByPlay {
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

func TestNewPlaysHighWaterMark(t *testing.T) {
	pbp := loadPBP(t)
	all := NewPlays(pbp.Plays, 0)
	if len(all) != len(pbp.Plays) {
		t.Fatalf("from zero: %d plays, want all %d", len(all), len(pbp.Plays))
	}
	// Ascending order.
	for i := 1; i < len(all); i++ {
		if all[i].SortOrder <= all[i-1].SortOrder {
			t.Fatalf("not ascending at %d: %d then %d", i, all[i-1].SortOrder, all[i].SortOrder)
		}
	}
	// Simulate: we've seen the first half; only the rest come back.
	mid := all[len(all)/2].SortOrder
	rest := NewPlays(pbp.Plays, mid)
	if len(rest) != len(all)-len(all)/2-1 {
		t.Errorf("after mark %d: got %d plays, want %d", mid, len(rest), len(all)-len(all)/2-1)
	}
	for _, p := range rest {
		if p.SortOrder <= mid {
			t.Errorf("play %d at or below mark %d", p.SortOrder, mid)
		}
	}
	// Nothing new when mark is at the end.
	if got := NewPlays(pbp.Plays, all[len(all)-1].SortOrder); len(got) != 0 {
		t.Errorf("expected 0 new plays, got %d", len(got))
	}
}

func TestGoldenEventSequence(t *testing.T) {
	// Replay the entire game through the diff; check goal events carry
	// correct teams and running score (CHI@FLA finished 2-3).
	pbp := loadPBP(t)
	score := map[string]int{pbp.HomeTeam.Abbrev: 0, pbp.AwayTeam.Abbrev: 0}
	var goals []string
	var last int64
	for _, p := range NewPlays(pbp.Plays, last) {
		score = RunningScore(pbp, p, score)
		e := BuildPlayEvent(pbp, p, score)
		if e.SchemaVersion != 1 || e.GameID != 2025020001 {
			t.Fatalf("bad envelope: %+v", e)
		}
		if p.TypeDescKey == "goal" {
			if e.ScoringTeam == "" {
				t.Error("goal event missing scoringTeam")
			}
			goals = append(goals, e.ScoringTeam)
		} else if e.ScoringTeam != "" {
			t.Errorf("%s event has scoringTeam %q", p.TypeDescKey, e.ScoringTeam)
		}
		last = p.SortOrder
	}
	if len(goals) != 5 { // 2+3 total goals in this game
		t.Errorf("saw %d goal events, want 5: %v", len(goals), goals)
	}
	if score["FLA"] != 3 || score["CHI"] != 2 {
		t.Errorf("final running score = %v, want FLA 3 CHI 2", score)
	}
}

func TestStateClassifiers(t *testing.T) {
	for _, s := range []string{"FINAL", "OFF"} {
		if !IsFinalState(s) {
			t.Errorf("IsFinalState(%s) = false", s)
		}
	}
	for _, s := range []string{"FUT", "PRE", "LIVE", "CRIT"} {
		if IsFinalState(s) {
			t.Errorf("IsFinalState(%s) = true", s)
		}
	}
	if !IsLiveState("LIVE") || !IsLiveState("CRIT") || IsLiveState("FUT") {
		t.Error("IsLiveState misclassifies")
	}
}
