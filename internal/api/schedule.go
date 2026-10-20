package api

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"hockeytrack/internal/lake"
	"hockeytrack/internal/schedsync"
)

// scheduleGame is a schedule row with the result attached once known.
type scheduleGame struct {
	ID       int64       `json:"id"`
	Season   int64       `json:"season"`
	Type     int         `json:"type"`
	Date     string      `json:"date"`
	Start    time.Time   `json:"start"`
	Away     string      `json:"away"`
	Home     string      `json:"home"`
	AwayName string      `json:"awayName,omitempty"`
	HomeName string      `json:"homeName,omitempty"`
	Venue    string      `json:"venue,omitempty"`
	Result   *gameResult `json:"result,omitempty"`
}

type gameResult struct {
	AwayScore      int    `json:"awayScore"`
	HomeScore      int    `json:"homeScore"`
	State          string `json:"state"`
	LastPeriodType string `json:"lastPeriodType,omitempty"`
	Live           bool   `json:"live,omitempty"`
}

type scheduleFilter struct {
	date, from, to string
	team           string
	venue          string // lower-cased substring
	season         int64
	gameType       int
	hasType        bool
}

func (s *Server) scheduleFilter(r *http.Request) (scheduleFilter, error) {
	var f scheduleFilter
	var err error
	now := s.deps.Now()
	if f.date, _, err = queryDate(r, "date", now); err != nil {
		return f, err
	}
	if f.from, _, err = queryDate(r, "from", now); err != nil {
		return f, err
	}
	if f.to, _, err = queryDate(r, "to", now); err != nil {
		return f, err
	}
	if f.team, err = queryTeam(r, "team"); err != nil {
		return f, err
	}
	var ok bool
	if f.season, ok, err = queryInt64(r, "season"); err != nil {
		return f, err
	} else if !ok {
		f.season = 0
	}
	if f.gameType, f.hasType, err = queryInt(r, "type"); err != nil {
		return f, err
	}
	f.venue = strings.ToLower(strings.TrimSpace(r.URL.Query().Get("venue")))
	if len(f.venue) > 64 {
		return f, badRequest("venue must be at most 64 characters")
	}
	return f, nil
}

func (f scheduleFilter) keep(g schedsync.SiteGame) bool {
	if f.date != "" && g.Date != f.date {
		return false
	}
	if f.from != "" && g.Date < f.from {
		return false
	}
	if f.to != "" && g.Date > f.to {
		return false
	}
	if f.team != "" && g.Away != f.team && g.Home != f.team {
		return false
	}
	if f.season != 0 && lake.SeasonOf(g.ID) != f.season {
		return false
	}
	if f.hasType && g.Type != f.gameType {
		return false
	}
	if f.venue != "" && !strings.Contains(strings.ToLower(g.Venue), f.venue) {
		return false
	}
	return true
}

// scheduleRows filters the schedule document and attaches results from
// the rollups of whichever seasons the rows span.
func (s *Server) scheduleRows(r *http.Request, f scheduleFilter) ([]scheduleGame, error) {
	ctx := r.Context()
	sched, err := s.schedule(ctx)
	if err != nil {
		return nil, err
	}
	rollups := map[int64]*lake.SeasonRollup{}
	var rows []scheduleGame
	for _, g := range sched.Games {
		if !f.keep(g) {
			continue
		}
		season := lake.SeasonOf(g.ID)
		row := scheduleGame{
			ID: g.ID, Season: season, Type: g.Type, Date: g.Date, Start: g.Start,
			Away: g.Away, Home: g.Home, AwayName: sched.Teams[g.Away], HomeName: sched.Teams[g.Home], Venue: g.Venue,
		}
		ro, seen := rollups[season]
		if !seen {
			ro, err = s.rollup(ctx, season)
			if err != nil && !errors.Is(err, errNotFound) {
				return nil, err
			}
			rollups[season] = ro
		}
		if ro != nil {
			if res := resultFor(ro, g.ID); res != nil {
				row.Result = res
			}
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].Start.Equal(rows[j].Start) {
			return rows[i].Start.Before(rows[j].Start)
		}
		return rows[i].ID < rows[j].ID
	})
	return rows, nil
}

