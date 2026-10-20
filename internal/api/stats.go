package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"hockeytrack/internal/lake"
)

// skaterRow is a leaderboard line: a player's totals across the teams
// they played for, with the derived rates a table shows.
type skaterRow struct {
	PlayerID int64    `json:"playerId"`
	Name     string   `json:"name"`
	Teams    []string `json:"teams"`
	Number   int      `json:"number,omitempty"`
	Position string   `json:"position,omitempty"`
	GP       int      `json:"gp"`
	lake.SkaterStats
	ShootingPct   float64 `json:"shootingPct"`
	FaceoffPct    float64 `json:"faceoffPct"`
	PointsPerGame float64 `json:"pointsPerGame"`
	TOIPerGame    int     `json:"toiPerGameSeconds"`
}

func (r *skaterRow) derive() {
	if r.SOG > 0 {
		r.ShootingPct = round3(float64(r.Goals) / float64(r.SOG))
	}
	if fo := r.FaceoffWins + r.FaceoffLosses; fo > 0 {
		r.FaceoffPct = round3(float64(r.FaceoffWins) / float64(fo))
	}
	if r.GP > 0 {
		r.PointsPerGame = round3(float64(r.Points) / float64(r.GP))
		r.TOIPerGame = r.TOISeconds / r.GP
	}
}

func round3(f float64) float64 { return float64(int(f*1000+0.5)) / 1000 }

// skaterSortKeys maps sort= values to the column they order by.
var skaterSortKeys = map[string]func(r skaterRow) float64{
	"goals":         func(r skaterRow) float64 { return float64(r.Goals) },
	"assists":       func(r skaterRow) float64 { return float64(r.Assists) },
	"points":        func(r skaterRow) float64 { return float64(r.Points) },
	"plusMinus":     func(r skaterRow) float64 { return float64(r.PlusMinus) },
	"pim":           func(r skaterRow) float64 { return float64(r.PIM) },
	"hits":          func(r skaterRow) float64 { return float64(r.Hits) },
	"sog":           func(r skaterRow) float64 { return float64(r.SOG) },
	"blocked":       func(r skaterRow) float64 { return float64(r.Blocked) },
	"ppGoals":       func(r skaterRow) float64 { return float64(r.PPGoals) },
	"giveaways":     func(r skaterRow) float64 { return float64(r.Giveaways) },
	"takeaways":     func(r skaterRow) float64 { return float64(r.Takeaways) },
	"shifts":        func(r skaterRow) float64 { return float64(r.Shifts) },
	"toi":           func(r skaterRow) float64 { return float64(r.TOISeconds) },
	"faceoffWins":   func(r skaterRow) float64 { return float64(r.FaceoffWins) },
	"faceoffPct":    func(r skaterRow) float64 { return r.FaceoffPct },
	"shootingPct":   func(r skaterRow) float64 { return r.ShootingPct },
	"pointsPerGame": func(r skaterRow) float64 { return r.PointsPerGame },
	"toiPerGame":    func(r skaterRow) float64 { return float64(r.TOIPerGame) },
	"gp":            func(r skaterRow) float64 { return float64(r.GP) },
}

// sortSpec reads sort= and order=, validated against the allowed keys.
func sortSpec[T any](r *http.Request, keys map[string]func(T) float64, def string) (func(T) float64, bool, error) {
	q := r.URL.Query()
	name := q.Get("sort")
	if name == "" {
		name = def
	}
	key, ok := keys[name]
	if !ok {
		names := make([]string, 0, len(keys))
		for k := range keys {
			names = append(names, k)
		}
		sort.Strings(names)
		return nil, false, badRequest("sort must be one of %s", strings.Join(names, ", "))
	}
	desc := true
	switch q.Get("order") {
	case "", "desc":
	case "asc":
		desc = false
	default:
		return nil, false, badRequest("order must be asc or desc")
	}
	return key, desc, nil
}

func sortRows[T any](rows []T, key func(T) float64, desc bool) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := key(rows[i]), key(rows[j])
		if desc {
			return a > b
		}
		return a < b
	})
}

// skaterRows merges a rollup's per-team rows into one per player, unless a
// team filter asks for that team's split.
func skaterRows(ro *lake.SeasonRollup, team, position string, minGP int) []skaterRow {
	byID := map[int64]*skaterRow{}
	var order []int64
	for _, sk := range ro.Skaters {
		if team != "" && sk.Team != team {
			continue
		}
		if position != "" && !positionMatches(position, sk.Position) {
			continue
		}
		row, ok := byID[sk.PlayerID]
		if !ok {
			row = &skaterRow{PlayerID: sk.PlayerID, Name: sk.Name, Number: sk.Number, Position: sk.Position}
			byID[sk.PlayerID] = row
			order = append(order, sk.PlayerID)
		}
		row.Teams = append(row.Teams, sk.Team)
		row.GP += sk.GP
		row.SkaterStats = addStats(row.SkaterStats, sk.SkaterStats)
	}
	rows := make([]skaterRow, 0, len(order))
	for _, id := range order {
		row := byID[id]
		if row.GP < minGP {
			continue
		}
		row.derive()
		rows = append(rows, *row)
	}
	return rows
}

