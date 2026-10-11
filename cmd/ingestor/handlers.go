package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

func parsePollerPayload(b []byte) (int64, error) {
	var p struct {
		GameID int64 `json:"gameId"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return 0, err
	}
	if p.GameID == 0 {
		return 0, errors.New("payload missing gameId")
	}
	return p.GameID, nil
}

// indexerJob is what one indexer invocation does: summarize one game and
// rebuild its season, or sync a whole season (0 means the current one).
type indexerJob struct {
	Prefix string
	Season int64
}

// parseIndexerPayload accepts either the nhl.game.final event EventBridge
// delivers (the prefix is in detail.s3Prefix) or the scheduler's daily
// input ({"season": 0}).
func parseIndexerPayload(b []byte) (indexerJob, error) {
	var p struct {
		Detail struct {
			S3Prefix string `json:"s3Prefix"`
		} `json:"detail"`
		Season int64 `json:"season"`
	}
	if len(b) == 0 {
		return indexerJob{}, nil
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return indexerJob{}, err
	}
	return indexerJob{Prefix: p.Detail.S3Prefix, Season: p.Season}, nil
}

// shouldHandOff returns a predicate that is true when the context deadline is
// within buffer. With no deadline (local/ECS runs) it never hands off.
func shouldHandOff(ctx context.Context, buffer time.Duration, now func() time.Time) func() bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return func() bool { return false }
	}
	return func() bool { return now().Add(buffer).After(deadline) }
}
