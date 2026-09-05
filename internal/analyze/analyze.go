// Package analyze flattens the final/ objects of archived games into CSV.
//
// It reads the same objects the poller and backfill leave behind
// (raw/{season}/{date}/{gameId}/final/{pbp,boxscore,landing,shifts}.json),
// either from the raw bucket or from a local mirror of it, and writes three
// tables: one row per game, one per play, one per shift. Only pbp.json is
// required; the other feeds fill in what they can when present, so games
// from any era (no shift charts before 2010-11, only goals and penalties
// before 2006-07) come out with blank cells rather than errors.
package analyze

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
)

// Source is the read side of an archive: anything that can enumerate keys
// under a prefix and fetch one of them. store.S3Archive and DirSource both
// satisfy it.
type Source interface {
	List(ctx context.Context, prefix string) ([]string, error)
	Get(ctx context.Context, key string) ([]byte, error)
}

// Options selects which games to flatten. Prefix narrows the List call
// (cheap for S3); Season and Date filter on the path segments of each game
// prefix, so they work the same whether the source uses raw/ keys or a bare
// {season}/{date}/{gameId} tree.
type Options struct {
	Prefix string
	Season int64  // 0 = every season
	Date   string // "" = every date; YYYY-MM-DD otherwise
}

// Writers receives the three tables. Any nil writer skips that table.
type Writers struct {
	Games, Plays, Shifts io.Writer
}

// Stats summarises a run.
type Stats struct {
	Games  int // games written to games.csv
	Plays  int
	Shifts int
	Failed int // games skipped because pbp.json was missing or unparsable
}

func (s Stats) String() string {
	return fmt.Sprintf("games=%d plays=%d shifts=%d failed=%d", s.Games, s.Plays, s.Shifts, s.Failed)
}

// ErrGamesFailed is returned when the run finished but some games could not
// be read; the tables hold every game that could.
var ErrGamesFailed = errors.New("some games could not be read")

var gamesHeader = []string{
	"gameId", "season", "gameType", "gameDate", "startTimeUTC", "gameState", "venue",
	"awayTeamId", "awayAbbrev", "homeTeamId", "homeAbbrev",
	"awayScore", "homeScore", "periods", "lastPeriodType",
	"awaySOG", "homeSOG", "plays", "shifts",
}

var playsHeader = []string{
	"gameId", "seq", "eventId", "period", "periodType", "timeInPeriod", "timeRemaining",
	"typeCode", "type", "situationCode", "teamId", "team",
	"x", "y", "zone", "shotType", "reason",
	"penaltyType", "penaltyDesc", "penaltyMinutes", "awayScore", "homeScore",
	"scoringPlayerId", "assist1PlayerId", "assist2PlayerId",
	"shootingPlayerId", "goalieInNetId", "blockingPlayerId",
	"winningPlayerId", "losingPlayerId",
	"hittingPlayerId", "hitteePlayerId",
	"committedByPlayerId", "drawnByPlayerId", "playerId",
}

var shiftsHeader = []string{
	"gameId", "shiftId", "playerId", "firstName", "lastName", "teamId", "team",
	"period", "shiftNumber", "startTime", "endTime", "duration", "seconds", "eventNumber",
}

// game is the subset of pbp.json (and, identically shaped, landing.json and
// boxscore.json) the tables need. Pointers distinguish "absent in this era"
// from zero.
type game struct {
	ID           int64  `json:"id"`
	Season       int64  `json:"season"`
	GameType     int    `json:"gameType"`
	GameDate     string `json:"gameDate"`
	StartTimeUTC string `json:"startTimeUTC"`
	GameState    string `json:"gameState"`
	Venue        struct {
		Default string `json:"default"`
	} `json:"venue"`
	PeriodDescriptor struct {
		Number int `json:"number"`
	} `json:"periodDescriptor"`
	GameOutcome struct {
		LastPeriodType string `json:"lastPeriodType"`
	} `json:"gameOutcome"`
	AwayTeam team   `json:"awayTeam"`
	HomeTeam team   `json:"homeTeam"`
	Plays    []play `json:"plays"`
}