func resultFor(ro *lake.SeasonRollup, id int64) *gameResult {
	for _, g := range ro.Games {
		if g.ID == id {
			return &gameResult{AwayScore: g.Away.Score, HomeScore: g.Home.Score, State: g.State, LastPeriodType: g.LastPeriodType, Live: g.Live}
		}
	}
	return nil
}

func (s *Server) handleSchedule(r *http.Request) (any, time.Duration, error) {
	f, err := s.scheduleFilter(r)
	if err != nil {
		return nil, 0, err
	}
	limit, offset, err := paging(r)
	if err != nil {
		return nil, 0, err
	}
	rows, err := s.scheduleRows(r, f)
	if err != nil {
		return nil, 0, err
	}
	return slicePage(f.season, rows, limit, offset), ttlIndex, nil
}

// teamRef is a team as the teams list shows it.
type teamRef struct {
	Abbrev string `json:"abbrev"`
	Name   string `json:"name,omitempty"`
}

// teamNames merges the schedule document's teams with the rollup's, so
// the list works with either alone.
func (s *Server) teamNames(r *http.Request) (map[string]string, error) {
	ctx := r.Context()
	names := map[string]string{}
	sched, err := s.schedule(ctx)
	if err != nil && !errors.Is(err, errNotFound) {
		return nil, err
	}
	if sched != nil {
		for abbrev, name := range sched.Teams {
			names[abbrev] = name
		}
	}
	if season, err := s.currentSeason(ctx); err == nil {
		if ro, err := s.rollup(ctx, season); err == nil {
			for _, t := range ro.Teams {
				if _, ok := names[t.Abbrev]; !ok {
					names[t.Abbrev] = t.Name
				}
			}
		} else if !errors.Is(err, errNotFound) {
			return nil, err
		}
	}
	return names, nil
}

func (s *Server) handleTeams(r *http.Request) (any, time.Duration, error) {
	names, err := s.teamNames(r)
	if err != nil {
		return nil, 0, err
	}
	items := make([]teamRef, 0, len(names))
	for abbrev, name := range names {
		items = append(items, teamRef{Abbrev: abbrev, Name: name})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Abbrev < items[j].Abbrev })
	return page{Total: len(items), Limit: len(items), Items: items}, ttlIndex, nil
}

type teamCard struct {
	Abbrev    string           `json:"abbrev"`
	Name      string           `json:"name,omitempty"`
	Season    int64            `json:"season"`
	Record    *lake.TeamSeason `json:"record,omitempty"`
	Standings *standingsRow    `json:"standings,omitempty"`
	LastGame  *lake.GameRow    `json:"lastGame,omitempty"`
	NextGame  *scheduleGame    `json:"nextGame,omitempty"`
}

func (s *Server) handleTeam(r *http.Request) (any, time.Duration, error) {
	team, err := cleanTeam(r.PathValue("team"))
	if err != nil {
		return nil, 0, err
	}
	season, err := s.querySeason(r)
	if err != nil {
		return nil, 0, err
	}
	ctx := r.Context()
	names, err := s.teamNames(r)
	if err != nil {
		return nil, 0, err
	}
	card := teamCard{Abbrev: team, Name: names[team], Season: season}
	known := card.Name != ""
	if ro, err := s.rollup(ctx, season); err == nil {
		for i := range ro.Teams {
			if ro.Teams[i].Abbrev == team {
				t := ro.Teams[i]
				card.Record = &t
				known = true
				if card.Name == "" {
					card.Name = t.Name
				}
			}
		}
		games := gamesForTeam(ro, team)
		for i := len(games) - 1; i >= 0; i-- {
			if games[i].Final() {
				g := games[i]
				card.LastGame = &g
				break
			}
		}
	} else if !errors.Is(err, errNotFound) {
		return nil, 0, err
	}
	if !known {
		return nil, 0, notFound("team %s is not in the schedule or the index", team)
	}
	if table, err := s.latestStandings(ctx); err == nil {
		for i := range table.rows {
			if table.rows[i].Abbrev == team {
				row := table.rows[i]
				card.Standings = &row
			}
		}
	} else if !errors.Is(err, errNotFound) {
		return nil, 0, err
	}
	rows, err := s.scheduleRows(r, scheduleFilter{team: team})
	if err != nil && !errors.Is(err, errNotFound) {
		return nil, 0, err
	}
	now := s.deps.Now()
	for i := range rows {
		if rows[i].Result == nil && rows[i].Start.After(now) {
			card.NextGame = &rows[i]
			break
		}
	}
	return card, ttlIndex, nil
}

