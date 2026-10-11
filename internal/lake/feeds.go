package lake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
)

// localized is the NHL's {"default": "..."} string wrapper.
type localized struct {
	Default string `json:"default"`
}

type rawTeam struct {
	ID         int64     `json:"id"`
	Abbrev     string    `json:"abbrev"`
	PlaceName  localized `json:"placeName"`
	CommonName localized `json:"commonName"`
	Score      int       `json:"score"`
	SOG        int       `json:"sog"`
}

func (t rawTeam) name() string {
	return strings.TrimSpace(t.PlaceName.Default + " " + t.CommonName.Default)
}

type rawPeriod struct {
	Number     int    `json:"number"`
	PeriodType string `json:"periodType"`
}

type rawRoster struct {
	TeamID        int64     `json:"teamId"`
	PlayerID      int64     `json:"playerId"`
	FirstName     localized `json:"firstName"`
	LastName      localized `json:"lastName"`
	SweaterNumber int       `json:"sweaterNumber"`
	PositionCode  string    `json:"positionCode"`
}

type rawPlay struct {
	EventID          int64          `json:"eventId"`
	SortOrder        int64          `json:"sortOrder"`
	TypeCode         int            `json:"typeCode"`
	TypeDescKey      string         `json:"typeDescKey"`
	TimeInPeriod     string         `json:"timeInPeriod"`
	TimeRemaining    string         `json:"timeRemaining"`
	SituationCode    string         `json:"situationCode"`
	PeriodDescriptor rawPeriod      `json:"periodDescriptor"`
	Details          map[string]any `json:"details"`
}

type rawPBP struct {
	ID               int64     `json:"id"`
	Season           int64     `json:"season"`
	GameType         int       `json:"gameType"`
	GameDate         string    `json:"gameDate"`
	StartTimeUTC     string    `json:"startTimeUTC"`
	GameState        string    `json:"gameState"`
	Venue            localized `json:"venue"`
	PeriodDescriptor rawPeriod `json:"periodDescriptor"`
	GameOutcome      struct {
		LastPeriodType string `json:"lastPeriodType"`
	} `json:"gameOutcome"`
	AwayTeam    rawTeam     `json:"awayTeam"`
	HomeTeam    rawTeam     `json:"homeTeam"`
	Plays       []rawPlay   `json:"plays"`
	RosterSpots []rawRoster `json:"rosterSpots"`
}

type rawSkater struct {
	PlayerID       int64     `json:"playerId"`
	SweaterNumber  int       `json:"sweaterNumber"`
	Name           localized `json:"name"`
	Position       string    `json:"position"`
	Goals          int       `json:"goals"`
	Assists        int       `json:"assists"`
	Points         int       `json:"points"`
	PlusMinus      int       `json:"plusMinus"`
	PIM            int       `json:"pim"`
	Hits           int       `json:"hits"`
	PowerPlayGoals int       `json:"powerPlayGoals"`
	SOG            int       `json:"sog"`
	BlockedShots   int       `json:"blockedShots"`
	Shifts         int       `json:"shifts"`
	Giveaways      int       `json:"giveaways"`
	Takeaways      int       `json:"takeaways"`
	TOI            string    `json:"toi"`
}

type rawGoalie struct {
	PlayerID         int64     `json:"playerId"`
	SweaterNumber    int       `json:"sweaterNumber"`
	Name             localized `json:"name"`
	GoalsAgainst     int       `json:"goalsAgainst"`
	ShotsAgainst     int       `json:"shotsAgainst"`
	Saves            int       `json:"saves"`
	SaveShotsAgainst string    `json:"saveShotsAgainst"` // "34/37", the older shape
	TOI              string    `json:"toi"`
	Starter          bool      `json:"starter"`
	Decision         string    `json:"decision"`
}

type rawBoxTeam struct {
	Forwards []rawSkater `json:"forwards"`
	Defense  []rawSkater `json:"defense"`
	Goalies  []rawGoalie `json:"goalies"`
}

type rawBoxscore struct {
	ID                int64 `json:"id"`
	PlayerByGameStats struct {
		AwayTeam rawBoxTeam `json:"awayTeam"`
		HomeTeam rawBoxTeam `json:"homeTeam"`
	} `json:"playerByGameStats"`
}

type rawLanding struct {
	Summary struct {
		ThreeStars []struct {
			Star       int       `json:"star"`
			PlayerID   int64     `json:"playerId"`
			TeamAbbrev string    `json:"teamAbbrev"`
			Name       localized `json:"name"`
			Position   string    `json:"position"`
			Goals      int       `json:"goals"`
			Assists    int       `json:"assists"`
			Points     int       `json:"points"`
		} `json:"threeStars"`
	} `json:"summary"`
}

// shiftTypeCode marks a real shift; the chart also carries goal markers
// (typeCode 505) that duplicate the play-by-play.
const shiftTypeCode = 517

