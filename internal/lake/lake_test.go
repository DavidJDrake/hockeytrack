package lake

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hockeytrack/internal/store"
)

func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", path))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fullBodies is 2025020001 (CHI 2 @ FLA 3, 2025-10-07) with all four feeds.
func fullBodies(t *testing.T) Bodies {
	return Bodies{
		PBP:      read(t, "internal/nhl/testdata/pbp.json"),
		Boxscore: read(t, "internal/nhl/testdata/boxscore.json"),
		Landing:  read(t, "internal/nhl/testdata/landing.json"),
		Shifts:   read(t, "internal/nhl/testdata/shifts.json"),
	}
}

func synthPBP(t *testing.T, id string) []byte {
	return read(t, "internal/synth/testdata/"+id+".json")
}

func TestSummarizeAllFeeds(t *testing.T) {
	s, err := Summarize(fullBodies(t))
	if err != nil {
		t.Fatal(err)
	}
	g := s.Game
	if g.ID != 2025020001 || g.Season != 20252026 || g.Type != 2 || g.Date != "2025-10-07" || g.State != "OFF" {
		t.Errorf("game header = %+v", g)
	}
	if g.Away.Abbrev != "CHI" || g.Away.Score != 2 || g.Home.Abbrev != "FLA" || g.Home.Score != 3 || g.Home.Name != "Florida Panthers" {
		t.Errorf("teams = %+v / %+v", g.Away, g.Home)
	}
	if !g.Start.Equal(time.Date(2025, 10, 7, 21, 0, 0, 0, time.UTC)) || g.Periods != 3 || g.LastPeriodType != "REG" || g.Live {
		t.Errorf("start/periods = %v %d %q live=%v", g.Start, g.Periods, g.LastPeriodType, g.Live)
	}
	if strings.Join(s.Feeds, ",") != "pbp,boxscore,shifts,landing" {
		t.Errorf("feeds = %v", s.Feeds)
	}
	if s.Away.Result != "L" || s.Home.Result != "W" {
		t.Errorf("results = %q %q", s.Away.Result, s.Home.Result)
	}

	// The scoring summary comes from the play-by-play with roster names.
	if len(s.Goals) != 5 {
		t.Fatalf("goals = %d", len(s.Goals))
	}
	first := s.Goals[0]
	if first.Scorer != (PlayerRef{8483493, "Frank Nazar"}) || first.Team != "CHI" || first.Strength != "ev" ||
		len(first.Assists) != 2 || first.Assists[0].ID != 8477479 || first.AwayScore != 1 || first.HomeScore != 0 ||
		first.Period != 1 || first.TimeInPeriod != "10:03" || first.ShotType != "snap" {
		t.Errorf("first goal = %+v", first)
	}

	// Faceoffs are only in the play-by-play: 70 of them, each with a winner
	// and a loser, split between the two teams.
	wins, losses := 0, 0
	for _, sk := range s.Skaters {
		wins += sk.FaceoffWins
		losses += sk.FaceoffLosses
	}
	if wins != 70 || losses != 70 || s.Away.FaceoffWins+s.Home.FaceoffWins != 70 || s.Away.FaceoffLosses+s.Home.FaceoffLosses != 70 {
		t.Errorf("faceoffs: skaters %d/%d teams %d+%d", wins, losses, s.Away.FaceoffWins, s.Home.FaceoffWins)
	}
	if s.Away.Hits+s.Home.Hits != 55 || s.Away.Blocked+s.Home.Blocked != 37 || s.Away.PIM+s.Home.PIM != 20 {
		t.Errorf("team counts: hits %d blocked %d pim %d", s.Away.Hits+s.Home.Hits, s.Away.Blocked+s.Home.Blocked, s.Away.PIM+s.Home.PIM)
	}

	// The boxscore supplies the official line, named from the roster.
	var donato *SkaterLine
	for i := range s.Skaters {
		if s.Skaters[i].PlayerID == 8477987 {
			donato = &s.Skaters[i]
		}
	}
	if donato == nil {
		t.Fatal("no line for 8477987")
	}
	if donato.Name != "Ryan Donato" || donato.Team != "CHI" || donato.Number != 8 || donato.Position != "C" ||
		donato.Hits != 3 || donato.SOG != 1 || donato.TOISeconds != 960 || donato.Shifts != 23 {
		t.Errorf("Donato = %+v", *donato)
	}
	if len(s.Skaters) < 30 {
		t.Errorf("only %d skaters", len(s.Skaters))
	}
	var knight *GoalieLine
	for i := range s.Goalies {
		if s.Goalies[i].PlayerID == 8481519 {
			knight = &s.Goalies[i]
		}
	}
	if knight == nil || knight.ShotsAgainst != 37 || knight.Saves != 34 || knight.GoalsAgainst != 3 ||
		knight.Decision != "L" || !knight.Starter || knight.Shutout || knight.TOISeconds != 59*60+4 ||
		knight.SavePct < 0.918 || knight.SavePct > 0.919 {
		t.Errorf("Knight = %+v", knight)
	}
	if len(s.Stars) != 3 || s.Stars[0].PlayerID != 8480003 || s.Stars[0].Team != "FLA" || s.Stars[0].Star != 1 {
		t.Errorf("stars = %+v", s.Stars)
	}
}

