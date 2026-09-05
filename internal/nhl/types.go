package nhl

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type Localized struct {
	Default string `json:"default"`
}

type ScheduleTeam struct {
	ID         int64     `json:"id"`
	Abbrev     string    `json:"abbrev"`
	PlaceName  Localized `json:"placeName"`
	CommonName Localized `json:"commonName"`
}

// Name joins place and nickname ("Tampa Bay Lightning"); falls back to the
// abbreviation when the API omits the names.
func (t ScheduleTeam) Name() string {
	n := strings.TrimSpace(t.PlaceName.Default + " " + t.CommonName.Default)
	if n == "" {
		return t.Abbrev
	}
	return n
}

type ScheduleGame struct {
	ID                int64        `json:"id"`
	Season            int64        `json:"season"`
	GameType          int          `json:"gameType"`
	StartTimeUTC      time.Time    `json:"startTimeUTC"`
	GameState         string       `json:"gameState"`
	GameScheduleState string       `json:"gameScheduleState"`
	Venue             Localized    `json:"venue"`
	AwayTeam          ScheduleTeam `json:"awayTeam"`
	HomeTeam          ScheduleTeam `json:"homeTeam"`
}

type ScheduleDay struct {
	Date  string         `json:"date"`
	Games []ScheduleGame `json:"games"`
}

type ScheduleResponse struct {
	// NextStartDate links to the following week; empty at the end of the
	// published schedule.
	NextStartDate string        `json:"nextStartDate"`
	GameWeek      []ScheduleDay `json:"gameWeek"`
}

// StandingsRow is one team's line in /v1/standings/{date}. Only the fields
// the site publishes are typed; the raw response is archived whole.
type StandingsRow struct {
	Date               string    `json:"date"`
	SeasonID           int64     `json:"seasonId"`
	ConferenceName     string    `json:"conferenceName"`
	DivisionName       string    `json:"divisionName"`
	DivisionSequence   int       `json:"divisionSequence"`
	ConferenceSequence int       `json:"conferenceSequence"`
	LeagueSequence     int       `json:"leagueSequence"`
	TeamAbbrev         Localized `json:"teamAbbrev"`
	TeamName           Localized `json:"teamName"`
	GamesPlayed        int       `json:"gamesPlayed"`
	Wins               int       `json:"wins"`
	Losses             int       `json:"losses"`
	OtLosses           int       `json:"otLosses"`
	Points             int       `json:"points"`
	GoalFor            int       `json:"goalFor"`
	GoalAgainst        int       `json:"goalAgainst"`
	StreakCode         string    `json:"streakCode"`
	StreakCount        int       `json:"streakCount"`
}

// Streak renders the API's code/count pair the way standings tables print
// it ("W3", "L2", "OT1"); empty when no games have been played.
func (r StandingsRow) Streak() string {
	if r.StreakCount == 0 || r.StreakCode == "" {
		return ""
	}
	return fmt.Sprintf("%s%d", r.StreakCode, r.StreakCount)
}

// StandingsResponse is /v1/standings/now. Off-season, the API redirects
// "now" to the final day of the previous regular season, so every row
// carries its own Date and SeasonID rather than the request date.
type StandingsResponse struct {
	WildCardIndicator bool           `json:"wildCardIndicator"`
	Standings         []StandingsRow `json:"standings"`
}

type PBPTeam struct {
	ID     int64  `json:"id"`
	Abbrev string `json:"abbrev"`
	Score  int    `json:"score"`
}

type PeriodDescriptor struct {
	Number     int    `json:"number"`
	PeriodType string `json:"periodType"`
}

type Play struct {
	EventID          int64            `json:"eventId"`
	SortOrder        int64            `json:"sortOrder"`
	TypeCode         int              `json:"typeCode"`
	TypeDescKey      string           `json:"typeDescKey"`
	TimeInPeriod     string           `json:"timeInPeriod"`
	PeriodDescriptor PeriodDescriptor `json:"periodDescriptor"`
	Details          json.RawMessage  `json:"details"`
	Raw              json.RawMessage  `json:"-"`
}

// UnmarshalJSON retains the original bytes of the play in Raw so events can
// carry the full untouched play object.
func (p *Play) UnmarshalJSON(b []byte) error {
	type alias Play
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*p = Play(a)
	p.Raw = append([]byte(nil), b...)
	return nil
}

type PlayDetails struct {
	EventOwnerTeamID int64 `json:"eventOwnerTeamId"`
	AwayScore        *int  `json:"awayScore"`
	HomeScore        *int  `json:"homeScore"`
}

// ParsedDetails extracts the commonly needed detail fields; a play with no
// details (e.g. period-start) returns the zero value.
func (p *Play) ParsedDetails() PlayDetails {
	var d PlayDetails
	if len(p.Details) > 0 {
		_ = json.Unmarshal(p.Details, &d)
	}
	return d
}

type PlayByPlay struct {
	ID        int64   `json:"id"`
	Season    int64   `json:"season"`
	GameDate  string  `json:"gameDate"`
	GameState string  `json:"gameState"`
	AwayTeam  PBPTeam `json:"awayTeam"`
	HomeTeam  PBPTeam `json:"homeTeam"`
	Plays     []Play  `json:"plays"`
}
