// Package events defines the versioned EventBridge event contract.
// Consumers dedupe on (gameId, seq); delivery is at-least-once.
package events

import (
	"context"
	"encoding/json"
)

const (
	SchemaVersion = 1
	Source        = "hockeytrack.poller"
	DTPlay        = "nhl.game.play"
	DTStatus      = "nhl.game.status"
	DTFinal       = "nhl.game.final"
	DTAlert       = "hockeytrack.alert"
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