func TestSummarizePBPOnly(t *testing.T) {
	cases := []struct {
		id                     string
		away, home             string
		awayScore, homeScore   int
		last                   string
		awayResult, homeResult string
		goals                  int
	}{
		{"2024021299", "VGK", "CGY", 4, 5, "SO", "SOL", "W", 9},
		{"2024021294", "FLA", "TBL", 1, 5, "REG", "L", "W", 6},
		{"2024021298", "", "", 0, 0, "OT", "", "", 0},
		{"1917020001", "MTL", "SEN", 7, 4, "REG", "W", "L", 11},
	}
	for _, c := range cases {
		s, err := Summarize(Bodies{PBP: synthPBP(t, c.id)})
		if err != nil {
			t.Fatalf("%s: %v", c.id, err)
		}
		if c.away != "" && (s.Game.Away.Abbrev != c.away || s.Game.Home.Abbrev != c.home ||
			s.Game.Away.Score != c.awayScore || s.Game.Home.Score != c.homeScore) {
			t.Errorf("%s: header = %+v", c.id, s.Game)
		}
		if s.Game.LastPeriodType != c.last {
			t.Errorf("%s: last period = %q", c.id, s.Game.LastPeriodType)
		}
		if c.awayResult != "" && (s.Away.Result != c.awayResult || s.Home.Result != c.homeResult) {
			t.Errorf("%s: results = %q %q", c.id, s.Away.Result, s.Home.Result)
		}
		if c.goals != 0 && len(s.Goals) != c.goals {
			t.Errorf("%s: goals = %d", c.id, len(s.Goals))
		}
		if len(s.Feeds) != 1 || s.Feeds[0] != "pbp" {
			t.Errorf("%s: feeds = %v", c.id, s.Feeds)
		}
		if len(s.Goalies) != 0 {
			t.Errorf("%s: goalie lines without a boxscore: %+v", c.id, s.Goalies)
		}
		if c.id == "1917020001" {
			goals, points := 0, 0
			for _, sk := range s.Skaters {
				goals += sk.Goals
				points += sk.Points
				if sk.Name == "" {
					t.Errorf("1917 skater %d has no name", sk.PlayerID)
				}
			}
			if goals != 11 || points < 11 {
				t.Errorf("1917 derived goals=%d points=%d", goals, points)
			}
		}
	}
}

// A shootout's deciding goal is in the scoring summary but is nobody's
// goal in the statistics, which is how the NHL counts it.
func TestShootoutGoalIsNotAPlayerGoal(t *testing.T) {
	s, err := Summarize(Bodies{PBP: synthPBP(t, "2024021299")})
	if err != nil {
		t.Fatal(err)
	}
	var so *Goal
	for i := range s.Goals {
		if s.Goals[i].PeriodType == "SO" {
			so = &s.Goals[i]
		}
	}
	if so == nil || so.Scorer.ID != 8480028 {
		t.Fatalf("shootout goal = %+v", so)
	}
	goals := 0
	for _, sk := range s.Skaters {
		goals += sk.Goals
	}
	if goals != 8 {
		t.Errorf("skater goals = %d, want 8 (the shootout winner excluded)", goals)
	}
}

