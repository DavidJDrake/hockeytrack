// Package schedsync pulls the NHL schedule and reconciles per-game
// EventBridge Scheduler entries against it.
package schedsync

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
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
	// Site, when set, receives the website's schedule document
	// (SiteKey) at the end of every sync.
	Site store.Archive
	Now  func() time.Time
}

// SiteKey is where the website reads the published schedule from.
const SiteKey = "data/schedule.json"

// SitePayload is the schedule document the website renders.
type SitePayload struct {
	GeneratedAt time.Time         `json:"generatedAt"`
	Teams       map[string]string `json:"teams"`
	Games       []SiteGame        `json:"games"`
}

type SiteGame struct {
	ID    int64     `json:"id"`
	Date  string    `json:"date"`
	Start time.Time `json:"start"`
	Away  string    `json:"away"`
	Home  string    `json:"home"`
	Type  int       `json:"type"`
	Venue string    `json:"venue"`
}

type Config struct {
	PregameBuffer time.Duration
	// Horizon bounds how far past the start date Sync follows the API's
	// week-to-week links. Zero means the single requested week.
	Horizon time.Duration
}

func EntryName(gameID int64) string {
	return fmt.Sprintf("hockeytrack-game-%d", gameID)
}

// Sync pulls every published week from date through date+Horizon, following
// the API's nextStartDate links, and reconciles games and scheduler entries.
// The first week's raw response is always archived; later weeks only when
// they hold games, so an off-season run does not litter the archive.
func Sync(ctx context.Context, d Deps, cfg Config, date string) error {
	start, err := time.Parse("2006-01-02", date)
	if err != nil {
		return fmt.Errorf("sync date %q: %w", date, err)
	}
	limit := start.Add(cfg.Horizon)
	site := &SitePayload{GeneratedAt: d.Now(), Teams: map[string]string{}}
	for week, first := date, true; week != ""; first = false {
		weekStart, err := time.Parse("2006-01-02", week)
		if err != nil {
			return fmt.Errorf("week start %q: %w", week, err)
		}
		if !first && weekStart.After(limit) {
			break
		}
		next, err := syncWeek(ctx, d, cfg, week, first, site)
		if err != nil {
			return err
		}
		if next <= week {
			break // no link, or a link that does not advance
		}
		week = next
	}
	return publishSite(ctx, d, site)
}

func publishSite(ctx context.Context, d Deps, site *SitePayload) error {
	if d.Site == nil {
		return nil
	}
	sort.Slice(site.Games, func(i, j int) bool {
		if !site.Games[i].Start.Equal(site.Games[j].Start) {
			return site.Games[i].Start.Before(site.Games[j].Start)
		}
		return site.Games[i].ID < site.Games[j].ID
	})
	body, err := json.Marshal(site)
	if err != nil {
		return err
	}
	return d.Site.Put(ctx, SiteKey, body)
}

func syncWeek(ctx context.Context, d Deps, cfg Config, date string, archiveAlways bool, site *SitePayload) (string, error) {
	sched, raw, err := d.Feed.Schedule(ctx, date)
	if err != nil {
		return "", err
	}
	hasGames := false
	for _, day := range sched.GameWeek {
		if len(day.Games) > 0 {
			hasGames = true
			break
		}
	}
	if archiveAlways || hasGames {
		if err := d.Archive.Put(ctx, store.ScheduleKey(date), raw); err != nil {
			return "", err
		}
	}
	for _, day := range sched.GameWeek {
		for _, g := range day.Games {
			name := EntryName(g.ID)
			site.Teams[g.AwayTeam.Abbrev] = g.AwayTeam.Name()
			site.Teams[g.HomeTeam.Abbrev] = g.HomeTeam.Name()
			site.Games = append(site.Games, SiteGame{
				ID: g.ID, Date: day.Date, Start: g.StartTimeUTC,
				Away: g.AwayTeam.Abbrev, Home: g.HomeTeam.Abbrev,
				Type: g.GameType, Venue: g.Venue.Default,
			})
			rec := store.GameRecord{
				GameID: g.ID, Season: g.Season, GameDate: day.Date,
				StartTimeUTC: g.StartTimeUTC,
				HomeAbbrev:   g.HomeTeam.Abbrev, AwayAbbrev: g.AwayTeam.Abbrev,
				Venue: g.Venue.Default, GameState: g.GameState, ScheduleEntryName: name,
			}
			if err := d.Store.UpsertSchedule(ctx, rec); err != nil {
				return "", err
			}
			switch {
			case g.GameScheduleState != "OK":
				if err := d.Scheduler.Delete(ctx, name); err != nil {
					return "", err
				}
			case g.StartTimeUTC.After(d.Now()) && !isFinal(g.GameState):
				if err := d.Scheduler.Ensure(ctx, name, g.StartTimeUTC.Add(-cfg.PregameBuffer), g.ID); err != nil {
					return "", err
				}
			}
		}
	}
	return sched.NextStartDate, nil
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
