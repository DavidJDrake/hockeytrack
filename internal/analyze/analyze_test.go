package analyze

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hockeytrack/internal/store"
)

const fixtures = "../nhl/testdata"

// fixture reads a captured NHL API response from internal/nhl/testdata.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// modernGame is the fixture game 2025020001 (CHI @ FLA, 2025-10-07) with
// every final feed present.
const modernGame = "raw/20252026/2025-10-07/2025020001/"

// earlyGame is the league's first game, 1917020001 (MTL @ SEN): goals and
// penalties only, no coordinates, no shot totals, no shift chart.
const earlyGame = "raw/19171918/1917-12-19/1917020001/"

func archive(t *testing.T) *store.FakeArchive {
	t.Helper()
	ctx := context.Background()
	a := store.NewFakeArchive()
	a.Put(ctx, modernGame+"final/pbp.json", fixture(t, "pbp.json"))
	a.Put(ctx, modernGame+"final/boxscore.json", fixture(t, "boxscore.json"))
	a.Put(ctx, modernGame+"final/landing.json", fixture(t, "landing.json"))
	a.Put(ctx, modernGame+"final/shifts.json", fixture(t, "shifts.json"))
	// Live-game snapshots sit beside final/ and must be ignored.
	a.Put(ctx, modernGame+"pbp/20251007T210500Z.json", []byte(`{"id":1,"plays":[]}`))
	a.Put(ctx, earlyGame+"final/pbp.json", fixture(t, "pbp_1917020001.json"))
	a.Put(ctx, earlyGame+"final/boxscore.json", []byte(`{}`))
	return a
}

type csvs struct {
	games, plays, shifts [][]string
}

func run(t *testing.T, src Source, opts Options) (Stats, error, csvs) {
	t.Helper()
	var g, p, s bytes.Buffer
	st, err := Run(context.Background(), src, opts, Writers{Games: &g, Plays: &p, Shifts: &s})
	return st, err, csvs{parse(t, g.String()), parse(t, p.String()), parse(t, s.String())}
}

func parse(t *testing.T, s string) [][]string {
	t.Helper()
	rows, err := csv.NewReader(strings.NewReader(s)).ReadAll()
	if err != nil {
		t.Fatalf("bad csv: %v\n%s", err, s)
	}
	return rows
}

// row returns the record as a header-keyed map.
func row(header, rec []string) map[string]string {
	m := map[string]string{}
	for i, h := range header {
		m[h] = rec[i]
	}
	return m
}