type team struct {
	ID     int64  `json:"id"`
	Abbrev string `json:"abbrev"`
	Score  *int   `json:"score"`
	SOG    *int   `json:"sog"`
}

type play struct {
	EventID          int64  `json:"eventId"`
	SortOrder        int64  `json:"sortOrder"`
	TypeCode         int    `json:"typeCode"`
	TypeDescKey      string `json:"typeDescKey"`
	TimeInPeriod     string `json:"timeInPeriod"`
	TimeRemaining    string `json:"timeRemaining"`
	SituationCode    string `json:"situationCode"`
	PeriodDescriptor struct {
		Number     int    `json:"number"`
		PeriodType string `json:"periodType"`
	} `json:"periodDescriptor"`
	Details details `json:"details"`
}

// details covers every player, position and score field the NHL has put in
// a play's details across eras. Coordinates stay json.Number so they are
// written back exactly as received.
type details struct {
	EventOwnerTeamID    *int64      `json:"eventOwnerTeamId"`
	XCoord              json.Number `json:"xCoord"`
	YCoord              json.Number `json:"yCoord"`
	ZoneCode            string      `json:"zoneCode"`
	ShotType            string      `json:"shotType"`
	Reason              string      `json:"reason"`
	PenaltyType         string      `json:"typeCode"`
	PenaltyDesc         string      `json:"descKey"`
	PenaltyMinutes      *int        `json:"duration"`
	AwayScore           *int        `json:"awayScore"`
	HomeScore           *int        `json:"homeScore"`
	ScoringPlayerID     *int64      `json:"scoringPlayerId"`
	Assist1PlayerID     *int64      `json:"assist1PlayerId"`
	Assist2PlayerID     *int64      `json:"assist2PlayerId"`
	ShootingPlayerID    *int64      `json:"shootingPlayerId"`
	GoalieInNetID       *int64      `json:"goalieInNetId"`
	BlockingPlayerID    *int64      `json:"blockingPlayerId"`
	WinningPlayerID     *int64      `json:"winningPlayerId"`
	LosingPlayerID      *int64      `json:"losingPlayerId"`
	HittingPlayerID     *int64      `json:"hittingPlayerId"`
	HitteePlayerID      *int64      `json:"hitteePlayerId"`
	CommittedByPlayerID *int64      `json:"committedByPlayerId"`
	DrawnByPlayerID     *int64      `json:"drawnByPlayerId"`
	PlayerID            *int64      `json:"playerId"`
}

// shiftChart is api.nhle.com's shiftcharts response.
type shiftChart struct {
	Data []shift `json:"data"`
}

// shiftTypeCode marks a real shift; the chart also carries goal markers
// (typeCode 505, duration null) that duplicate pbp goals and are dropped.
const shiftTypeCode = 517

type shift struct {
	ID          int64  `json:"id"`
	PlayerID    int64  `json:"playerId"`
	FirstName   string `json:"firstName"`
	LastName    string `json:"lastName"`
	TeamID      int64  `json:"teamId"`
	TeamAbbrev  string `json:"teamAbbrev"`
	Period      int    `json:"period"`
	ShiftNumber int    `json:"shiftNumber"`
	StartTime   string `json:"startTime"`
	EndTime     string `json:"endTime"`
	Duration    string `json:"duration"`
	EventNumber int64  `json:"eventNumber"`
	TypeCode    int    `json:"typeCode"`
}

