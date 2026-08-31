// Package schedsync pulls the NHL schedule and reconciles per-game
// EventBridge Scheduler entries against it.
package schedsync

import (
	"context"
	"fmt"
	"sync"
	"time"

	"hockeytrack/internal/nhl"
	"hockeytrack/internal/store"
)

type ScheduleFeed interface {
	Schedule(ctx context.Context, date string) (*nhl.ScheduleResponse, []byte, error)
}

type SchedulerAPI interface {
	Ensure(ctx context.Context, name string, fireAt time.Time, gameID int64) error
	Delete(ctx context.Context, name string) error
}

type Deps struct {
	Feed      ScheduleFeed
	Store     store.GameStore
	Archive   store.Archive
	Scheduler SchedulerAPI
	Now       func() time.Time
}

type Config struct {
	PregameBuffer time.Duration
}

func EntryName(gameID int64) string {
	return fmt.Sprintf("hockeytrack-game-%d", gameID)
}

func Sync(ctx context.Context, d Deps, cfg Config, date string) error {
	sched, raw, err := d.Feed.Schedule(ctx, date)
	if err != nil {
		return err
	}
	if err := d.Archive.Put(ctx, store.ScheduleKey(date), raw); err != nil {
		return err
	}
	for _, day := range sched.GameWeek {
		for _, g := range day.Games {
			name := EntryName(g.ID)
			rec := store.GameRecord{
				GameID: g.ID, Season: g.Season, GameDate: day.Date,
				StartTimeUTC: g.StartTimeUTC,
				HomeAbbrev:   g.HomeTeam.Abbrev, AwayAbbrev: g.AwayTeam.Abbrev,
				Venue: g.Venue.Default, GameState: g.GameState, ScheduleEntryName: name,
			}
			if err := d.Store.UpsertSchedule(ctx, rec); err != nil {
				return err
			}
			switch {
			case g.GameScheduleState != "OK":
				if err := d.Scheduler.Delete(ctx, name); err != nil {
					return err
				}
			case g.StartTimeUTC.After(d.Now()) && !isFinal(g.GameState):
				if err := d.Scheduler.Ensure(ctx, name, g.StartTimeUTC.Add(-cfg.PregameBuffer), g.ID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func isFinal(s string) bool { return s == "FINAL" || s == "OFF" }

type FakeEntry struct {
	FireAt time.Time
	GameID int64
}

type FakeScheduler struct {
	mu      sync.Mutex
	Entries map[string]FakeEntry
}

func NewFakeScheduler() *FakeScheduler {
	return &FakeScheduler{Entries: map[string]FakeEntry{}}
}

func (f *FakeScheduler) Ensure(_ context.Context, name string, fireAt time.Time, gameID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Entries[name] = FakeEntry{FireAt: fireAt, GameID: gameID}
	return nil
}

func (f *FakeScheduler) Delete(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.Entries, name)
	return nil
}