func want(t *testing.T, m map[string]string, expect map[string]string) {
	t.Helper()
	for k, v := range expect {
		if got, ok := m[k]; !ok {
			t.Errorf("no column %q", k)
		} else if got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestRunFakeArchive(t *testing.T) {
	st, err, tb := run(t, archive(t), Options{Prefix: "raw/"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Games != 2 || st.Failed != 0 {
		t.Fatalf("stats %v", st)
	}
	if len(tb.games) != 3 { // header + 2 games, oldest season first
		t.Fatalf("games.csv has %d rows", len(tb.games))
	}
	if len(tb.plays) != 1+st.Plays || len(tb.shifts) != 1+st.Shifts {
		t.Fatalf("row counts plays=%d shifts=%d, stats %v", len(tb.plays), len(tb.shifts), st)
	}

	early := row(tb.games[0], tb.games[1])
	want(t, early, map[string]string{
		"gameId": "1917020001", "season": "19171918", "gameDate": "1917-12-19",
		"awayAbbrev": "MTL", "homeAbbrev": "SEN", "awayScore": "7", "homeScore": "4",
		"periods": "3", "lastPeriodType": "REG",
		"awaySOG": "", "homeSOG": "", // not recorded in 1917
		"plays": "19", "shifts": "0",
	})

	modern := row(tb.games[0], tb.games[2])
	want(t, modern, map[string]string{
		"gameId": "2025020001", "season": "20252026", "gameType": "2", "gameDate": "2025-10-07",
		"startTimeUTC": "2025-10-07T21:00:00Z", "gameState": "OFF", "venue": "Amerant Bank Arena",
		"awayTeamId": "16", "awayAbbrev": "CHI", "homeTeamId": "13", "homeAbbrev": "FLA",
		"awayScore": "2", "homeScore": "3", "periods": "3", "lastPeriodType": "REG",
		"awaySOG": "19", "homeSOG": "37", "shifts": "851", // 856 chart rows minus 5 goal markers
	})

	// Plays: the first faceoff of the modern game carries both players and
	// centre-ice coordinates; a period-start has no details at all.
	byType := map[string]map[string]string{}
	for _, rec := range tb.plays[1:] {
		r := row(tb.plays[0], rec)
		if r["gameId"] == "2025020001" {
			if _, seen := byType[r["type"]]; !seen {
				byType[r["type"]] = r
			}
		}
	}
	want(t, byType["faceoff"], map[string]string{
		"seq": "11", "period": "1", "periodType": "REG", "timeInPeriod": "00:00", "timeRemaining": "20:00",
		"typeCode": "502", "situationCode": "1551", "teamId": "16", "team": "CHI",
		"x": "0", "y": "0", "zone": "N", "winningPlayerId": "8477450", "losingPlayerId": "8477935",
		"scoringPlayerId": "", "penaltyMinutes": "",
	})
	want(t, byType["period-start"], map[string]string{"seq": "8", "teamId": "", "team": "", "x": "", "y": ""})
	want(t, byType["blocked-shot"], map[string]string{
		"x": "-61", "y": "3", "zone": "D", "team": "FLA", "reason": "blocked",
		"shootingPlayerId": "8473419", "blockingPlayerId": "8482807",
	})
	goal := byType["goal"]
	if goal["scoringPlayerId"] == "" || goal["awayScore"] == "" || goal["homeScore"] == "" || goal["goalieInNetId"] == "" {
		t.Errorf("goal row missing scorer/score/goalie: %v", goal)
	}

	// The 1917 game: 11 goals and 8 penalties, nothing else; no coordinates.
	early1917 := 0
	for _, rec := range tb.plays[1:] {
		r := row(tb.plays[0], rec)
		if r["gameId"] != "1917020001" {
			continue
		}
		early1917++
		if r["x"] != "" || r["y"] != "" || r["situationCode"] != "" {
			t.Errorf("1917 play should have no coordinates or situation: %v", r)
		}
		switch r["type"] {
		case "goal":
			if r["scoringPlayerId"] == "" || r["team"] == "" {
				t.Errorf("1917 goal missing scorer or team: %v", r)
			}
		case "penalty":
			if r["committedByPlayerId"] == "" || r["penaltyMinutes"] == "" || r["penaltyType"] == "" {
				t.Errorf("1917 penalty missing fields: %v", r)
			}
		default:
			t.Errorf("unexpected 1917 play type %q", r["type"])
		}
	}
	if early1917 != 19 {
		t.Errorf("1917 plays = %d, want 19", early1917)
	}

	// Shifts: only real shifts (typeCode 517), all from the modern game.
	first := row(tb.shifts[0], tb.shifts[1])
	want(t, first, map[string]string{
		"gameId": "2025020001", "shiftId": "15536563", "playerId": "8473419",
		"firstName": "Brad", "lastName": "Marchand", "teamId": "13", "team": "FLA",
		"period": "1", "shiftNumber": "1", "startTime": "00:00", "endTime": "00:34",
		"duration": "00:34", "seconds": "34", "eventNumber": "6",
	})
	for _, rec := range tb.shifts[1:] {
		r := row(tb.shifts[0], rec)
		if r["gameId"] != "2025020001" || r["duration"] == "" || r["shiftNumber"] == "0" {
			t.Errorf("non-shift row leaked into shifts.csv: %v", r)
		}
	}
}

func TestRunDirSource(t *testing.T) {
	// A local mirror of `aws s3 sync s3://bucket/raw ./raw`: same keys, so
	// the same rows, regardless of whether the tree is rooted at raw/.
	a := archive(t)
	root := t.TempDir()
	for k, b := range a.Objects {
		p := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(k, "raw/")))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, errA, fromArchive := run(t, a, Options{Prefix: "raw/"})
	_, errD, fromDir := run(t, DirSource{Root: root}, Options{})
	if errA != nil || errD != nil {
		t.Fatal(errA, errD)
	}
	for name, pair := range map[string][2][][]string{
		"games":  {fromArchive.games, fromDir.games},
		"plays":  {fromArchive.plays, fromDir.plays},
		"shifts": {fromArchive.shifts, fromDir.shifts},
	} {
		if len(pair[0]) != len(pair[1]) {
			t.Fatalf("%s: %d rows from archive, %d from dir", name, len(pair[0]), len(pair[1]))
		}
		for i := range pair[0] {
			if strings.Join(pair[0][i], ",") != strings.Join(pair[1][i], ",") {
				t.Errorf("%s row %d differs:\n%v\n%v", name, i, pair[0][i], pair[1][i])
			}
		}
	}
}

func TestFilters(t *testing.T) {
	a := archive(t)
	cases := []struct {
		opts Options
		want []string
	}{
		{Options{Prefix: "raw/"}, []string{"1917020001", "2025020001"}},
		{Options{Prefix: "raw/20252026/"}, []string{"2025020001"}},
		{Options{Prefix: "raw/", Season: 19171918}, []string{"1917020001"}},
		{Options{Prefix: "raw/", Season: 20252026, Date: "2025-10-07"}, []string{"2025020001"}},
		{Options{Prefix: "raw/", Season: 20252026, Date: "2025-10-08"}, nil},
		{Options{Prefix: "raw/", Season: 20002001}, nil},
	}
	for _, c := range cases {
		_, err, tb := run(t, a, c.opts)
		if err != nil {
			t.Fatalf("%+v: %v", c.opts, err)
		}
		var got []string
		for _, rec := range tb.games[1:] {
			got = append(got, rec[0])
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%+v: games %v, want %v", c.opts, got, c.want)
		}
	}
}

func TestBadGameIsSkipped(t *testing.T) {
	a := archive(t)
	broken := "raw/20252026/2025-10-08/2025020002/"
	a.Put(context.Background(), broken+"final/pbp.json", []byte(`{"id":`))
	a.Put(context.Background(), broken+"final/shifts.json", []byte(`not json`))
	// A shift chart that does not parse costs only its rows.
	a.Put(context.Background(), modernGame+"final/shifts.json", []byte(`not json`))

	st, err, tb := run(t, a, Options{Prefix: "raw/"})
	if !errors.Is(err, ErrGamesFailed) {
		t.Fatalf("err = %v, want ErrGamesFailed", err)
	}
	if st.Games != 2 || st.Failed != 1 || st.Shifts != 0 {
		t.Errorf("stats %v", st)
	}
	if len(tb.games) != 3 || len(tb.shifts) != 1 {
		t.Errorf("games rows %d, shifts rows %d", len(tb.games), len(tb.shifts))
	}
}

func TestNilWritersSkipTables(t *testing.T) {
	var g bytes.Buffer
	st, err := Run(context.Background(), archive(t), Options{Prefix: "raw/"}, Writers{Games: &g})
	if err != nil {
		t.Fatal(err)
	}
	// Counts are still reported so games.csv's shifts column is right.
	if st.Shifts != 851 || st.Plays == 0 {
		t.Errorf("stats %v", st)
	}
	if rows := parse(t, g.String()); len(rows) != 3 {
		t.Errorf("games rows = %d", len(rows))
	}
}

func TestClockSeconds(t *testing.T) {
	for in, out := range map[string]string{"00:34": "34", "20:00": "1200", "1:05": "65", "": "", "x:y": "", "00:60": ""} {
		if got := clockSeconds(in); got != out {
			t.Errorf("clockSeconds(%q) = %q, want %q", in, got, out)
		}
	}
}
