package nhl

import (
	"encoding/json"
	"time"
)

type Localized struct {
	Default string `json:"default"`
}

type ScheduleTeam struct {
	ID     int64  `json:"id"`
	Abbrev string `json:"abbrev"`
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
	GameWeek []ScheduleDay `json:"gameWeek"`
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