type rawShift struct {
	ID          int64  `json:"id"`
	PlayerID    int64  `json:"playerId"`
	FirstName   string `json:"firstName"`
	LastName    string `json:"lastName"`
	TeamAbbrev  string `json:"teamAbbrev"`
	Period      int    `json:"period"`
	ShiftNumber int    `json:"shiftNumber"`
	StartTime   string `json:"startTime"`
	EndTime     string `json:"endTime"`
	Duration    string `json:"duration"`
	TypeCode    int    `json:"typeCode"`
}

type rawShiftChart struct {
	Data []rawShift `json:"data"`
}

// decode parses untrusted JSON into dst. A value of the wrong type for a
// field leaves that field zero and the rest filled, which encoding/json
// already does; only malformed JSON is an error. Numbers inside the plays'
// details are kept as json.Number so they pass through unchanged.
func decode(feed string, b []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	err := d.Decode(dst)
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		slog.Warn("feed field has an unexpected type; left blank", "feed", feed, "field", typeErr.Field, "value", typeErr.Value)
		return nil
	}
	if err != nil {
		return fmt.Errorf("parse %s: %w", feed, err)
	}
	return nil
}

// numField reads a numeric detail, whatever numeric shape the feed used.
func numField(d map[string]any, key string) (float64, bool) {
	v, ok := d[key]
	if !ok || v == nil {
		return 0, false
	}
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

func idField(d map[string]any, key string) (int64, bool) {
	f, ok := numField(d, key)
	if !ok || f != float64(int64(f)) {
		return 0, false
	}
	return int64(f), true
}

func intField(d map[string]any, key string) (int, bool) {
	id, ok := idField(d, key)
	return int(id), ok
}

func strField(d map[string]any, key string) string {
	s, _ := d[key].(string)
	return s
}

// clockSeconds converts "mm:ss" (or "mmmm:ss" for a season total) to whole
// seconds; -1 when the string is not a clock.
func clockSeconds(clock string) int {
	m, s, ok := strings.Cut(clock, ":")
	if !ok {
		return -1
	}
	mins, err1 := strconv.Atoi(m)
	secs, err2 := strconv.Atoi(s)
	if err1 != nil || err2 != nil || mins < 0 || secs < 0 || secs > 59 {
		return -1
	}
	return mins*60 + secs
}

// Feeds locates a game's objects under its archive prefix. Final is set
// when the end-of-game sweep is present; otherwise the keys point at the
// newest live snapshot of each feed, and absent feeds are empty.
type Feeds struct {
	Prefix   string
	Final    bool
	PBP      string
	Boxscore string
	Landing  string
	Shifts   string
}

// ErrNoFeeds is returned when a prefix holds no play-by-play at all.
var ErrNoFeeds = errors.New("no play-by-play under prefix")

// Locate lists prefix once and picks the feeds to read.
func Locate(ctx context.Context, src Source, prefix string) (Feeds, error) {
	keys, err := src.List(ctx, prefix)
	if err != nil {
		return Feeds{}, err
	}
	return locate(prefix, keys)
}

func locate(prefix string, keys []string) (Feeds, error) {
	f := Feeds{Prefix: prefix}
	have := map[string]bool{}
	newest := map[string]string{}
	for _, k := range keys {
		rel := strings.TrimPrefix(k, prefix)
		have[rel] = true
		// Snapshot keys are {feed}/{yyyymmddThhmmssZ}.json, and the
		// timestamp format sorts lexically.
		if feed, ts, ok := strings.Cut(rel, "/"); ok && feed != "final" && strings.HasSuffix(ts, ".json") && ts > newest[feed] {
			newest[feed] = ts
		}
	}
	if have["final/pbp.json"] {
		f.Final = true
		f.PBP = prefix + "final/pbp.json"
		for _, feed := range []string{"boxscore", "landing", "shifts"} {
			if have["final/"+feed+".json"] {
				f.set(feed, prefix+"final/"+feed+".json")
			}
		}
		return f, nil
	}
	if newest["pbp"] == "" {
		return f, ErrNoFeeds
	}
	f.PBP = prefix + "pbp/" + newest["pbp"]
	if ts := newest["boxscore"]; ts != "" {
		f.Boxscore = prefix + "boxscore/" + ts
	}
	return f, nil
}

func (f *Feeds) set(feed, key string) {
	switch feed {
	case "boxscore":
		f.Boxscore = key
	case "landing":
		f.Landing = key
	case "shifts":
		f.Shifts = key
	}
}

// Bodies is the raw bytes of a game's feeds; only PBP is guaranteed.
type Bodies struct {
	PBP, Boxscore, Landing, Shifts []byte
}

// Load fetches every located feed. A missing optional feed is logged and
// left nil; a missing play-by-play is an error.
func Load(ctx context.Context, src Source, f Feeds) (Bodies, error) {
	var b Bodies
	var err error
	if b.PBP, err = src.Get(ctx, f.PBP); err != nil {
		return b, fmt.Errorf("get %s: %w", f.PBP, err)
	}
	for _, opt := range []struct {
		key string
		dst *[]byte
	}{{f.Boxscore, &b.Boxscore}, {f.Landing, &b.Landing}, {f.Shifts, &b.Shifts}} {
		if opt.key == "" {
			continue
		}
		body, err := src.Get(ctx, opt.key)
		if err != nil {
			slog.Warn("optional feed unavailable", "key", opt.key, "err", err)
			continue
		}
		*opt.dst = body
	}
	return b, nil
}

// Plays flattens a play-by-play feed into typed plays, in the NHL's order.
func Plays(pbp []byte) ([]Play, error) {
	var raw rawPBP
	if err := decode("pbp", pbp, &raw); err != nil {
		return nil, err
	}
	if raw.ID == 0 {
		return nil, errors.New("pbp has no game id")
	}
	r := newRoster(&raw)
	out := make([]Play, 0, len(raw.Plays))
	for _, p := range raw.Plays {
		out = append(out, r.play(p))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// playerRoles maps each detail key naming a player to the role it means.
var playerRoles = []struct{ key, role string }{
	{"scoringPlayerId", "scorer"}, {"assist1PlayerId", "assist1"}, {"assist2PlayerId", "assist2"},
	{"shootingPlayerId", "shooter"}, {"goalieInNetId", "goalie"}, {"blockingPlayerId", "blocker"},
	{"winningPlayerId", "winner"}, {"losingPlayerId", "loser"},
	{"hittingPlayerId", "hitter"}, {"hitteePlayerId", "hittee"},
	{"committedByPlayerId", "committedBy"}, {"drawnByPlayerId", "drawnBy"}, {"servedByPlayerId", "servedBy"},
	{"playerId", "player"},
}

func (r *roster) play(p rawPlay) Play {
	out := Play{
		Seq: p.SortOrder, EventID: p.EventID,
		Period: p.PeriodDescriptor.Number, PeriodType: p.PeriodDescriptor.PeriodType,
		TimeInPeriod: p.TimeInPeriod, TimeRemaining: p.TimeRemaining,
		Type: p.TypeDescKey, TypeCode: p.TypeCode, SituationCode: p.SituationCode,
		Details: p.Details,
	}
	d := p.Details
	if id, ok := idField(d, "eventOwnerTeamId"); ok {
		out.Team = r.teamAbbrev[id]
	}
	for _, pr := range playerRoles {
		if id, ok := idField(d, pr.key); ok {
			out.Players = append(out.Players, PlayPlayer{Role: pr.role, ID: id, Name: r.name[id]})
		}
	}
	if x, ok := numField(d, "xCoord"); ok {
		out.X = &x
	}
	if y, ok := numField(d, "yCoord"); ok {
		out.Y = &y
	}
	out.Zone = strField(d, "zoneCode")
	if n, ok := intField(d, "awayScore"); ok {
		out.AwayScore = &n
	}
	if n, ok := intField(d, "homeScore"); ok {
		out.HomeScore = &n
	}
	return out
}

// Shifts flattens a shift chart into rows, dropping the goal markers.
func Shifts(chart []byte) ([]Shift, error) {
	var raw rawShiftChart
	if err := decode("shifts", chart, &raw); err != nil {
		return nil, err
	}
	out := make([]Shift, 0, len(raw.Data))
	for _, s := range raw.Data {
		if s.TypeCode != shiftTypeCode {
			continue
		}
		out = append(out, Shift{
			ID: s.ID, PlayerID: s.PlayerID, Name: strings.TrimSpace(s.FirstName + " " + s.LastName),
			Team: s.TeamAbbrev, Period: s.Period, ShiftNumber: s.ShiftNumber,
			StartTime: s.StartTime, EndTime: s.EndTime, Duration: s.Duration,
			Seconds: max(clockSeconds(s.Duration), 0),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Period != b.Period {
			return a.Period < b.Period
		}
		if a.StartTime != b.StartTime {
			return a.StartTime < b.StartTime
		}
		return a.ID < b.ID
	})
	return out, nil
}

// roster is what the play-by-play says about who is who in this game.
type roster struct {
	teamAbbrev map[int64]string
	name       map[int64]string
	number     map[int64]int
	position   map[int64]string
	team       map[int64]string // player id -> team abbrev
}

func newRoster(p *rawPBP) *roster {
	r := &roster{
		teamAbbrev: map[int64]string{p.AwayTeam.ID: p.AwayTeam.Abbrev, p.HomeTeam.ID: p.HomeTeam.Abbrev},
		name:       map[int64]string{}, number: map[int64]int{}, position: map[int64]string{}, team: map[int64]string{},
	}
	for _, s := range p.RosterSpots {
		if s.PlayerID == 0 {
			continue
		}
		if n := strings.TrimSpace(s.FirstName.Default + " " + s.LastName.Default); n != "" {
			r.name[s.PlayerID] = n
		}
		r.number[s.PlayerID] = s.SweaterNumber
		r.position[s.PlayerID] = s.PositionCode
		r.team[s.PlayerID] = r.teamAbbrev[s.TeamID]
	}
	return r
}
