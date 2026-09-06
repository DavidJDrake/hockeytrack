// Package events defines the versioned EventBridge event contract.
// Consumers dedupe on (gameId, seq); delivery is at-least-once.
package events

import (
	"context"
	"encoding/json"
	"time"
)

const (
	SchemaVersion = 1
	Source        = "hockeytrack.poller"
	// SourceSynthetic marks events produced by a replayed game. Every
	// notification rule pins the real source, so a synthetic run reaches
	// no subscriber unless one opts in.
	SourceSynthetic = "hockeytrack.synthetic"
	DTPlay          = "nhl.game.play"
	DTStatus        = "nhl.game.status"
	DTFinal         = "nhl.game.final"
	DTClock         = "nhl.game.clock"
	DTRoster        = "nhl.game.roster"
	DTAlert         = "hockeytrack.alert"
)

type PlayEvent struct {
	SchemaVersion int             `json:"schemaVersion"`
	GameID        int64           `json:"gameId"`
	Seq           int64           `json:"seq"`
	PlayType      string          `json:"playType"`
	HomeTeam      string          `json:"homeTeam"`
	AwayTeam      string          `json:"awayTeam"`
	ActingTeam    string          `json:"actingTeam,omitempty"`
	ScoringTeam   string          `json:"scoringTeam,omitempty"`
	Period        int             `json:"period"`
	TimeInPeriod  string          `json:"timeInPeriod"`
	Score         map[string]int  `json:"score"`
	Raw           json.RawMessage `json:"raw"`
}

type StatusEvent struct {
	SchemaVersion int            `json:"schemaVersion"`
	GameID        int64          `json:"gameId"`
	PrevState     string         `json:"prevState"`
	GameState     string         `json:"gameState"`
	Score         map[string]int `json:"score"`
}

type FinalEvent struct {
	SchemaVersion int            `json:"schemaVersion"`
	GameID        int64          `json:"gameId"`
	HomeTeam      string         `json:"homeTeam"`
	AwayTeam      string         `json:"awayTeam"`
	Score         map[string]int `json:"score"`
	S3Prefix      string         `json:"s3Prefix"`
}

// ClockEvent is the per-poll heartbeat while a game is live: the clock,
// period, shots and situation as the feed reported them at ObservedAt.
// Consumers run their own clock from SecondsRemaining/Running/ObservedAt
// between heartbeats.
type ClockEvent struct {
	SchemaVersion    int            `json:"schemaVersion"`
	GameID           int64          `json:"gameId"`
	GameState        string         `json:"gameState"`
	Period           int            `json:"period"`
	PeriodType       string         `json:"periodType"`
	SecondsRemaining int            `json:"secondsRemaining"`
	TimeRemaining    string         `json:"timeRemaining"`
	Running          bool           `json:"running"`
	InIntermission   bool           `json:"inIntermission"`
	SituationCode    string         `json:"situationCode"`
	HomeTeam         string         `json:"homeTeam"`
	AwayTeam         string         `json:"awayTeam"`
	Score            map[string]int `json:"score"`
	Shots            map[string]int `json:"shots"`
	ObservedAt       time.Time      `json:"observedAt"`
}

// RosterPlayer maps a player id to the sweater number worn in this game.
type RosterPlayer struct {
	PlayerID int64  `json:"playerId"`
	Team     string `json:"team"`
	Number   int    `json:"number"`
	Position string `json:"position"`
}

// RosterEvent is published the first time a game's roster is seen and
// whenever it changes, so consumers can print numbers without an NHL call.
type RosterEvent struct {
	SchemaVersion int            `json:"schemaVersion"`
	GameID        int64          `json:"gameId"`
	HomeTeam      string         `json:"homeTeam"`
	AwayTeam      string         `json:"awayTeam"`
	Players       []RosterPlayer `json:"players"`
}

type AlertEvent struct {
	SchemaVersion int    `json:"schemaVersion"`
	GameID        int64  `json:"gameId"`
	Reason        string `json:"reason"`
}

type Publisher interface {
	Publish(ctx context.Context, detailType string, detail any) error
}

type PublishedEvent struct {
	DetailType string
	Detail     any
}

// FakePublisher records events in memory for tests.
type FakePublisher struct {
	Published []PublishedEvent
	Err       error
}

func (f *FakePublisher) Publish(_ context.Context, detailType string, detail any) error {
	if f.Err != nil {
		return f.Err
	}
	f.Published = append(f.Published, PublishedEvent{DetailType: detailType, Detail: detail})
	return nil
}