// Run flattens every game under opts into the writers. It returns
// ErrGamesFailed when some games were skipped (their count is in Stats),
// and any other error only when listing or writing itself failed.
func Run(ctx context.Context, src Source, opts Options, w Writers) (Stats, error) {
	var st Stats
	keys, err := src.List(ctx, opts.Prefix)
	if err != nil {
		return st, fmt.Errorf("list %q: %w", opts.Prefix, err)
	}
	games, have := findGames(keys, opts)

	out, err := newTables(w)
	if err != nil {
		return st, err
	}
	for _, prefix := range games {
		if ctx.Err() != nil {
			return st, ctx.Err()
		}
		g, err := loadGame(ctx, src, prefix)
		if err != nil {
			slog.Warn("skipping game", "prefix", prefix, "err", err)
			st.Failed++
			continue
		}
		var chart shiftChart
		if have[prefix+"final/shifts.json"] {
			b, err := src.Get(ctx, prefix+"final/shifts.json")
			if err != nil {
				return st, fmt.Errorf("get %sfinal/shifts.json: %w", prefix, err)
			}
			if err := json.Unmarshal(b, &chart); err != nil {
				// A bad chart costs the shift rows, not the game.
				slog.Warn("unreadable shift chart", "prefix", prefix, "err", err)
			}
		}
		n, err := out.writeGame(g, chart)
		if err != nil {
			return st, err
		}
		st.Games++
		st.Plays += len(g.Plays)
		st.Shifts += n
	}
	if err := out.flush(); err != nil {
		return st, err
	}
	if st.Failed > 0 {
		return st, ErrGamesFailed
	}
	return st, nil
}

// findGames returns the sorted game prefixes (…/{season}/{date}/{gameId}/)
// that hold a final/pbp.json and pass the season/date filters, plus the set
// of every listed key so optional feeds can be checked without a request.
func findGames(keys []string, opts Options) ([]string, map[string]bool) {
	have := make(map[string]bool, len(keys))
	var games []string
	for _, k := range keys {
		have[k] = true
		prefix, ok := strings.CutSuffix(k, "final/pbp.json")
		if !ok {
			continue
		}
		parts := strings.Split(strings.TrimSuffix(prefix, "/"), "/")
		if len(parts) < 3 {
			continue
		}
		season, date := parts[len(parts)-3], parts[len(parts)-2]
		if opts.Season != 0 && season != strconv.FormatInt(opts.Season, 10) {
			continue
		}
		if opts.Date != "" && date != opts.Date {
			continue
		}
		games = append(games, prefix)
	}
	sort.Strings(games)
	return games, have
}

func loadGame(ctx context.Context, src Source, prefix string) (*game, error) {
	b, err := src.Get(ctx, prefix+"final/pbp.json")
	if err != nil {
		return nil, err
	}
	var g game
	if err := json.Unmarshal(b, &g); err != nil {
		return nil, fmt.Errorf("parse pbp.json: %w", err)
	}
	if g.ID == 0 {
		return nil, errors.New("pbp.json has no game id")
	}
	return &g, nil
}

type tables struct {
	games, plays, shifts *csv.Writer
}

func newTables(w Writers) (*tables, error) {
	t := &tables{}
	open := func(dst io.Writer, header []string) (*csv.Writer, error) {
		if dst == nil {
			return nil, nil
		}
		cw := csv.NewWriter(dst)
		return cw, cw.Write(header)
	}
	var err error
	if t.games, err = open(w.Games, gamesHeader); err != nil {
		return nil, err
	}
	if t.plays, err = open(w.Plays, playsHeader); err != nil {
		return nil, err
	}
	if t.shifts, err = open(w.Shifts, shiftsHeader); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *tables) flush() error {
	for _, cw := range []*csv.Writer{t.games, t.plays, t.shifts} {
		if cw == nil {
			continue
		}
		cw.Flush()
		if err := cw.Error(); err != nil {
			return err
		}
	}
	return nil
}