func addStats(a, b lake.SkaterStats) lake.SkaterStats {
	a.Goals += b.Goals
	a.Assists += b.Assists
	a.Points += b.Points
	a.PlusMinus += b.PlusMinus
	a.PIM += b.PIM
	a.Hits += b.Hits
	a.SOG += b.SOG
	a.Blocked += b.Blocked
	a.PPGoals += b.PPGoals
	a.Giveaways += b.Giveaways
	a.Takeaways += b.Takeaways
	a.Shifts += b.Shifts
	a.TOISeconds += b.TOISeconds
	a.FaceoffWins += b.FaceoffWins
	a.FaceoffLosses += b.FaceoffLosses
	return a
}

// positionMatches accepts the NHL's codes (C, L, R, D) and F for any forward.
func positionMatches(want, have string) bool {
	want = strings.ToUpper(want)
	if want == "F" {
		return have == "C" || have == "L" || have == "R"
	}
	return want == have
}

func (s *Server) handleSkaters(r *http.Request) (any, time.Duration, error) {
	season, err := s.querySeason(r)
	if err != nil {
		return nil, 0, err
	}
	limit, offset, err := paging(r)
	if err != nil {
		return nil, 0, err
	}
	team, err := queryTeam(r, "team")
	if err != nil {
		return nil, 0, err
	}
	position := strings.ToUpper(r.URL.Query().Get("position"))
	if position != "" && !strings.Contains("C L R D F", position) {
		return nil, 0, badRequest("position must be one of C, L, R, D, F")
	}
	minGP, _, err := queryInt(r, "minGp")
	if err != nil {
		return nil, 0, err
	}
	key, desc, err := sortSpec(r, skaterSortKeys, "points")
	if err != nil {
		return nil, 0, err
	}
	ro, err := s.rollup(r.Context(), season)
	if err != nil {
		return nil, 0, err
	}
	rows := skaterRows(ro, team, position, minGP)
	sortRows(rows, key, desc)
	return slicePage(season, rows, limit, offset), ttlIndex, nil
}

// goalieRow is a goalie leaderboard line.
type goalieRow struct {
	PlayerID     int64    `json:"playerId"`
	Name         string   `json:"name"`
	Teams        []string `json:"teams"`
	Number       int      `json:"number,omitempty"`
	GP           int      `json:"gp"`
	Starts       int      `json:"starts"`
	Wins         int      `json:"wins"`
	Losses       int      `json:"losses"`
	OTLosses     int      `json:"otLosses"`
	Shutouts     int      `json:"shutouts"`
	ShotsAgainst int      `json:"shotsAgainst"`
	Saves        int      `json:"saves"`
	GoalsAgainst int      `json:"goalsAgainst"`
	TOISeconds   int      `json:"toiSeconds"`
	SavePct      float64  `json:"savePct"`
	GAA          float64  `json:"gaa"`
}

func (g *goalieRow) derive() {
	if g.ShotsAgainst > 0 {
		g.SavePct = round3(float64(g.Saves) / float64(g.ShotsAgainst))
	}
	if g.TOISeconds > 0 {
		g.GAA = float64(int(float64(g.GoalsAgainst)*3600/float64(g.TOISeconds)*100+0.5)) / 100
	}
}

var goalieSortKeys = map[string]func(r goalieRow) float64{
	"wins":         func(r goalieRow) float64 { return float64(r.Wins) },
	"losses":       func(r goalieRow) float64 { return float64(r.Losses) },
	"otLosses":     func(r goalieRow) float64 { return float64(r.OTLosses) },
	"gp":           func(r goalieRow) float64 { return float64(r.GP) },
	"starts":       func(r goalieRow) float64 { return float64(r.Starts) },
	"shutouts":     func(r goalieRow) float64 { return float64(r.Shutouts) },
	"savePct":      func(r goalieRow) float64 { return r.SavePct },
	"gaa":          func(r goalieRow) float64 { return r.GAA },
	"shotsAgainst": func(r goalieRow) float64 { return float64(r.ShotsAgainst) },
	"saves":        func(r goalieRow) float64 { return float64(r.Saves) },
	"goalsAgainst": func(r goalieRow) float64 { return float64(r.GoalsAgainst) },
	"toi":          func(r goalieRow) float64 { return float64(r.TOISeconds) },
}

