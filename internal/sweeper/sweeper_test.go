package sweeper

import (
	"context"
	"testing"
	"time"

	"hockeytrack/internal/store"
)

var now = time.Date(2026, 1, 16, 1, 0, 0, 0, time.UTC) // 8pm ET Jan 15

func seed(t *testing.T, gs *store.FakeGameStore, id int64, start time.Time, state string, done bool, leaseUntil time.Time) {
	t.Helper()
	ctx := context.Background()
	gs.UpsertSchedule(ctx, store.GameRecord{GameID: id, GameDate: start.UTC().Format("2006-01-02"), StartTimeUTC: start})
	gs.UpdatePollerState(ctx, id, store.PollerState{GameState: state, Done: done})
	if !leaseUntil.IsZero() {
		gs.AcquireLease(ctx, id, "someworker", leaseUntil)
	}
}

func TestSweepRestartsOnlyDeadLiveGames(t *testing.T) {
	gs := store.NewFakeGameStore()
	gs.Now = func() time.Time { return now }
	inv := &FakeInvoker{}

	gameStart := now.Add(-90 * time.Minute)
	seed(t, gs, 1, gameStart, "LIVE", false, time.Time{})             // dead: no lease -> restart
	seed(t, gs, 2, gameStart, "LIVE", false, now.Add(-time.Minute))   // dead: expired lease -> restart
	seed(t, gs, 3, gameStart, "LIVE", false, now.Add(time.Minute))    // healthy lease -> skip
	seed(t, gs, 4, gameStart, "OFF", true, time.Time{})               // done -> skip
	seed(t, gs, 5, now.Add(2*time.Hour), "FUT", false, time.Time{})   // not started -> skip
	seed(t, gs, 6, now.Add(-7*time.Hour), "LIVE", false, time.Time{}) // outside give-up window -> skip

	if err := Sweep(context.Background(), gs, inv, now); err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, id := range inv.Invoked {
		got[id] = true
	}
	if !got[1] || !got[2] {
		t.Errorf("dead games not restarted: invoked=%v", inv.Invoked)
	}
	if got[3] || got[4] || got[5] || got[6] {
		t.Errorf("healthy/done/future/expired games invoked: %v", inv.Invoked)
	}
}

func TestSweepCoversYesterdayUTC(t *testing.T) {
	// A 10pm ET game starts 03:00 UTC next day but its gameDate row may be
	// keyed to the previous UTC date after midnight; sweep must check both days.
	gs := store.NewFakeGameStore()
	gs.Now = func() time.Time { return now }
	inv := &FakeInvoker{}
	yesterdayStart := now.Add(-2 * time.Hour) // Jan 15 UTC date, now is Jan 16 UTC
	seed(t, gs, 7, yesterdayStart, "LIVE", false, time.Time{})
	if err := Sweep(context.Background(), gs, inv, now); err != nil {
		t.Fatal(err)
	}
	if len(inv.Invoked) != 1 || inv.Invoked[0] != 7 {
		t.Errorf("yesterday's live game not restarted: %v", inv.Invoked)
	}
}