// writeGame appends the game's rows to every open table and returns the
// number of shift rows written.
func (t *tables) writeGame(g *game, chart shiftChart) (int, error) {
	id := strconv.FormatInt(g.ID, 10)
	abbrev := map[int64]string{g.AwayTeam.ID: g.AwayTeam.Abbrev, g.HomeTeam.ID: g.HomeTeam.Abbrev}

	shifts := 0
	if t.shifts != nil {
		for _, s := range chart.Data {
			if s.TypeCode != shiftTypeCode {
				continue
			}
			row := []string{
				id, strconv.FormatInt(s.ID, 10), strconv.FormatInt(s.PlayerID, 10), s.FirstName, s.LastName,
				strconv.FormatInt(s.TeamID, 10), s.TeamAbbrev,
				strconv.Itoa(s.Period), strconv.Itoa(s.ShiftNumber), s.StartTime, s.EndTime, s.Duration,
				clockSeconds(s.Duration), strconv.FormatInt(s.EventNumber, 10),
			}
			if err := t.shifts.Write(row); err != nil {
				return 0, err
			}
			shifts++
		}
	} else {
		for _, s := range chart.Data {
			if s.TypeCode == shiftTypeCode {
				shifts++
			}
		}
	}

	if t.plays != nil {
		for _, p := range g.Plays {
			d := p.Details
			var teamID, team string
			if d.EventOwnerTeamID != nil {
				teamID = strconv.FormatInt(*d.EventOwnerTeamID, 10)
				team = abbrev[*d.EventOwnerTeamID]
			}
			row := []string{
				id, strconv.FormatInt(p.SortOrder, 10), strconv.FormatInt(p.EventID, 10),
				strconv.Itoa(p.PeriodDescriptor.Number), p.PeriodDescriptor.PeriodType, p.TimeInPeriod, p.TimeRemaining,
				strconv.Itoa(p.TypeCode), p.TypeDescKey, p.SituationCode, teamID, team,
				string(d.XCoord), string(d.YCoord), d.ZoneCode, d.ShotType, d.Reason,
				d.PenaltyType, d.PenaltyDesc, optInt(d.PenaltyMinutes), optInt(d.AwayScore), optInt(d.HomeScore),
				optID(d.ScoringPlayerID), optID(d.Assist1PlayerID), optID(d.Assist2PlayerID),
				optID(d.ShootingPlayerID), optID(d.GoalieInNetID), optID(d.BlockingPlayerID),
				optID(d.WinningPlayerID), optID(d.LosingPlayerID),
				optID(d.HittingPlayerID), optID(d.HitteePlayerID),
				optID(d.CommittedByPlayerID), optID(d.DrawnByPlayerID), optID(d.PlayerID),
			}
			if err := t.plays.Write(row); err != nil {
				return 0, err
			}
		}
	}

	if t.games != nil {
		periods := g.PeriodDescriptor.Number
		for _, p := range g.Plays {
			if p.PeriodDescriptor.Number > periods {
				periods = p.PeriodDescriptor.Number
			}
		}
		row := []string{
			id, strconv.FormatInt(g.Season, 10), strconv.Itoa(g.GameType), g.GameDate, g.StartTimeUTC, g.GameState, g.Venue.Default,
			strconv.FormatInt(g.AwayTeam.ID, 10), g.AwayTeam.Abbrev, strconv.FormatInt(g.HomeTeam.ID, 10), g.HomeTeam.Abbrev,
			optInt(g.AwayTeam.Score), optInt(g.HomeTeam.Score), strconv.Itoa(periods), g.GameOutcome.LastPeriodType,
			optInt(g.AwayTeam.SOG), optInt(g.HomeTeam.SOG), strconv.Itoa(len(g.Plays)), strconv.Itoa(shifts),
		}
		if err := t.games.Write(row); err != nil {
			return 0, err
		}
	}
	return shifts, nil
}

func optInt(v *int) string {
	if v == nil {
		return ""
	}
	return strconv.Itoa(*v)
}

func optID(v *int64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatInt(*v, 10)
}

// clockSeconds converts an "mm:ss" clock string to whole seconds, or ""
// when it is not one.
func clockSeconds(clock string) string {
	m, s, ok := strings.Cut(clock, ":")
	if !ok {
		return ""
	}
	mins, err1 := strconv.Atoi(m)
	secs, err2 := strconv.Atoi(s)
	if err1 != nil || err2 != nil || mins < 0 || secs < 0 || secs > 59 {
		return ""
	}
	return strconv.Itoa(mins*60 + secs)
}