func goalieRows(ro *lake.SeasonRollup, team string, minGP int) []goalieRow {
	byID := map[int64]*goalieRow{}
	var order []int64
	for _, g := range ro.Goalies {
		if team != "" && g.Team != team {
			continue
		}
		row, ok := byID[g.PlayerID]
		if !ok {
			row = &goalieRow{PlayerID: g.PlayerID, Name: g.Name, Number: g.Number}
			byID[g.PlayerID] = row
			order = append(order, g.PlayerID)
		}
		row.Teams = append(row.Teams, g.Team)
		row.GP += g.GP
		row.Starts += g.Starts
		row.Wins += g.Wins
		row.Losses += g.Losses
		row.OTLosses += g.OTLosses
		row.Shutouts += g.Shutouts
		row.ShotsAgainst += g.ShotsAgainst
		row.Saves += g.Saves
		row.GoalsAgainst += g.GoalsAgainst
		row.TOISeconds += g.TOISeconds
	}
	rows := make([]goalieRow, 0, len(order))
	for _, id := range order {
		row := byID[id]
		if row.GP < minGP {
			continue
		}
		row.derive()
		rows = append(rows, *row)
	}
	return rows
}

func (s *Server) handleGoalies(r *http.Request) (any, time.Duration, error) {
	season, err := s.querySeason(r)
	if err != nil {
		return nil, 0, err
	}
	limit, offset, err := paging(r)
	if err != nil {
		return nil, 0, err
	}
	team, err := queryTeam(r, "team")
	if err != nil {
		return nil, 0, err
	}
	minGP, _, err := queryInt(r, "minGp")
	if err != nil {
		return nil, 0, err
	}
	key, desc, err := sortSpec(r, goalieSortKeys, "wins")
	if err != nil {
		return nil, 0, err
	}
	ro, err := s.rollup(r.Context(), season)
	if err != nil {
		return nil, 0, err
	}
	rows := goalieRows(ro, team, minGP)
	sortRows(rows, key, desc)
	return slicePage(season, rows, limit, offset), ttlIndex, nil
}

// teamRow is a team's season line with the derived columns.
type teamRow struct {
	lake.TeamSeason
	GoalDifferential int     `json:"goalDifferential"`
	PointPct         float64 `json:"pointPct"`
	FaceoffPct       float64 `json:"faceoffPct"`
}

var teamSortKeys = map[string]func(r teamRow) float64{
	"points":           func(r teamRow) float64 { return float64(r.Points) },
	"wins":             func(r teamRow) float64 { return float64(r.Wins) },
	"losses":           func(r teamRow) float64 { return float64(r.Losses) },
	"otLosses":         func(r teamRow) float64 { return float64(r.OTLosses) },
	"gp":               func(r teamRow) float64 { return float64(r.GP) },
	"row":              func(r teamRow) float64 { return float64(r.ROW) },
	"goalsFor":         func(r teamRow) float64 { return float64(r.GoalsFor) },
	"goalsAgainst":     func(r teamRow) float64 { return float64(r.GoalsAgainst) },
	"goalDifferential": func(r teamRow) float64 { return float64(r.GoalDifferential) },
	"shotsFor":         func(r teamRow) float64 { return float64(r.ShotsFor) },
	"shotsAgainst":     func(r teamRow) float64 { return float64(r.ShotsAgainst) },
	"pim":              func(r teamRow) float64 { return float64(r.PIM) },
	"ppGoals":          func(r teamRow) float64 { return float64(r.PPGoals) },
	"hits":             func(r teamRow) float64 { return float64(r.Hits) },
	"blocked":          func(r teamRow) float64 { return float64(r.Blocked) },
	"faceoffPct":       func(r teamRow) float64 { return r.FaceoffPct },
	"pointPct":         func(r teamRow) float64 { return r.PointPct },
}

func (s *Server) handleTeamStats(r *http.Request) (any, time.Duration, error) {
	season, err := s.querySeason(r)
	if err != nil {
		return nil, 0, err
	}
	key, desc, err := sortSpec(r, teamSortKeys, "points")
	if err != nil {
		return nil, 0, err
	}
	ro, err := s.rollup(r.Context(), season)
	if err != nil {
		return nil, 0, err
	}
	rows := make([]teamRow, 0, len(ro.Teams))
	for _, t := range ro.Teams {
		row := teamRow{TeamSeason: t, GoalDifferential: t.GoalsFor - t.GoalsAgainst}
		if t.GP > 0 {
			row.PointPct = round3(float64(t.Points) / float64(2*t.GP))
		}
		if fo := t.FaceoffWins + t.FaceoffLosses; fo > 0 {
			row.FaceoffPct = round3(float64(t.FaceoffWins) / float64(fo))
		}
		rows = append(rows, row)
	}
	sortRows(rows, key, desc)
	return page{Season: season, Total: len(rows), Limit: len(rows), Items: rows}, ttlIndex, nil
}