func TestStrength(t *testing.T) {
	cases := []struct {
		code   string
		home   bool
		want   string
		empty  bool
		reason string
	}{
		{"1551", false, "ev", false, "five on five"},
		{"1451", true, "pp", false, "home up a man"},
		{"1451", false, "sh", false, "away down a man"},
		{"0651", false, "ev", false, "away pulled the goalie at even strength"},
		{"0651", true, "ev", true, "home scores into the empty net"},
		{"0641", false, "pp", false, "away on the power play with the goalie pulled"},
		{"1460", false, "sh", true, "shorthanded empty-netter"},
		{"", true, "ev", false, "no code"},
	}
	for _, c := range cases {
		got, empty := strength(c.code, c.home)
		if got != c.want || empty != c.empty {
			t.Errorf("%s (%s, home=%v): got %s/%v want %s/%v", c.reason, c.code, c.home, got, empty, c.want, c.empty)
		}
	}
}

func TestDecodeIsLenientAboutTypesOnly(t *testing.T) {
	pbp := read(t, "internal/nhl/testdata/pbp.json")
	bad := strings.Replace(string(pbp), `"sog":19`, `"sog":"nineteen"`, 1)
	if !strings.Contains(bad, "nineteen") {
		t.Fatal("fixture no longer has the expected field")
	}
	s, err := Summarize(Bodies{PBP: []byte(bad)})
	if err != nil {
		t.Fatalf("a wrong type should not fail the game: %v", err)
	}
	if s.Game.Away.SOG != 0 || s.Game.Away.Score != 2 || len(s.Goals) != 5 {
		t.Errorf("one bad field should blank one cell: %+v", s.Game.Away)
	}
	if _, err := Summarize(Bodies{PBP: []byte(`{"id": 1, "plays": [`)}); err == nil {
		t.Error("malformed JSON should fail")
	}
	if _, err := Summarize(Bodies{PBP: []byte(`{"season": 20252026}`)}); err == nil {
		t.Error("a feed without a game id should fail")
	}
}

func TestPlaysAndShifts(t *testing.T) {
	plays, err := Plays(read(t, "internal/nhl/testdata/pbp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(plays) != 361 {
		t.Errorf("plays = %d", len(plays))
	}
	var goal *Play
	for i := range plays {
		if plays[i].Type == "goal" {
			goal = &plays[i]
			break
		}
	}
	if goal == nil {
		t.Fatal("no goal")
	}
	if goal.Team != "CHI" || goal.EventID != 258 || goal.Seq != 166 || goal.Zone != "O" || goal.X == nil || *goal.X != 66 ||
		goal.AwayScore == nil || *goal.AwayScore != 1 {
		t.Errorf("goal = %+v", *goal)
	}
	roles := map[string]int64{}
	for _, p := range goal.Players {
		roles[p.Role] = p.ID
	}
	if roles["scorer"] != 8483493 || roles["assist1"] != 8477479 || roles["goalie"] != 8475683 {
		t.Errorf("roles = %v", roles)
	}
	if goal.Details["shotType"] != "snap" {
		t.Errorf("details not passed through: %v", goal.Details)
	}
	// Details re-encode as the NHL sent them: integers stay integers.
	b, _ := json.Marshal(goal.Details["scoringPlayerId"])
	if string(b) != "8483493" {
		t.Errorf("scoringPlayerId re-encoded as %s", b)
	}
	for i := 1; i < len(plays); i++ {
		if plays[i].Seq < plays[i-1].Seq {
			t.Fatalf("plays not in sequence order at %d", i)
		}
	}

	shifts, err := Shifts(read(t, "internal/nhl/testdata/shifts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(shifts) == 0 {
		t.Fatal("no shifts")
	}
	for _, s := range shifts {
		if s.PlayerID == 8473419 && s.Period == 1 && s.ShiftNumber == 1 {
			if s.Name != "Brad Marchand" || s.Team != "FLA" || s.Seconds != 34 || s.Duration != "00:34" {
				t.Errorf("Marchand's first shift = %+v", s)
			}
			return
		}
	}
	t.Error("Marchand's first shift not found")
}

func TestLocate(t *testing.T) {
	prefix := "raw/20252026/2025-10-07/2025020001/"
	keys := []string{
		prefix + "pbp/20251007T210000Z.json", prefix + "pbp/20251007T230000Z.json",
		prefix + "boxscore/20251007T210500Z.json",
	}
	f, err := locate(prefix, keys)
	if err != nil || f.Final || f.PBP != prefix+"pbp/20251007T230000Z.json" || f.Boxscore != prefix+"boxscore/20251007T210500Z.json" || f.Landing != "" {
		t.Errorf("live: %+v %v", f, err)
	}
	keys = append(keys, prefix+"final/pbp.json", prefix+"final/shifts.json")
	f, err = locate(prefix, keys)
	if err != nil || !f.Final || f.PBP != prefix+"final/pbp.json" || f.Shifts != prefix+"final/shifts.json" || f.Boxscore != "" {
		t.Errorf("final: %+v %v", f, err)
	}
	if _, err := locate(prefix, []string{prefix + "boxscore/20251007T210500Z.json"}); !errors.Is(err, ErrNoFeeds) {
		t.Errorf("no pbp: %v", err)
	}
}

func TestSeasonHelpers(t *testing.T) {
	if SeasonOf(2024021299) != 20242025 || SeasonOf(1917020001) != 19171918 {
		t.Error("SeasonOf")
	}
	if CurrentSeason(time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)) != 20262027 ||
		CurrentSeason(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)) != 20252026 ||
		CurrentSeason(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) != 20262027 {
		t.Error("CurrentSeason")
	}
	s, d, id, ok := ParseGamePrefix("raw/20252026/2025-10-07/2025020001/")
	if !ok || s != 20252026 || d != "2025-10-07" || id != 2025020001 {
		t.Errorf("ParseGamePrefix = %d %s %d %v", s, d, id, ok)
	}
	if _, _, _, ok := ParseGamePrefix("raw/schedule/2025-10-07.json"); ok {
		t.Error("a schedule key is not a game prefix")
	}
	if SummaryKey(20252026, 2025020001) != "v1/seasons/20252026/games/2025020001.json" || RollupKey(20252026) != "v1/seasons/20252026.json" {
		t.Error("keys")
	}
}

