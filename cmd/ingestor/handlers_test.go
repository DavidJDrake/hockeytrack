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