func (s *Server) handleTeamGames(r *http.Request) (any, time.Duration, error) {
	team, err := cleanTeam(r.PathValue("team"))
	if err != nil {
		return nil, 0, err
	}
	season, err := s.querySeason(r)
	if err != nil {
		return nil, 0, err
	}
	limit, offset, err := paging(r)
	if err != nil {
		return nil, 0, err
	}
	gameType, hasType, err := queryInt(r, "type")
	if err != nil {
		return nil, 0, err
	}
	f := scheduleFilter{team: team, season: season, gameType: gameType, hasType: hasType}
	rows, err := s.scheduleRows(r, f)
	if err != nil && !errors.Is(err, errNotFound) {
		return nil, 0, err
	}
	// The schedule document covers the published season only; the index
	// holds every archived game, so past seasons come from there.
	seen := map[int64]bool{}
	for _, row := range rows {
		seen[row.ID] = true
	}
	ro, err := s.rollup(r.Context(), season)
	if err != nil && !errors.Is(err, errNotFound) {
		return nil, 0, err
	}
	if ro != nil {
		for _, g := range gamesForTeam(ro, team) {
			if seen[g.ID] || (hasType && g.Type != gameType) {
				continue
			}
			rows = append(rows, scheduleGame{
				ID: g.ID, Season: g.Season, Type: g.Type, Date: g.Date, Start: g.Start,
				Away: g.Away.Abbrev, Home: g.Home.Abbrev, AwayName: g.Away.Name, HomeName: g.Home.Name, Venue: g.Venue,
				Result: &gameResult{AwayScore: g.Away.Score, HomeScore: g.Home.Score, State: g.State, LastPeriodType: g.LastPeriodType, Live: g.Live},
			})
		}
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].Date != rows[j].Date {
				return rows[i].Date < rows[j].Date
			}
			return rows[i].ID < rows[j].ID
		})
	}
	if len(rows) == 0 {
		return nil, 0, notFound("team %s has no games in season %d", team, season)
	}
	return slicePage(season, rows, limit, offset), ttlIndex, nil
}

type rosterEntry struct {
	PlayerID int64  `json:"playerId"`
	Name     string `json:"name"`
	Number   int    `json:"number,omitempty"`
	Position string `json:"position,omitempty"`
	GP       int    `json:"gp"`
	Goals    int    `json:"goals,omitempty"`
	Assists  int    `json:"assists,omitempty"`
	Points   int    `json:"points,omitempty"`
	Wins     int    `json:"wins,omitempty"`
	Losses   int    `json:"losses,omitempty"`
}

func (s *Server) handleTeamRoster(r *http.Request) (any, time.Duration, error) {
	team, err := cleanTeam(r.PathValue("team"))
	if err != nil {
		return nil, 0, err
	}
	season, err := s.querySeason(r)
	if err != nil {
		return nil, 0, err
	}
	ro, err := s.rollup(r.Context(), season)
	if err != nil {
		return nil, 0, err
	}
	var items []rosterEntry
	for _, sk := range ro.Skaters {
		if sk.Team == team {
			items = append(items, rosterEntry{PlayerID: sk.PlayerID, Name: sk.Name, Number: sk.Number, Position: sk.Position,
				GP: sk.GP, Goals: sk.Goals, Assists: sk.Assists, Points: sk.Points})
		}
	}
	for _, g := range ro.Goalies {
		if g.Team == team {
			items = append(items, rosterEntry{PlayerID: g.PlayerID, Name: g.Name, Number: g.Number, Position: "G",
				GP: g.GP, Wins: g.Wins, Losses: g.Losses})
		}
	}
	if len(items) == 0 {
		return nil, 0, notFound("team %s has no players in season %d", team, season)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Position == "G" != (items[j].Position == "G") {
			return items[i].Position != "G"
		}
		if items[i].Number != items[j].Number {
			return items[i].Number < items[j].Number
		}
		return items[i].PlayerID < items[j].PlayerID
	})
	return page{Season: season, Total: len(items), Limit: len(items), Items: items}, ttlIndex, nil
}