func TestRollup(t *testing.T) {
	ids := []string{"2024021292", "2024021294", "2024021297", "2024021298", "2024021299", "2024021300"}
	var games []GameSummary
	for _, id := range ids {
		s, err := Summarize(Bodies{PBP: synthPBP(t, id)})
		if err != nil {
			t.Fatal(err)
		}
		games = append(games, *s)
	}
	// A duplicate summary for the same game replaces, not doubles.
	games = append(games, games[0])
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	r := Rollup(20242025, games, now)
	if r.Season != 20242025 || len(r.Games) != 6 || !r.GeneratedAt.Equal(now) || r.Version != Version {
		t.Fatalf("rollup header: season=%d games=%d", r.Season, len(r.Games))
	}
	for i := 1; i < len(r.Games); i++ {
		if r.Games[i].Date < r.Games[i-1].Date {
			t.Error("games not in date order")
		}
	}
	gp := 0
	teams := map[string]TeamSeason{}
	for _, tm := range r.Teams {
		gp += tm.GP
		teams[tm.Abbrev] = tm
	}
	if gp != 12 {
		t.Errorf("team games = %d, want 12", gp)
	}
	if cgy := teams["CGY"]; cgy.Wins != 1 || cgy.Points != 2 || cgy.ROW != 0 || cgy.GoalsFor != 5 || cgy.GoalsAgainst != 4 || cgy.HomeWins != 1 {
		t.Errorf("CGY (shootout win) = %+v", cgy)
	}
	if vgk := teams["VGK"]; vgk.OTLosses != 1 || vgk.Points != 1 || vgk.AwayOTLosses != 1 {
		t.Errorf("VGK (shootout loss) = %+v", vgk)
	}
	if tbl := teams["TBL"]; tbl.Wins < 1 || tbl.ROW < 1 {
		t.Errorf("TBL = %+v", tbl)
	}
	// Points-ordered, then every skater row names a player on a team.
	for i := 1; i < len(r.Skaters); i++ {
		if r.Skaters[i].Points > r.Skaters[i-1].Points {
			t.Fatal("skaters not in points order")
		}
	}
	gpSum := 0
	for _, sk := range r.Skaters {
		gpSum += sk.GP
		if sk.Team == "" || sk.PlayerID == 0 {
			t.Errorf("bad skater row %+v", sk)
		}
	}
	if gpSum == 0 {
		t.Error("no skater games")
	}
	// A live game is listed but counts toward nothing.
	live := games[1]
	live.Game.ID, live.Game.State, live.Game.Live = 2024029999, "LIVE", true
	live.Away.Result, live.Home.Result = "", ""
	r2 := Rollup(20242025, append(games, live), now)
	if len(r2.Games) != 7 {
		t.Errorf("live game not listed: %d", len(r2.Games))
	}
	gp2 := 0
	for _, tm := range r2.Teams {
		gp2 += tm.GP
	}
	if gp2 != 12 {
		t.Errorf("live game counted toward totals: %d", gp2)
	}
}

