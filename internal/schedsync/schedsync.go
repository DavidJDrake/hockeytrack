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

type StandingsFeed interface {
	Standings(ctx context.Context) (*nhl.StandingsResponse, []byte, error)
}

type SchedulerAPI interface {
	Ensure(ctx context.Context, name string, fireAt time.Time, gameID int64) error
	Delete(ctx context.Context, name string) error
}

type Deps struct {
	Feed ScheduleFeed
	// Standings, when set, is fetched once per run after the schedule is
	// reconciled: the raw table is archived and a trimmed copy published
	// to Site (StandingsKey). Nil skips standings entirely.
	Standings StandingsFeed
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

// StandingsKey is where the website reads the published standings from.
const StandingsKey = "data/standings.json"

// StandingsPayload is the standings document the website renders. Teams are
// ordered by conference, division, and the NHL's own division rank, so the
// page never re-derives tiebreakers.
type StandingsPayload struct {
	GeneratedAt time.Time `json:"generatedAt"`
	// Season and Date are the standings' own (e.g. 20252026 and the
	// final day of that season all summer), not the run date.
	Season int64           `json:"season"`
	Date   string          `json:"date"`
	Teams  []StandingsTeam `json:"teams"`
}

type StandingsTeam struct {
	Conference string `json:"conference"`
	Division   string `json:"division"`
	Abbrev     string `json:"abbrev"`
	Name       string `json:"name"`
	Rank       int    `json:"rank"` // position within the division
	GP         int    `json:"gp"`
	W          int    `json:"w"`
	L          int    `json:"l"`
	OTL        int    `json:"otl"`
	PTS        int    `json:"pts"`
	GF         int    `json:"gf"`
	GA         int    `json:"ga"`
	Streak     string `json:"streak"`
}

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
	if err := publishSite(ctx, d, site); err != nil {
		return err
	}
	// Standings last: a failure here still surfaces as a failed run, but
	// only after every game is recorded and armed.
	return syncStandings(ctx, d, date)
}

// syncStandings archives today's raw standings and publishes the trimmed
// site document. It is a no-op without a Standings feed.
func syncStandings(ctx context.Context, d Deps, date string) error {
	if d.Standings == nil {
		return nil
	}
	st, raw, err := d.Standings.Standings(ctx)
	if err != nil {
		return fmt.Errorf("standings: %w", err)
	}
	if err := d.Archive.Put(ctx, store.StandingsKey(date), raw); err != nil {
		return err
	}
	if d.Site == nil {
		return nil
	}
	body, err := json.Marshal(TrimStandings(st, d.Now()))
	if err != nil {
		return err
	}
	return d.Site.Put(ctx, StandingsKey, body)
}

// TrimStandings reduces the API's ~90-field rows to what the page shows,
// ordered by conference, division, then the NHL's division rank.
func TrimStandings(st *nhl.StandingsResponse, now time.Time) *StandingsPayload {
	p := &StandingsPayload{GeneratedAt: now, Teams: make([]StandingsTeam, 0, len(st.Standings))}
	for _, r := range st.Standings {
		if p.Season == 0 {
			p.Season, p.Date = r.SeasonID, r.Date
		}
		p.Teams = append(p.Teams, StandingsTeam{
			Conference: r.ConferenceName, Division: r.DivisionName,
			Abbrev: r.TeamAbbrev.Default, Name: r.TeamName.Default,
			Rank: r.DivisionSequence, GP: r.GamesPlayed,
			W: r.Wins, L: r.Losses, OTL: r.OtLosses, PTS: r.Points,
			GF: r.GoalFor, GA: r.GoalAgainst, Streak: r.Streak(),
		})
	}
	sort.SliceStable(p.Teams, func(i, j int) bool {
		a, b := p.Teams[i], p.Teams[j]
		if a.Conference != b.Conference {
			return a.Conference < b.Conference
		}
		if a.Division != b.Division {
			return a.Division < b.Division
		}
		return a.Rank < b.Rank
	})
	return p
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
