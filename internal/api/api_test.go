package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"

	"hockeytrack/internal/lake"
	"hockeytrack/internal/schedsync"
	"hockeytrack/internal/store"
)

var testNow = time.Date(2025, 10, 8, 12, 0, 0, 0, time.UTC)

func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", path))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fixture builds the three stores the API reads: a raw archive with one
// fully swept 2025 game, six 2024 regular-season finals, one live game and
// two standings snapshots; an index built from it by the real Indexer;
// and a schedule document with past, live and future games.
func fixture(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	raw := store.NewFakeArchive()
	index := store.NewFakeArchive()
	site := store.NewFakeArchive()

	pbp := read(t, "internal/nhl/testdata/pbp.json")
	final := "raw/20252026/2025-10-07/2025020001/"
	_ = raw.Put(ctx, final+"final/pbp.json", pbp)
	_ = raw.Put(ctx, final+"final/boxscore.json", read(t, "internal/nhl/testdata/boxscore.json"))
	_ = raw.Put(ctx, final+"final/landing.json", read(t, "internal/nhl/testdata/landing.json"))
	_ = raw.Put(ctx, final+"final/shifts.json", read(t, "internal/nhl/testdata/shifts.json"))

	live := strings.Replace(string(pbp), `"id":2025020001,"season"`, `"id":2025020002,"season"`, 1)
	live = strings.Replace(live, `"gameState":"OFF"`, `"gameState":"LIVE"`, 1)
	if !strings.Contains(live, "2025020002") || !strings.Contains(live, `"LIVE"`) {
		t.Fatal("live fixture substitution failed")
	}
	_ = raw.Put(ctx, "raw/20252026/2025-10-08/2025020002/pbp/20251008T230000Z.json", []byte(live))
	_ = raw.Put(ctx, "raw/20252026/2025-10-08/2025020002/pbp/20251008T225500Z.json", pbp)

	for _, id := range []string{"2024021292", "2024021294", "2024021297", "2024021298", "2024021299", "2024021300"} {
		_ = raw.Put(ctx, "raw/20242025/2025-04-15/"+id+"/final/pbp.json", read(t, "internal/synth/testdata/"+id+".json"))
	}
	standings := read(t, "internal/nhl/testdata/standings.json")
	_ = raw.Put(ctx, store.StandingsKey("2025-10-01"), standings)
	_ = raw.Put(ctx, store.StandingsKey("2025-10-08"), standings)
	_ = raw.Put(ctx, "raw/schedule/2025-10-07.json", []byte("{}"))

	ix := &lake.Indexer{Raw: raw, Index: index, Now: func() time.Time { return testNow }}
	for _, season := range []int64{20242025, 20252026} {
		if _, err := ix.SyncSeason(ctx, season); err != nil {
			t.Fatalf("index %d: %v", season, err)
		}
	}

	sched := schedsync.SitePayload{
		GeneratedAt: testNow,
		Teams:       map[string]string{"CHI": "Chicago Blackhawks", "FLA": "Florida Panthers", "TBL": "Tampa Bay Lightning", "BOS": "Boston Bruins"},
		Games: []schedsync.SiteGame{
			{ID: 2025020001, Date: "2025-10-07", Start: time.Date(2025, 10, 7, 21, 0, 0, 0, time.UTC), Away: "CHI", Home: "FLA", Type: 2, Venue: "Amerant Bank Arena"},
			{ID: 2025020002, Date: "2025-10-08", Start: time.Date(2025, 10, 8, 23, 0, 0, 0, time.UTC), Away: "CHI", Home: "FLA", Type: 2, Venue: "Amerant Bank Arena"},
			{ID: 2025020500, Date: "2026-01-15", Start: time.Date(2026, 1, 16, 0, 0, 0, 0, time.UTC), Away: "TBL", Home: "BOS", Type: 2, Venue: "TD Garden"},
			{ID: 2025020600, Date: "2026-02-01", Start: time.Date(2026, 2, 1, 18, 0, 0, 0, time.UTC), Away: "FLA", Home: "TBL", Type: 2, Venue: "Amalie Arena"},
			{ID: 2025010050, Date: "2025-09-25", Start: time.Date(2025, 9, 25, 23, 0, 0, 0, time.UTC), Away: "BOS", Home: "TBL", Type: 1, Venue: "Amalie Arena"},
		},
	}
	body, _ := json.Marshal(sched)
	_ = site.Put(ctx, schedsync.SiteKey, body)

	return New(Deps{Raw: raw, Index: index, Site: site, Now: func() time.Time { return testNow }, Version: "test"})
}

