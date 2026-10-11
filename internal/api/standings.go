package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"hockeytrack/internal/store"
)

// standingsRow is the NHL's standings line with the columns a table shows.
// The raw response has about ninety fields; these are the ones worth
// typing, and the archive keeps the rest.
type standingsRow struct {
	Abbrev           string  `json:"abbrev"`
	Name             string  `json:"name"`
	Conference       string  `json:"conference"`
	Division         string  `json:"division"`
	DivisionRank     int     `json:"divisionRank"`
	ConferenceRank   int     `json:"conferenceRank"`
	LeagueRank       int     `json:"leagueRank"`
	WildcardRank     int     `json:"wildcardRank,omitempty"`
	Clinch           string  `json:"clinch,omitempty"`
	GP               int     `json:"gp"`
	Wins             int     `json:"wins"`
	Losses           int     `json:"losses"`
	OTLosses         int     `json:"otLosses"`
	Ties             int     `json:"ties,omitempty"`
	Points           int     `json:"points"`
	PointPct         float64 `json:"pointPct"`
	RegulationWins   int     `json:"regulationWins"`
	ROW              int     `json:"row"`
	GoalsFor         int     `json:"goalsFor"`
	GoalsAgainst     int     `json:"goalsAgainst"`
	GoalDifferential int     `json:"goalDifferential"`
	HomeRecord       string  `json:"homeRecord"`
	RoadRecord       string  `json:"roadRecord"`
	L10Record        string  `json:"l10Record"`
	Streak           string  `json:"streak,omitempty"`
	ShootoutWins     int     `json:"shootoutWins"`
	ShootoutLosses   int     `json:"shootoutLosses"`
}

type localized struct {
	Default string `json:"default"`
}

// rawStandingsRow is the subset of the NHL's row this endpoint reads.
type rawStandingsRow struct {
	TeamAbbrev         localized `json:"teamAbbrev"`
	TeamName           localized `json:"teamName"`
	ConferenceName     string    `json:"conferenceName"`
	ConferenceAbbrev   string    `json:"conferenceAbbrev"`
	DivisionName       string    `json:"divisionName"`
	DivisionAbbrev     string    `json:"divisionAbbrev"`
	DivisionSequence   int       `json:"divisionSequence"`
	ConferenceSequence int       `json:"conferenceSequence"`
	LeagueSequence     int       `json:"leagueSequence"`
	WildcardSequence   int       `json:"wildcardSequence"`
	ClinchIndicator    string    `json:"clinchIndicator"`
	GamesPlayed        int       `json:"gamesPlayed"`
	Wins               int       `json:"wins"`
	Losses             int       `json:"losses"`
	OtLosses           int       `json:"otLosses"`
	Ties               int       `json:"ties"`
	Points             int       `json:"points"`
	PointPctg          float64   `json:"pointPctg"`
	RegulationWins     int       `json:"regulationWins"`
	RegulationPlusOt   int       `json:"regulationPlusOtWins"`
	GoalFor            int       `json:"goalFor"`
	GoalAgainst        int       `json:"goalAgainst"`
	GoalDifferential   int       `json:"goalDifferential"`
	HomeWins           int       `json:"homeWins"`
	HomeLosses         int       `json:"homeLosses"`
	HomeOtLosses       int       `json:"homeOtLosses"`
	RoadWins           int       `json:"roadWins"`
	RoadLosses         int       `json:"roadLosses"`
	RoadOtLosses       int       `json:"roadOtLosses"`
	L10Wins            int       `json:"l10Wins"`
	L10Losses          int       `json:"l10Losses"`
	L10OtLosses        int       `json:"l10OtLosses"`
	StreakCode         string    `json:"streakCode"`
	StreakCount        int       `json:"streakCount"`
	ShootoutWins       int       `json:"shootoutWins"`
	ShootoutLosses     int       `json:"shootoutLosses"`
	Date               string    `json:"date"`
	SeasonID           int64     `json:"seasonId"`
}

type standingsTable struct {
	Date     string         `json:"date"`     // the standings' own date
	Season   int64          `json:"season"`   // the standings' own season
	Archived string         `json:"archived"` // the day the snapshot was taken
	rows     []standingsRow `json:"-"`
}

func record(w, l, otl int) string { return fmt.Sprintf("%d-%d-%d", w, l, otl) }

// standingsDates lists the days a standings snapshot was archived, newest
// first.
func (s *Server) standingsDates(ctx context.Context) ([]string, error) {
	v, err := s.memo.get("standings-dates", ttlStandings, func() (any, error) {
		keys, err := s.deps.Raw.List(ctx, "raw/standings/")
		if err != nil {
			return nil, err
		}
		var dates []string
		for _, k := range keys {
			d := strings.TrimSuffix(strings.TrimPrefix(k, "raw/standings/"), ".json")
			if len(d) == 10 {
				dates = append(dates, d)
			}
		}
		sort.Sort(sort.Reverse(sort.StringSlice(dates)))
		return dates, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]string), nil
}

