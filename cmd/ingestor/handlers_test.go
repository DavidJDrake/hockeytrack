package main

import (
	"context"
	"testing"
	"time"
)

func TestParsePollerPayload(t *testing.T) {
	id, err := parsePollerPayload([]byte(`{"gameId":2025020740}`))
	if err != nil || id != 2025020740 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	if _, err := parsePollerPayload([]byte(`{}`)); err == nil {
		t.Error("missing gameId should error")
	}
	if _, err := parsePollerPayload([]byte(`not json`)); err == nil {
		t.Error("bad json should error")
	}
}

func TestParseIndexerPayload(t *testing.T) {
	job, err := parseIndexerPayload([]byte(`{"source":"hockeytrack.poller","detail-type":"nhl.game.final","detail":{"gameId":2025020001,"s3Prefix":"raw/20252026/2025-10-07/2025020001/"}}`))
	if err != nil || job.Prefix != "raw/20252026/2025-10-07/2025020001/" || job.Season != 0 {
		t.Errorf("final event: %+v %v", job, err)
	}
	job, err = parseIndexerPayload([]byte(`{"season":20242025}`))
	if err != nil || job.Prefix != "" || job.Season != 20242025 {
		t.Errorf("season job: %+v %v", job, err)
	}
	if job, err := parseIndexerPayload(nil); err != nil || job != (indexerJob{}) {
		t.Errorf("empty payload: %+v %v", job, err)
	}
	if _, err := parseIndexerPayload([]byte(`nope`)); err == nil {
		t.Error("bad json should error")
	}
}

func TestHandoffPredicate(t *testing.T) {
	deadline := time.Date(2026, 1, 15, 23, 15, 0, 0, time.UTC)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	early := func() time.Time { return deadline.Add(-5 * time.Minute) }
	late := func() time.Time { return deadline.Add(-30 * time.Second) }

	if shouldHandOff(ctx, 60*time.Second, early)() {
		t.Error("handed off with 5 minutes remaining")
	}
	if !shouldHandOff(ctx, 60*time.Second, late)() {
		t.Error("did not hand off with 30s remaining")
	}
	// No deadline (local runs): never hand off.
	if shouldHandOff(context.Background(), 60*time.Second, early)() {
		t.Error("handed off without a deadline")
	}
}