// get performs a request and decodes a 200's JSON body into a fresh value;
// a fresh value, not a reused one, so an omitted field cannot inherit the
// previous response's.
func get[T any](t *testing.T, s *Server, path string) (T, *httptest.ResponseRecorder) {
	t.Helper()
	var out T
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK && rec.Header().Get("Content-Type") == "application/json; charset=utf-8" {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: bad JSON: %v\n%s", path, err, rec.Body.String())
		}
	}
	return out, rec
}

func list(t *testing.T, s *Server, path string) listPage {
	t.Helper()
	p, rec := get[listPage](t, s, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status %d: %s", path, rec.Code, rec.Body.String())
	}
	return p
}

func wantStatus(t *testing.T, s *Server, path string, status int) {
	t.Helper()
	_, rec := get[json.RawMessage](t, s, path)
	if rec.Code != status {
		t.Errorf("%s: status %d, want %d: %s", path, rec.Code, status, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/json; charset=utf-8" && rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("%s: content type %q", path, rec.Header().Get("Content-Type"))
	}
}

type listPage struct {
	Season int64             `json:"season"`
	Total  int               `json:"total"`
	Limit  int               `json:"limit"`
	Offset int               `json:"offset"`
	Items  []json.RawMessage `json:"items"`
}

func items[T any](t *testing.T, p listPage) []T {
	t.Helper()
	out := make([]T, 0, len(p.Items))
	for _, raw := range p.Items {
		var v T
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func TestRootAndMisc(t *testing.T) {
	s := fixture(t)
	root, rec := get[struct {
		Season  int64             `json:"season"`
		Seasons int               `json:"seasons"`
		Version string            `json:"version"`
		Links   map[string]string `json:"links"`
	}](t, s, "/v1")
	if rec.Code != 200 || root.Season != 20252026 || root.Seasons != 2 || root.Version != "test" || root.Links["schedule"] == "" {
		t.Errorf("root = %+v (%d)", root, rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=300" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("no CORS header")
	}
	wantStatus(t, s, "/v1/", 200)
	wantStatus(t, s, "/healthz", 200)
	spec, rec := get[map[string]any](t, s, "/v1/openapi.json")
	if rec.Code != 200 || spec["openapi"] != "3.1.0" || len(spec["paths"].(map[string]any)) < 20 {
		t.Error("openapi document incomplete")
	}
	wantStatus(t, s, "/v1/nope", 404)
	wantStatus(t, s, "/", 404)
	req := httptest.NewRequest(http.MethodPost, "/v1/games/2025020001", nil)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rec.Code)
	}

	if seasons := list(t, s, "/v1/seasons"); seasons.Total != 2 {
		t.Errorf("seasons = %+v", seasons)
	}
	meta, _ := get[seasonMeta](t, s, "/v1/seasons/20252026")
	if meta.Games != 1 || meta.FinalGames != 1 || meta.FirstDate != "2025-10-07" || len(meta.Teams) != 2 || meta.Goalies != 2 {
		t.Errorf("season meta = %+v", meta)
	}
	wantStatus(t, s, "/v1/seasons/20302031", 404)
	wantStatus(t, s, "/v1/seasons/abc", 400)
	wantStatus(t, s, "/v1/games?season=2025", 400)
	wantStatus(t, s, "/v1/games?season=20302031", 404)
}

func TestSchedule(t *testing.T) {
	s := fixture(t)
	p := list(t, s, "/v1/schedule?date=2025-10-07")
	rows := items[scheduleGame](t, p)
	if p.Total != 1 || rows[0].ID != 2025020001 || rows[0].Result == nil || rows[0].Result.AwayScore != 2 || rows[0].Result.HomeScore != 3 || rows[0].HomeName != "Florida Panthers" {
		t.Errorf("schedule on a date = %+v", rows)
	}
	p = list(t, s, "/v1/schedule?date=today")
	if p.Total != 1 || items[scheduleGame](t, p)[0].ID != 2025020002 {
		t.Errorf("today = %+v", p)
	}
	p = list(t, s, "/v1/schedule?team=tbl")
	rows = items[scheduleGame](t, p)
	if p.Total != 3 || rows[0].ID != 2025010050 || rows[1].Result != nil || rows[2].Home != "TBL" {
		t.Errorf("team filter = %+v", rows)
	}
	if p = list(t, s, "/v1/schedule?team=TBL&type=2"); p.Total != 2 {
		t.Errorf("type filter = %d", p.Total)
	}
	p = list(t, s, "/v1/schedule?from=2026-01-01&to=2026-01-31")
	if p.Total != 1 || items[scheduleGame](t, p)[0].ID != 2025020500 {
		t.Errorf("range = %+v", p)
	}
	if p = list(t, s, "/v1/schedule?venue=amalie"); p.Total != 2 {
		t.Errorf("venue = %d", p.Total)
	}
	p = list(t, s, "/v1/schedule?limit=2&offset=1")
	if p.Total != 5 || len(p.Items) != 2 || p.Offset != 1 || p.Limit != 2 {
		t.Errorf("paging = %+v", p)
	}
	p = list(t, s, "/v1/schedule?season=20252026")
	if p.Total != 5 || p.Season != 20252026 {
		t.Errorf("season filter = %+v", p)
	}
	wantStatus(t, s, "/v1/schedule?date=yesterday", 400)
	wantStatus(t, s, "/v1/schedule?team=1", 400)
	wantStatus(t, s, "/v1/schedule?limit=0", 400)
	wantStatus(t, s, "/v1/schedule?offset=-1", 400)

	// Games the schedule document no longer lists come from the index:
	// schedule-sync publishes from today forward, so last week's games and
	// every past season exist only as rollup rows.
	p = list(t, s, "/v1/schedule?season=20242025")
	rows = items[scheduleGame](t, p)
	if p.Total != 6 || len(rows) == 0 || rows[0].Result == nil || rows[0].Season != 20242025 || rows[0].Date != "2025-04-15" || rows[0].Away == "" || rows[0].HomeName == "" {
		t.Fatalf("past season from the index = %d %+v", p.Total, rows)
	}
	away := rows[0].Away
	p = list(t, s, "/v1/schedule?date=2025-04-15&team="+away)
	if p.Total < 1 || items[scheduleGame](t, p)[0].Result == nil {
		t.Errorf("past date and team from the index = %+v", p)
	}
	if p = list(t, s, "/v1/schedule?from=2025-04-01&to=2025-10-07"); p.Total != 8 {
		t.Errorf("range spanning two seasons = %d, want 6 indexed + 2 scheduled", p.Total)
	}
	// A game in both is listed once.
	if p = list(t, s, "/v1/schedule?date=2025-10-07"); p.Total != 1 {
		t.Errorf("game in document and index = %d", p.Total)
	}

	// Without a schedule document the endpoint says so.
	noSite := New(Deps{Raw: store.NewFakeArchive(), Index: store.NewFakeArchive(), Now: func() time.Time { return testNow }})
	wantStatus(t, noSite, "/v1/schedule", 404)
}

// "today" is a UTC date unless the caller names a time zone: at 02:00 UTC
// on 8 October it is still the evening of the 7th in New York.
func TestScheduleTimeZone(t *testing.T) {
	s := fixture(t)
	s.deps.Now = func() time.Time { return time.Date(2025, 10, 8, 2, 0, 0, 0, time.UTC) }
	p := list(t, s, "/v1/schedule?date=today")
	if p.Total != 1 || items[scheduleGame](t, p)[0].ID != 2025020002 {
		t.Errorf("today in UTC = %+v", p)
	}
	p = list(t, s, "/v1/schedule?date=today&tz=America/New_York")
	if p.Total != 1 || items[scheduleGame](t, p)[0].ID != 2025020001 {
		t.Errorf("today in New York = %+v", p)
	}
	if p = list(t, s, "/v1/schedule?from=today&to=today&tz=America/New_York"); p.Total != 1 {
		t.Errorf("from/to today in New York = %d", p.Total)
	}
	// An explicit date is a date; tz does not shift it.
	p = list(t, s, "/v1/schedule?date=2025-10-08&tz=Pacific/Kiritimati")
	if p.Total != 1 || items[scheduleGame](t, p)[0].ID != 2025020002 {
		t.Errorf("explicit date with tz = %+v", p)
	}
	for _, bad := range []string{"Mars/Olympus", "../../etc/passwd", "Local", "/usr/share/zoneinfo/UTC", strings.Repeat("A", 65)} {
		wantStatus(t, s, "/v1/schedule?date=today&tz="+bad, 400)
	}
	wantStatus(t, s, "/v1/standings?date=today&tz=Nowhere/Here", 400)
}

func TestTeams(t *testing.T) {
	s := fixture(t)
	p := list(t, s, "/v1/teams")
	teams := items[teamRef](t, p)
	if len(teams) != 4 || teams[0].Abbrev != "BOS" || teams[2].Name != "Florida Panthers" {
		t.Errorf("teams = %+v", teams)
	}
	card, rec := get[teamCard](t, s, "/v1/teams/fla")
	if rec.Code != 200 || card.Abbrev != "FLA" || card.Record == nil || card.Record.Wins != 1 || card.Standings == nil ||
		card.LastGame == nil || card.LastGame.ID != 2025020001 || card.NextGame == nil || card.NextGame.ID != 2025020002 {
		t.Errorf("FLA card = %+v (%d)", card, rec.Code)
	}
	card, _ = get[teamCard](t, s, "/v1/teams/BOS")
	if card.Record != nil || card.LastGame != nil || card.NextGame == nil || card.NextGame.ID != 2025020500 {
		t.Errorf("BOS card = %+v", card)
	}
	wantStatus(t, s, "/v1/teams/ZZZ", 404)
	wantStatus(t, s, "/v1/teams/F1", 400)

	if p = list(t, s, "/v1/teams/FLA/games"); p.Total != 3 || p.Season != 20252026 {
		t.Errorf("FLA games = %+v", p)
	}
	p = list(t, s, "/v1/teams/CGY/games?season=20242025")
	if p.Total != 1 || items[scheduleGame](t, p)[0].Result.LastPeriodType != "SO" {
		t.Errorf("CGY games come from the index: %+v", p)
	}
	wantStatus(t, s, "/v1/teams/CGY/games", 404)

	p = list(t, s, "/v1/teams/FLA/roster")
	roster := items[rosterEntry](t, p)
	if len(roster) < 18 || roster[len(roster)-1].Position != "G" || roster[0].Position == "G" {
		t.Errorf("roster = %d entries, first %+v, last %+v", len(roster), roster[0], roster[len(roster)-1])
	}
	wantStatus(t, s, "/v1/teams/BOS/roster", 404)
}

type standingsResponse struct {
	Date     string         `json:"date"`
	Season   int64          `json:"season"`
	Archived string         `json:"archived"`
	Total    int            `json:"total"`
	Items    []standingsRow `json:"items"`
}

func TestStandings(t *testing.T) {
	s := fixture(t)
	res, rec := get[standingsResponse](t, s, "/v1/standings")
	if rec.Code != 200 || res.Archived != "2025-10-08" || res.Date != "2026-04-17" || res.Season != 20252026 || res.Total != 32 ||
		res.Items[0].LeagueRank != 1 || res.Items[0].Abbrev != "COL" || res.Items[0].HomeRecord == "" {
		t.Errorf("standings = %s %s %d %d %+v", res.Archived, res.Date, res.Season, res.Total, res.Items[0])
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=3600" {
		t.Errorf("Cache-Control = %q", cc)
	}
	res, rec = get[standingsResponse](t, s, "/v1/standings?date=2025-10-05")
	if res.Archived != "2025-10-01" || rec.Header().Get("Cache-Control") != "public, max-age=86400" {
		t.Errorf("on a date = %s %q", res.Archived, rec.Header().Get("Cache-Control"))
	}
	if res, _ = get[standingsResponse](t, s, "/v1/standings?division=atlantic"); res.Total != 8 {
		t.Errorf("atlantic = %d", res.Total)
	}
	if res, _ = get[standingsResponse](t, s, "/v1/standings?conference=Western"); res.Total != 16 {
		t.Errorf("western = %d", res.Total)
	}
	wantStatus(t, s, "/v1/standings?date=2025-09-01", 404)
	wantStatus(t, s, "/v1/standings?date=soon", 400)
}

func TestGames(t *testing.T) {
	s := fixture(t)
	if p := list(t, s, "/v1/games"); p.Total != 1 || p.Season != 20252026 {
		t.Errorf("games = %+v", p)
	}
	p := list(t, s, "/v1/games?season=20242025&team=cgy")
	if p.Total != 1 || items[lake.GameRow](t, p)[0].ID != 2024021299 {
		t.Errorf("games by team = %+v", p)
	}
	if p = list(t, s, "/v1/games?season=20242025&date=2025-04-15&type=2"); p.Total != 6 {
		t.Errorf("games by date = %d", p.Total)
	}

	g, rec := get[lake.GameSummary](t, s, "/v1/games/2025020001")
	if rec.Code != 200 || g.Game.ID != 2025020001 || g.Home.Result != "W" || len(g.Skaters) < 30 || len(g.Stars) != 3 || g.Game.Live {
		t.Errorf("game = %+v (%d)", g.Game, rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=86400" {
		t.Errorf("final Cache-Control = %q", cc)
	}
	g, rec = get[lake.GameSummary](t, s, "/v1/games/2025020002")
	if rec.Code != 200 || g.Game.ID != 2025020002 || !g.Game.Live || g.Game.State != "LIVE" || g.Home.Result != "" {
		t.Errorf("live game = %+v (%d)", g.Game, rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=10" {
		t.Errorf("live Cache-Control = %q", cc)
	}
	wantStatus(t, s, "/v1/games/2025029999", 404)
	wantStatus(t, s, "/v1/games/abc", 400)
	wantStatus(t, s, "/v1/games/0", 400)

	box, _ := get[struct {
		Skaters []lake.SkaterLine `json:"skaters"`
		Goalies []lake.GoalieLine `json:"goalies"`
	}](t, s, "/v1/games/2025020001/boxscore")
	// The boxscore lists every dressed goalie, backups included.
	if len(box.Goalies) != 4 || len(box.Skaters) < 30 {
		t.Errorf("boxscore: %d skaters %d goalies", len(box.Skaters), len(box.Goalies))
	}
}

func TestPlaysShiftsRaw(t *testing.T) {
	s := fixture(t)
	if p := list(t, s, "/v1/games/2025020001/plays?type=goal"); p.Total != 5 {
		t.Errorf("goals = %d", p.Total)
	}
	p := list(t, s, "/v1/games/2025020001/plays?type=goal,penalty&team=chi")
	for _, pl := range items[lake.Play](t, p) {
		if pl.Team != "CHI" || (pl.Type != "goal" && pl.Type != "penalty") {
			t.Errorf("filter leaked %+v", pl)
		}
	}
	if p.Total == 0 {
		t.Error("no CHI goals or penalties")
	}
	if p = list(t, s, "/v1/games/2025020001/plays?player=8483493&period=1"); p.Total == 0 {
		t.Error("Nazar has no first-period plays")
	}
	p, rec := get[listPage](t, s, "/v1/games/2025020001/plays?limit=2&offset=1")
	if p.Total != 361 || len(p.Items) != 2 || p.Offset != 1 || rec.Header().Get("Cache-Control") != "public, max-age=86400" {
		t.Errorf("paging = total %d items %d %q", p.Total, len(p.Items), rec.Header().Get("Cache-Control"))
	}
	p, rec = get[listPage](t, s, "/v1/games/2025020002/plays?type=goal")
	if p.Total != 5 || rec.Header().Get("Cache-Control") != "public, max-age=10" {
		t.Errorf("live plays = %d %q", p.Total, rec.Header().Get("Cache-Control"))
	}
	wantStatus(t, s, "/v1/games/2025020001/plays?period=first", 400)
	wantStatus(t, s, "/v1/games/2025029999/plays", 404)

	p = list(t, s, "/v1/games/2025020001/shifts?player=8473419")
	shifts := items[lake.Shift](t, p)
	if p.Total == 0 || shifts[0].Name != "Brad Marchand" {
		t.Errorf("shifts = %+v", p)
	}
	p = list(t, s, "/v1/games/2025020001/shifts?team=FLA&period=3")
	for _, sh := range items[lake.Shift](t, p) {
		if sh.Team != "FLA" || sh.Period != 3 {
			t.Errorf("shift filter leaked %+v", sh)
		}
	}
	wantStatus(t, s, "/v1/games/2024021299/shifts", 404)

	_, rec = get[json.RawMessage](t, s, "/v1/games/2025020001/raw/pbp")
	if rec.Code != 200 || rec.Body.Len() != len(read(t, "internal/nhl/testdata/pbp.json")) ||
		rec.Header().Get("X-Archive-Key") != "raw/20252026/2025-10-07/2025020001/final/pbp.json" ||
		rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("raw pbp: %d %d %q", rec.Code, rec.Body.Len(), rec.Header().Get("X-Archive-Key"))
	}
	_, rec = get[json.RawMessage](t, s, "/v1/games/2025020002/raw/pbp")
	if rec.Header().Get("X-Archive-Key") != "raw/20252026/2025-10-08/2025020002/pbp/20251008T230000Z.json" {
		t.Errorf("live raw picks the newest snapshot: %q", rec.Header().Get("X-Archive-Key"))
	}
	wantStatus(t, s, "/v1/games/2025020001/raw/bogus", 400)
	wantStatus(t, s, "/v1/games/2024021299/raw/shifts", 404)
}

func TestStats(t *testing.T) {
	s := fixture(t)
	p := list(t, s, "/v1/stats/skaters?season=20242025&sort=goals&limit=3")
	rows := items[skaterRow](t, p)
	if len(rows) != 3 || rows[0].Goals < rows[1].Goals || rows[1].Goals < rows[2].Goals || p.Total < 100 {
		t.Errorf("skaters by goals = %+v (total %d)", rows, p.Total)
	}
	p = list(t, s, "/v1/stats/skaters?season=20242025&sort=goals&order=asc&limit=1")
	if items[skaterRow](t, p)[0].Goals != 0 {
		t.Error("ascending order")
	}
	p = list(t, s, "/v1/stats/skaters?season=20252026&team=chi&position=D")
	for _, r := range items[skaterRow](t, p) {
		if r.Position != "D" || len(r.Teams) != 1 || r.Teams[0] != "CHI" {
			t.Errorf("filter leaked %+v", r)
		}
	}
	p = list(t, s, "/v1/stats/skaters?season=20252026&position=F&sort=faceoffPct&minGp=1")
	if p.Total == 0 || items[skaterRow](t, p)[0].FaceoffPct == 0 {
		t.Error("faceoff pct sort")
	}
	p = list(t, s, "/v1/stats/skaters?season=20252026&sort=points&limit=1")
	if top := items[skaterRow](t, p)[0]; top.GP != 1 || top.Points < 2 || top.TOIPerGame == 0 {
		t.Errorf("top scorer = %+v", top)
	}
	wantStatus(t, s, "/v1/stats/skaters?sort=bogus", 400)
	wantStatus(t, s, "/v1/stats/skaters?order=sideways", 400)
	wantStatus(t, s, "/v1/stats/skaters?position=X", 400)

	p = list(t, s, "/v1/stats/goalies?season=20252026")
	goalies := items[goalieRow](t, p)
	if len(goalies) != 2 || goalies[0].Wins != 1 || goalies[1].Losses != 1 || goalies[1].SavePct < 0.9 || goalies[1].GAA == 0 {
		t.Errorf("goalies = %+v", goalies)
	}
	if p = list(t, s, "/v1/stats/goalies?season=20252026&sort=savePct&minGp=2"); p.Total != 0 {
		t.Errorf("minGp = %d", p.Total)
	}

	p = list(t, s, "/v1/stats/teams?season=20242025")
	teams := items[teamRow](t, p)
	if len(teams) != 12 || teams[0].Points < teams[len(teams)-1].Points || teams[0].PointPct == 0 {
		t.Errorf("teams = %d, first %+v", len(teams), teams[0])
	}
	p = list(t, s, "/v1/stats/teams?season=20242025&sort=goalsAgainst&order=asc")
	if teams = items[teamRow](t, p); teams[0].GoalsAgainst > teams[1].GoalsAgainst {
		t.Error("goalsAgainst ascending")
	}
}

func TestPlayers(t *testing.T) {
	s := fixture(t)
	card, rec := get[playerCard](t, s, "/v1/players/8483493")
	if rec.Code != 200 || card.Name != "Frank Nazar" || len(card.Teams) != 1 || card.Teams[0] != "CHI" || card.Skater == nil || card.Skater.Goals != 1 || card.Goalie != nil {
		t.Errorf("Nazar = %+v (%d)", card, rec.Code)
	}
	card, _ = get[playerCard](t, s, "/v1/players/8481519")
	if card.Goalie == nil || card.Goalie.Losses != 1 || card.Position != "G" {
		t.Errorf("Knight = %+v", card)
	}
	wantStatus(t, s, "/v1/players/1", 404)
	wantStatus(t, s, "/v1/players/8483493?season=20242025", 404)
	wantStatus(t, s, "/v1/players/x", 400)

	p := list(t, s, "/v1/players/8483493/games")
	log := items[gameLogEntry](t, p)
	if p.Total != 1 || log[0].Game.ID != 2025020001 || log[0].Skater == nil || log[0].Skater.Goals != 1 {
		t.Errorf("game log = %+v", log)
	}
	p = list(t, s, "/v1/players/8481519/games")
	if log := items[gameLogEntry](t, p); len(log) != 1 || log[0].Goalie == nil {
		t.Errorf("goalie log = %+v", log)
	}
}

func TestSearch(t *testing.T) {
	s := fixture(t)
	res, _ := get[searchResult](t, s, "/v1/search?q=nazar")
	if len(res.Players) != 1 || res.Players[0].Label != "Frank Nazar" || res.Players[0].Href != "/v1/players/8483493?season=20252026" {
		t.Errorf("player search = %+v", res.Players)
	}
	if res, _ = get[searchResult](t, s, "/v1/search?q=Panthers"); len(res.Teams) != 1 || res.Teams[0].ID != "FLA" {
		t.Errorf("team search = %+v", res.Teams)
	}
	if res, _ = get[searchResult](t, s, "/v1/search?q=amalie"); len(res.Venues) != 1 || res.Venues[0].Href != "/v1/schedule?venue=Amalie+Arena" {
		t.Errorf("venue search = %+v", res.Venues)
	}
	if res, _ = get[searchResult](t, s, "/v1/search?q=2025020001"); len(res.Games) != 1 || res.Games[0].Detail != "2-3" {
		t.Errorf("game id search = %+v", res.Games)
	}
	if res, _ = get[searchResult](t, s, "/v1/search?q=2025-04-15&season=20242025"); len(res.Games) != 6 {
		t.Errorf("date search = %d", len(res.Games))
	}
	if res, _ = get[searchResult](t, s, "/v1/search?q=chi@fla"); len(res.Games) != 2 {
		t.Errorf("matchup search = %+v", res.Games)
	}
	if res, _ = get[searchResult](t, s, "/v1/search?q=bob"); len(res.Players) == 0 {
		t.Error("goalie search")
	}
	wantStatus(t, s, "/v1/search?q=a", 400)
	wantStatus(t, s, "/v1/search", 400)
}

func TestLambdaURLAdapter(t *testing.T) {
	s := fixture(t)
	h := LambdaURLHandler(s)
	ev := events.LambdaFunctionURLRequest{
		RawPath:        "/v1/games/2025020001/plays",
		RawQueryString: "type=goal&limit=1",
		Headers:        map[string]string{"host": "api.example.test", "accept": "application/json"},
	}
	ev.RequestContext.HTTP.Method = "GET"
	resp, err := h(context.Background(), ev)
	if err != nil || resp.StatusCode != 200 || resp.IsBase64Encoded {
		t.Fatalf("adapter: %v %+v", err, resp)
	}
	var p listPage
	if err := json.Unmarshal([]byte(resp.Body), &p); err != nil || p.Total != 5 || len(p.Items) != 1 {
		t.Errorf("body = %s", resp.Body)
	}
	if resp.Headers["Content-Type"] != "application/json; charset=utf-8" || resp.Headers["Cache-Control"] == "" {
		t.Errorf("headers = %v", resp.Headers)
	}
	ev.RawPath = "/v1/games/1/plays"
	resp, _ = h(context.Background(), ev)
	if resp.StatusCode != 404 {
		t.Errorf("404 path: %d", resp.StatusCode)
	}
	ev.RawPath, ev.RawQueryString = "/healthz", ""
	ev.RequestContext.HTTP.Method = ""
	if resp, _ = h(context.Background(), ev); resp.StatusCode != 200 {
		t.Errorf("default method: %d", resp.StatusCode)
	}
}

func TestMemo(t *testing.T) {
	now := testNow
	m := newMemo(func() time.Time { return now })
	calls := 0
	load := func() (any, error) { calls++; return calls, nil }
	v, _ := m.get("k", time.Minute, load)
	v2, _ := m.get("k", time.Minute, load)
	if v != 1 || v2 != 1 || calls != 1 {
		t.Errorf("not cached: %v %v %d", v, v2, calls)
	}
	now = now.Add(2 * time.Minute)
	if v, _ := m.get("k", time.Minute, load); v != 2 {
		t.Errorf("not expired: %v", v)
	}
	// A miss is remembered briefly; a transport error is not.
	misses := 0
	_, err := m.get("missing", time.Hour, func() (any, error) { misses++; return nil, fmt.Errorf("x: %w", errNotFound) })
	_, err2 := m.get("missing", time.Hour, func() (any, error) { misses++; return nil, errNotFound })
	if !errors.Is(err, errNotFound) || !errors.Is(err2, errNotFound) || misses != 1 {
		t.Errorf("negative cache: %v %v %d", err, err2, misses)
	}
	fails := 0
	_, _ = m.get("flaky", time.Hour, func() (any, error) { fails++; return nil, errors.New("boom") })
	_, _ = m.get("flaky", time.Hour, func() (any, error) { fails++; return nil, errors.New("boom") })
	if fails != 2 {
		t.Errorf("transport errors cached: %d", fails)
	}
}
