package synth

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSnapshotsPregameComesFirst(t *testing.T) {
	s, err := Snapshots(fixture(t, "2025020001"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(s) != 362 {
		t.Fatalf("snapshots = %d, want 362 (pregame + one per play)", len(s))
	}
	p := s[0].PBP
	if p.GameState != "PRE" || len(p.Plays) != 0 {
		t.Errorf("pregame = %s with %d plays", p.GameState, len(p.Plays))
	}
	if p.HomeTeam.Score != 0 || p.AwayTeam.Score != 0 || p.HomeTeam.SOG != 0 || p.AwayTeam.SOG != 0 {
		t.Errorf("pregame score/shots = %+v %+v", p.HomeTeam, p.AwayTeam)
	}
	if p.Clock.SecondsRemaining != 1200 || p.Clock.TimeRemaining != "20:00" || p.Clock.Running {
		t.Errorf("pregame clock = %+v", p.Clock)
	}
	if len(p.RosterSpots) != 40 {
		t.Errorf("pregame rosterSpots = %d, want 40", len(p.RosterSpots))
	}
	if p.ID != 2025020001 {
		t.Errorf("pregame id = %d", p.ID)
	}
}

func TestSnapshotsLastIsFinalAndComplete(t *testing.T) {
	s, err := Snapshots(fixture(t, "2025020001"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	p := s[len(s)-1].PBP
	if p.GameState != "FINAL" {
		t.Errorf("last state = %q, want FINAL", p.GameState)
	}
	if len(p.Plays) != 361 {
		t.Errorf("last plays = %d, want 361", len(p.Plays))
	}
	if p.AwayTeam.Score != 2 || p.HomeTeam.Score != 3 {
		t.Errorf("last score = %d-%d, want 2-3", p.AwayTeam.Score, p.HomeTeam.Score)
	}
	if p.AwayTeam.SOG != 19 || p.HomeTeam.SOG != 37 {
		t.Errorf("last shots = %d-%d, want 19-37", p.AwayTeam.SOG, p.HomeTeam.SOG)
	}
}

// Values hand-verified against the fixture: see the plan's "Verified data
// facts". Index i is the snapshot whose last play is plays[i-1].
func TestSnapshotsScoreAndShotsAtCutPoints(t *testing.T) {
	s, err := Snapshots(fixture(t, "2025020001"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		playIdx                int // index into the plays slice
		sortOrder              int64
		away, home, asog, hsog int
		period                 int
		remaining              string
	}{
		{66, 166, 1, 0, 3, 8, 1, "09:57"},
		{136, 340, 2, 2, 5, 17, 2, "18:51"},
		{293, 729, 2, 3, 16, 30, 3, "10:20"},
		{351, 852, 2, 3, 19, 37, 3, "01:53"},
	}
	for _, c := range cases {
		p := s[c.playIdx+1].PBP
		last := p.Plays[len(p.Plays)-1]
		if last.SortOrder != c.sortOrder {
			t.Fatalf("snapshot %d ends at sortOrder %d, want %d", c.playIdx+1, last.SortOrder, c.sortOrder)
		}
		if p.AwayTeam.Score != c.away || p.HomeTeam.Score != c.home {
			t.Errorf("sort %d score = %d-%d, want %d-%d", c.sortOrder, p.AwayTeam.Score, p.HomeTeam.Score, c.away, c.home)
		}
		if p.AwayTeam.SOG != c.asog || p.HomeTeam.SOG != c.hsog {
			t.Errorf("sort %d shots = %d-%d, want %d-%d", c.sortOrder, p.AwayTeam.SOG, p.HomeTeam.SOG, c.asog, c.hsog)
		}
		if p.PeriodDescriptor.Number != c.period || p.Clock.TimeRemaining != c.remaining {
			t.Errorf("sort %d clock = P%d %s, want P%d %s", c.sortOrder, p.PeriodDescriptor.Number, p.Clock.TimeRemaining, c.period, c.remaining)
		}
		if p.SituationCode != "1551" {
			t.Errorf("sort %d situationCode = %q", c.sortOrder, p.SituationCode)
		}
	}
}

// Last five minutes of the third with a one-goal margin is CRIT; earlier is
// LIVE. Snapshot 352 is P3 with 01:53 left at 2-3.
func TestSnapshotsCritInTheLastFiveMinutes(t *testing.T) {
	s, err := Snapshots(fixture(t, "2025020001"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := s[352].PBP.GameState; got != "CRIT" {
		t.Errorf("late one-goal state = %q, want CRIT", got)
	}
	if got := s[137].PBP.GameState; got != "LIVE" {
		t.Errorf("mid-game state = %q, want LIVE", got)
	}
}

// period-end at play index 129 (P1) and 232 (P2) is followed by another
// period-start, so it is an intermission; the P3 period-end at 359 is not.
func TestSnapshotsIntermissionOnlyBetweenPeriods(t *testing.T) {
	s, err := Snapshots(fixture(t, "2025020001"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{129, 232} {
		c := s[i+1].PBP.Clock
		if !c.InIntermission || c.Running || c.SecondsRemaining != 0 {
			t.Errorf("snapshot after play %d = %+v, want stopped intermission", i, c)
		}
	}
	if c := s[360].PBP.Clock; c.InIntermission {
		t.Errorf("final period-end = %+v, want no intermission", c)
	}
}

func TestSnapshotsClockNeverRewindsWithinAPeriod(t *testing.T) {
	s, err := Snapshots(fixture(t, "2025020001"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	prevPeriod, prev := 0, 0
	for i, sn := range s[1:] {
		p := sn.PBP
		if p.PeriodDescriptor.Number != prevPeriod {
			prevPeriod, prev = p.PeriodDescriptor.Number, p.Clock.SecondsRemaining
			continue
		}
		if p.Clock.SecondsRemaining > prev {
			t.Fatalf("snapshot %d clock went up: %d > %d", i+1, p.Clock.SecondsRemaining, prev)
		}
		prev = p.Clock.SecondsRemaining
	}
}

// A feed with no period markers and no shot plays still terminates, still
// reports zero shots, and still carries an empty situation code.
func TestSnapshotsPreModernTierStillEndsFinal(t *testing.T) {
	s, err := Snapshots(fixture(t, "1917020001"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	last := s[len(s)-1].PBP
	if last.GameState != "FINAL" {
		t.Errorf("state = %q, want FINAL", last.GameState)
	}
	if last.AwayTeam.SOG != 0 || last.HomeTeam.SOG != 0 {
		t.Errorf("shots = %d-%d, want 0-0 (feed records no shots)", last.AwayTeam.SOG, last.HomeTeam.SOG)
	}
	if last.SituationCode != "" {
		t.Errorf("situationCode = %q, want empty", last.SituationCode)
	}
	if last.AwayTeam.Score != 7 || last.HomeTeam.Score != 4 {
		t.Errorf("score = %d-%d, want 7-4", last.AwayTeam.Score, last.HomeTeam.Score)
	}
}

func TestSnapshotsIntervalModeIsCoarserButComplete(t *testing.T) {
	s, err := Snapshots(fixture(t, "2025020001"), Options{Interval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(s) >= 362 {
		t.Errorf("interval snapshots = %d, want fewer than the 362 per-play ones", len(s))
	}
	last := s[len(s)-1].PBP
	if last.GameState != "FINAL" || len(last.Plays) != 361 {
		t.Errorf("interval last = %s with %d plays", last.GameState, len(last.Plays))
	}
}

// The whole document travels, not just the fields the poller reads, and the
// numeric ids survive the round trip (see fact F8).
func TestSnapshotsPreserveUnknownFieldsAndIDs(t *testing.T) {
	s, err := Snapshots(fixture(t, "2025020001"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(s[len(s)-1].Raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"venue", "summary", "tvBroadcasts", "gameType", "startTimeUTC"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("field %q dropped from the snapshot", k)
		}
	}
	if string(s[0].Raw[:1]) != "{" {
		t.Errorf("raw is not an object")
	}
	if got := doc["id"]; got != json.Number("2025020001") && got != float64(2025020001) {
		t.Errorf("id round-tripped as %#v", got)
	}
}

// Play.Raw must be the original play bytes, because the play event carries
// them through to consumers verbatim.
func TestSnapshotsKeepOriginalPlayBytes(t *testing.T) {
	s, err := Snapshots(fixture(t, "2025020001"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	var orig struct {
		Plays []json.RawMessage `json:"plays"`
	}
	if err := json.Unmarshal(fixture(t, "2025020001"), &orig); err != nil {
		t.Fatal(err)
	}
	got := s[len(s)-1].PBP.Plays[0].Raw
	var a, b map[string]any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(orig.Plays[0], &b); err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Errorf("play 0 has %d fields, original had %d", len(a), len(b))
	}
}

func TestSnapshotsRewritesGameID(t *testing.T) {
	s, err := Snapshots(fixture(t, "2025020001"), Options{GameID: 11025020001})
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{0, len(s) - 1} {
		if s[i].PBP.ID != 11025020001 {
			t.Errorf("snapshot %d id = %d", i, s[i].PBP.ID)
		}
	}
}