// playerCard is GET /v1/players/{id}: the season's totals and the
// per-team splits behind them.
type playerCard struct {
	PlayerID int64               `json:"playerId"`
	Name     string              `json:"name"`
	Position string              `json:"position,omitempty"`
	Number   int                 `json:"number,omitempty"`
	Season   int64               `json:"season"`
	Teams    []string            `json:"teams"`
	Skater   *skaterRow          `json:"skater,omitempty"`
	Goalie   *goalieRow          `json:"goalie,omitempty"`
	Splits   []lake.SkaterSeason `json:"skaterSplits,omitempty"`
	GSplits  []lake.GoalieSeason `json:"goalieSplits,omitempty"`
}

func (s *Server) playerCard(r *http.Request) (*playerCard, *lake.SeasonRollup, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, nil, err
	}
	season, err := s.querySeason(r)
	if err != nil {
		return nil, nil, err
	}
	ro, err := s.rollup(r.Context(), season)
	if err != nil {
		return nil, nil, err
	}
	card := &playerCard{PlayerID: id, Season: season, Teams: []string{}}
	seen := map[string]bool{}
	for _, sk := range ro.Skaters {
		if sk.PlayerID == id {
			card.Splits = append(card.Splits, sk)
			card.Name, card.Position, card.Number = sk.Name, sk.Position, sk.Number
			if !seen[sk.Team] {
				card.Teams = append(card.Teams, sk.Team)
				seen[sk.Team] = true
			}
		}
	}
	for _, g := range ro.Goalies {
		if g.PlayerID == id {
			card.GSplits = append(card.GSplits, g)
			card.Name, card.Position, card.Number = g.Name, "G", g.Number
			if !seen[g.Team] {
				card.Teams = append(card.Teams, g.Team)
				seen[g.Team] = true
			}
		}
	}
	if len(card.Splits) == 0 && len(card.GSplits) == 0 {
		return nil, nil, notFound("player %d has no games in season %d", id, season)
	}
	if len(card.Splits) > 0 {
		sub := &lake.SeasonRollup{Skaters: card.Splits}
		if rows := skaterRows(sub, "", "", 0); len(rows) == 1 {
			card.Skater = &rows[0]
		}
	}
	if len(card.GSplits) > 0 {
		sub := &lake.SeasonRollup{Goalies: card.GSplits}
		if rows := goalieRows(sub, "", 0); len(rows) == 1 {
			card.Goalie = &rows[0]
		}
	}
	return card, ro, nil
}

func (s *Server) handlePlayer(r *http.Request) (any, time.Duration, error) {
	card, _, err := s.playerCard(r)
	if err != nil {
		return nil, 0, err
	}
	return card, ttlIndex, nil
}

// gameLogEntry is one game in a player's log.
type gameLogEntry struct {
	Game   lake.GameRow     `json:"game"`
	Skater *lake.SkaterLine `json:"skater,omitempty"`
	Goalie *lake.GoalieLine `json:"goalie,omitempty"`
}

// handlePlayerGames reads the summaries of every game the player's teams
// played and pulls the player's line from each, concurrently.
func (s *Server) handlePlayerGames(r *http.Request) (any, time.Duration, error) {
	card, ro, err := s.playerCard(r)
	if err != nil {
		return nil, 0, err
	}
	limit, offset, err := paging(r)
	if err != nil {
		return nil, 0, err
	}
	ctx := r.Context()
	var games []lake.GameRow
	seen := map[int64]bool{}
	for _, team := range card.Teams {
		for _, g := range gamesForTeam(ro, team) {
			if g.Final() && !seen[g.ID] {
				games = append(games, g)
				seen[g.ID] = true
			}
		}
	}
	entries := make([]*gameLogEntry, len(games))
	var firstErr error
	var mu sync.Mutex
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for i, g := range games {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, g lake.GameRow) {
			defer wg.Done()
			defer func() { <-sem }()
			sum, err := s.gameSummary(ctx, g.ID)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("game %d: %w", g.ID, err)
				}
				mu.Unlock()
				return
			}
			for _, line := range sum.Skaters {
				if line.PlayerID == card.PlayerID {
					l := line
					entries[i] = &gameLogEntry{Game: g, Skater: &l}
					return
				}
			}
			for _, line := range sum.Goalies {
				if line.PlayerID == card.PlayerID {
					l := line
					entries[i] = &gameLogEntry{Game: g, Goalie: &l}
					return
				}
			}
		}(i, g)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, 0, firstErr
	}
	items := make([]gameLogEntry, 0, len(entries))
	for _, e := range entries {
		if e != nil {
			items = append(items, *e)
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Game.Date < items[j].Game.Date })
	return slicePage(card.Season, items, limit, offset), ttlIndex, nil
}