func TestIndexer(t *testing.T) {
	ctx := context.Background()
	raw := store.NewFakeArchive()
	index := store.NewFakeArchive()
	b := fullBodies(t)
	final := "raw/20252026/2025-10-07/2025020001/"
	for k, v := range map[string][]byte{
		final + "pbp/20251007T210000Z.json": b.PBP,
		final + "final/pbp.json":            b.PBP,
		final + "final/boxscore.json":       b.Boxscore,
		final + "final/landing.json":        b.Landing,
		final + "final/shifts.json":         b.Shifts,
		// A game still in progress: snapshots only, no sweep yet.
		"raw/20252026/2025-10-08/2025020002/pbp/20251008T230000Z.json":      b.PBP,
		"raw/20252026/2025-10-08/2025020002/boxscore/20251008T230000Z.json": b.Boxscore,
		"raw/schedule/2025-10-07.json":                                      []byte(`{}`),
	} {
		_ = raw.Put(ctx, k, v)
	}
	ix := &Indexer{Raw: raw, Index: index, Now: func() time.Time { return time.Unix(0, 0) }, Concurrency: 2}

	if _, err := ix.BuildGame(ctx, "raw/20252026/2025-10-08/2025020002/"); !errors.Is(err, ErrNotFinal) {
		t.Errorf("live game: %v", err)
	}
	if _, err := ix.BuildGame(ctx, "raw/schedule/"); err == nil {
		t.Error("a non-game prefix should fail")
	}
	s, err := ix.BuildGame(ctx, final)
	if err != nil || s.Game.ID != 2025020001 {
		t.Fatalf("BuildGame: %v", err)
	}
	if _, err := index.Get(ctx, SummaryKey(20252026, 2025020001)); err != nil {
		t.Fatal("summary not written")
	}

	st, err := ix.SyncSeason(ctx, 20252026)
	if err != nil || st.Final != 1 || st.Skipped != 1 || st.Built != 0 || st.Failed != 0 {
		t.Errorf("sync after build: %v %v", st, err)
	}
	_ = index.Put(ctx, "v1/seasons/20252026.json", nil)
	delete(index.Objects, SummaryKey(20252026, 2025020001))
	st, err = ix.SyncSeason(ctx, 20252026)
	if err != nil || st.Built != 1 || st.Skipped != 0 {
		t.Errorf("sync rebuilds a missing summary: %v %v", st, err)
	}
	body, err := index.Get(ctx, RollupKey(20252026))
	if err != nil {
		t.Fatal("rollup not written")
	}
	var r SeasonRollup
	if err := json.Unmarshal(body, &r); err != nil || len(r.Games) != 1 || len(r.Teams) != 2 || r.Teams[0].Abbrev != "FLA" {
		t.Errorf("rollup = %+v (%v)", r, err)
	}
	seasons, err := ix.Seasons(ctx)
	if err != nil || len(seasons) != 1 || seasons[0] != 20252026 {
		t.Errorf("seasons = %v %v", seasons, err)
	}

	// A corrupt final feed costs that game, not the season.
	_ = raw.Put(ctx, "raw/20252026/2025-10-09/2025020003/final/pbp.json", []byte("{"))
	st, err = ix.SyncSeason(ctx, 20252026)
	if !errors.Is(err, ErrGamesFailed) || st.Failed != 1 || st.Final != 2 {
		t.Errorf("corrupt game: %v %v", st, err)
	}
}

func TestDirArchive(t *testing.T) {
	ctx := context.Background()
	d := store.DirArchive{Root: filepath.Join(t.TempDir(), "index")}
	keys, err := d.List(ctx, "v1/")
	if err != nil || len(keys) != 0 {
		t.Fatalf("empty root: %v %v", keys, err)
	}
	if err := d.Put(ctx, "v1/seasons/20252026.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if err := d.Put(ctx, "../escape.json", []byte("{}")); err == nil {
		t.Error("a key escaping the root should be refused")
	}
	keys, _ = d.List(ctx, "v1/seasons/")
	body, err := d.Get(ctx, "v1/seasons/20252026.json")
	if len(keys) != 1 || keys[0] != "v1/seasons/20252026.json" || err != nil || string(body) != "{}" {
		t.Errorf("round trip: %v %s %v", keys, body, err)
	}
}
