package store

import (
	"context"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 15, 23, 0, 0, 0, time.UTC)

func newFake(now time.Time) *FakeGameStore {
	f := NewFakeGameStore()
	f.Now = func() time.Time { return now }
	return f
}

func TestUpsertPreservesPollerFields(t *testing.T) {
	ctx := context.Background()
	f := newFake(t0)
	rec := GameRecord{GameID: 1, GameDate: "2026-01-15", HomeAbbrev: "BUF", AwayAbbrev: "MTL", StartTimeUTC: t0}
	if err := f.UpsertSchedule(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := f.UpdatePollerState(ctx, 1, PollerState{LastPlaySortOrder: 50, ChainCount: 2, GameState: "LIVE"}); err != nil {
		t.Fatal(err)
	}
	rec.Venue = "KeyBank Center" // schedule-sync runs again with updated info
	if err := f.UpsertSchedule(ctx, rec); err != nil {
		t.Fatal(err)
	}
	got, _ := f.Get(ctx, 1)
	if got.LastPlaySortOrder != 50 || got.ChainCount != 2 {
		t.Errorf("poller fields clobbered: %+v", got)
	}
	if got.Venue != "KeyBank Center" {
		t.Errorf("schedule field not updated: %q", got.Venue)
	}
}

func TestLeaseSemantics(t *testing.T) {
	ctx := context.Background()
	f := newFake(t0)
	f.UpsertSchedule(ctx, GameRecord{GameID: 1, GameDate: "2026-01-15"})

	ok, err := f.AcquireLease(ctx, 1, "workerA", t0.Add(60*time.Second))
	if err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v", ok, err)
	}
	// Second worker cannot steal an unexpired lease.
	if ok, _ := f.AcquireLease(ctx, 1, "workerB", t0.Add(60*time.Second)); ok {
		t.Error("workerB acquired unexpired lease held by workerA")
	}
	// Same owner can re-acquire.
	if ok, _ := f.AcquireLease(ctx, 1, "workerA", t0.Add(90*time.Second)); !ok {
		t.Error("workerA could not re-acquire its own lease")
	}
	// Renew only for the owner.
	if ok, _ := f.RenewLease(ctx, 1, "workerB", t0.Add(120*time.Second)); ok {
		t.Error("workerB renewed a lease it does not own")
	}
	if ok, _ := f.RenewLease(ctx, 1, "workerA", t0.Add(120*time.Second)); !ok {
		t.Error("workerA could not renew")
	}
	// After expiry another worker can take it.
	f.Now = func() time.Time { return t0.Add(3 * time.Minute) }
	if ok, _ := f.AcquireLease(ctx, 1, "workerB", t0.Add(4*time.Minute)); !ok {
		t.Error("workerB could not acquire expired lease")
	}
}

func TestReleaseLease(t *testing.T) {
	ctx := context.Background()
	f := newFake(t0)
	f.UpsertSchedule(ctx, GameRecord{GameID: 1, GameDate: "2026-01-15"})
	f.AcquireLease(ctx, 1, "workerA", t0.Add(time.Minute))
	if err := f.ReleaseLease(ctx, 1, "workerA"); err != nil {
		t.Fatal(err)
	}
	// Released lease is immediately acquirable by another worker.
	if ok, _ := f.AcquireLease(ctx, 1, "workerB", t0.Add(time.Minute)); !ok {
		t.Error("lease not acquirable after release")
	}
	// Releasing someone else's lease is a no-op, not an error.
	if err := f.ReleaseLease(ctx, 1, "workerA"); err != nil {
		t.Errorf("stale release errored: %v", err)
	}
}

func TestListByDateAndMissingGet(t *testing.T) {
	ctx := context.Background()
	f := newFake(t0)
	f.UpsertSchedule(ctx, GameRecord{GameID: 1, GameDate: "2026-01-15"})
	f.UpsertSchedule(ctx, GameRecord{GameID: 2, GameDate: "2026-01-15"})
	f.UpsertSchedule(ctx, GameRecord{GameID: 3, GameDate: "2026-01-16"})
	games, err := f.ListByDate(ctx, "2026-01-15")
	if err != nil || len(games) != 2 {
		t.Fatalf("ListByDate: %d games, err=%v", len(games), err)
	}
	got, err := f.Get(ctx, 999)
	if err != nil || got != nil {
		t.Errorf("missing game: got=%v err=%v, want nil,nil", got, err)
	}
}
