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

// shouldHandOff returns a predicate that is true when the context deadline is
// within buffer. With no deadline (local/ECS runs) it never hands off.
func shouldHandOff(ctx context.Context, buffer time.Duration, now func() time.Time) func() bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return func() bool { return false }
	}
	return func() bool { return now().Add(buffer).After(deadline) }
}