// standingsOn loads the snapshot archived on a date.
func (s *Server) standingsOn(ctx context.Context, date string) (*standingsTable, error) {
	v, err := s.memo.get("standings:"+date, ttlStatic, func() (any, error) {
		body, err := s.deps.Raw.Get(ctx, store.StandingsKey(date))
		if err != nil {
			if missing(err) {
				return nil, fmt.Errorf("no standings archived on %s: %w", date, errNotFound)
			}
			return nil, err
		}
		var raw struct {
			Standings []rawStandingsRow `json:"standings"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, badGateway("standings on %s are unreadable", date)
		}
		t := &standingsTable{Archived: date}
		for _, r := range raw.Standings {
			if t.Season == 0 {
				t.Season, t.Date = r.SeasonID, r.Date
			}
			row := standingsRow{
				Abbrev: r.TeamAbbrev.Default, Name: r.TeamName.Default,
				Conference: r.ConferenceName, Division: r.DivisionName,
				DivisionRank: r.DivisionSequence, ConferenceRank: r.ConferenceSequence, LeagueRank: r.LeagueSequence,
				WildcardRank: r.WildcardSequence, Clinch: r.ClinchIndicator,
				GP: r.GamesPlayed, Wins: r.Wins, Losses: r.Losses, OTLosses: r.OtLosses, Ties: r.Ties,
				Points: r.Points, PointPct: r.PointPctg, RegulationWins: r.RegulationWins, ROW: r.RegulationPlusOt,
				GoalsFor: r.GoalFor, GoalsAgainst: r.GoalAgainst, GoalDifferential: r.GoalDifferential,
				HomeRecord:   record(r.HomeWins, r.HomeLosses, r.HomeOtLosses),
				RoadRecord:   record(r.RoadWins, r.RoadLosses, r.RoadOtLosses),
				L10Record:    record(r.L10Wins, r.L10Losses, r.L10OtLosses),
				ShootoutWins: r.ShootoutWins, ShootoutLosses: r.ShootoutLosses,
			}
			if r.StreakCount > 0 && r.StreakCode != "" {
				row.Streak = fmt.Sprintf("%s%d", r.StreakCode, r.StreakCount)
			}
			t.rows = append(t.rows, row)
		}
		sort.SliceStable(t.rows, func(i, j int) bool { return t.rows[i].LeagueRank < t.rows[j].LeagueRank })
		return t, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*standingsTable), nil
}

// latestStandings is the newest archived snapshot.
func (s *Server) latestStandings(ctx context.Context) (*standingsTable, error) {
	dates, err := s.standingsDates(ctx)
	if err != nil {
		return nil, err
	}
	if len(dates) == 0 {
		return nil, fmt.Errorf("no standings archived yet: %w", errNotFound)
	}
	return s.standingsOn(ctx, dates[0])
}

func (s *Server) handleStandings(r *http.Request) (any, time.Duration, error) {
	ctx := r.Context()
	date, hasDate, err := queryDate(r, "date", s.deps.Now())
	if err != nil {
		return nil, 0, err
	}
	conference := strings.ToLower(r.URL.Query().Get("conference"))
	division := strings.ToLower(r.URL.Query().Get("division"))
	var table *standingsTable
	ttl := ttlStandings
	if hasDate {
		// The snapshot taken on the date, or the newest one before it.
		dates, err := s.standingsDates(ctx)
		if err != nil {
			return nil, 0, err
		}
		pick := ""
		for _, d := range dates {
			if d <= date {
				pick = d
				break
			}
		}
		if pick == "" {
			return nil, 0, notFound("no standings archived on or before %s", date)
		}
		if table, err = s.standingsOn(ctx, pick); err != nil {
			return nil, 0, err
		}
		if pick < s.deps.Now().UTC().Format("2006-01-02") {
			ttl = ttlStatic
		}
	} else if table, err = s.latestStandings(ctx); err != nil {
		return nil, 0, err
	}
	rows := make([]standingsRow, 0, len(table.rows))
	for _, row := range table.rows {
		if conference != "" && strings.ToLower(row.Conference) != conference {
			continue
		}
		if division != "" && strings.ToLower(row.Division) != division {
			continue
		}
		rows = append(rows, row)
	}
	return map[string]any{
		"date": table.Date, "season": table.Season, "archived": table.Archived,
		"total": len(rows), "items": rows,
	}, ttl, nil
}
