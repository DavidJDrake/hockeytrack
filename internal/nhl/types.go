package nhl

import "time"

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
