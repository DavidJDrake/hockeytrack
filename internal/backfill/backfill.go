// Package backfill archives the final feeds of every finished game in a past
// season. It is the forward-only pipeline's one concession to history: no
// polling, no events, no scheduler entries, no DynamoDB writes — just the
// same final/ objects the poller leaves behind, written for games it never
// saw. Runs are resumable: objects already in the archive are not re-fetched.
package backfill

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"hockeytrack/internal/nhl"
	"hockeytrack/internal/store"
)

type Feed interface {
	Schedule(ctx context.Context, date string) (*nhl.ScheduleResponse, []byte, error)
	RawFeed(ctx context.Context, gameID int64, feed string) ([]byte, error)
	ShiftCharts(ctx context.Context, gameID int64) ([]byte, error)
}

type Deps struct {
	Feed    Feed
	Archive store.Lister
	Now     func() time.Time
	Sleep   func(ctx context.Context, d time.Duration) error
}

type Config struct {
	// Interval is the minimum gap between consecutive NHL requests. Zero
	// disables pacing (tests only; be polite in production).
	Interval time.Duration
	// MaxAttempts bounds retries of a single feed on throttles, upstream
	// faults and network errors. Zero means DefaultMaxAttempts.
	MaxAttempts int
	// Backoff is the first retry delay; it doubles per attempt. Zero means
	// DefaultBackoff.
	Backoff time.Duration
}

const (
	DefaultMaxAttempts = 5
	DefaultBackoff     = time.Second

	// FirstShiftSeason is the first season for which the shift-chart feed
	// has data; earlier seasons return an empty set and are not requested.
	FirstShiftSeason int64 = 20102011
)

type Stats struct {
	Weeks   int // schedule weeks walked
	Games   int // finished games belonging to the season
	Fetched int // feed objects written
	Skipped int // feed objects already present
	Missing int // feeds the API reports absent (404)
	Failed  int // feeds that failed after all retries
}

// StartDate is the date the season walk begins from: September 1 of the
// season's first year, comfortably before any NHL season has started.
func StartDate(season int64) string {
	return fmt.Sprintf("%04d-09-01", season/10000)
}

// endCap bounds the walk for a season whose next-season week never appears
// (the current season, or an API that stops linking forward).
func endCap(season int64) time.Time {
	return time.Date(int(season%10000)+1, time.January, 1, 0, 0, 0, 0, time.UTC)
}

func feeds(season int64) []string {
	if season >= FirstShiftSeason {
		return []string{"pbp", "boxscore", "landing", "shifts"}
	}
	return []string{"pbp", "boxscore", "landing"}
}

func isFinal(s string) bool { return s == "FINAL" || s == "OFF" }

type runner struct {
	d        Deps
	cfg      Config
	season   int64
	existing map[string]bool
	last     time.Time
	stats    Stats
	failures []error
}

// Run walks the season's schedule week by week from StartDate, archiving
// each week that holds the season's games, and fetches the final feeds of
// every finished game. It stops at the first week that belongs to a later
// season. A non-nil error with a populated Stats means some feeds failed
// permanently; rerunning picks them up because their objects are absent.
func Run(ctx context.Context, d Deps, cfg Config, season int64) (Stats, error) {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = DefaultBackoff
	}
	r := &runner{d: d, cfg: cfg, season: season}

	keys, err := d.Archive.List(ctx, fmt.Sprintf("raw/%d/", season))
	if err != nil {
		return r.stats, fmt.Errorf("list archive: %w", err)
	}
	r.existing = make(map[string]bool, len(keys))
	for _, k := range keys {
		r.existing[k] = true
	}
	slog.Info("backfill start", "season", season, "existingObjects", len(keys))

	cap := endCap(season)
	for week := StartDate(season); week != ""; {
		weekStart, err := time.Parse("2006-01-02", week)
		if err != nil {
			return r.stats, fmt.Errorf("week start %q: %w", week, err)
		}
		if weekStart.After(cap) {
			break
		}
		next, done, err := r.week(ctx, week)
		if err != nil {
			return r.stats, err
		}
		if done || next <= week {
			break
		}
		week = next
	}

	slog.Info("backfill finished", "season", season, "stats", r.stats)
	if r.stats.Failed > 0 {
		return r.stats, fmt.Errorf("%d feeds failed permanently (first: %w)", r.stats.Failed, r.failures[0])
	}
	return r.stats, nil
}

// week processes one schedule week and reports whether the walk should stop
// because a later season has begun.
func (r *runner) week(ctx context.Context, date string) (next string, done bool, err error) {
	var sched *nhl.ScheduleResponse
	var raw []byte
	err = r.fetch(ctx, "schedule/"+date, func() error {
		var e error
		sched, raw, e = r.d.Feed.Schedule(ctx, date)
		return e
	})
	if err != nil {
		return "", false, fmt.Errorf("schedule %s: %w", date, err)
	}
	if sched == nil {
		return "", false, fmt.Errorf("schedule %s: not available", date)
	}
	r.stats.Weeks++

	ours := false
	for _, day := range sched.GameWeek {
		for _, g := range day.Games {
			switch {
			case g.Season > r.season:
				done = true
			case g.Season == r.season:
				ours = true
			}
		}
	}
	if ours {
		if err := r.d.Archive.Put(ctx, store.ScheduleKey(date), raw); err != nil {
			return "", false, fmt.Errorf("archive week %s: %w", date, err)
		}
	}
	for _, day := range sched.GameWeek {
		for _, g := range day.Games {
			if g.Season != r.season || !isFinal(g.GameState) {
				continue
			}
			r.stats.Games++
			if err := r.game(ctx, day.Date, g.ID); err != nil {
				return "", false, err
			}
		}
	}
	slog.Info("backfill week", "season", r.season, "week", date, "games", r.stats.Games, "fetched", r.stats.Fetched, "skipped", r.stats.Skipped)
	return sched.NextStartDate, done, nil
}

func (r *runner) game(ctx context.Context, date string, id int64) error {
	for _, feed := range feeds(r.season) {
		key := store.FinalKey(r.season, date, id, feed)
		if r.existing[key] {
			r.stats.Skipped++
			continue
		}
		var body []byte
		err := r.fetch(ctx, fmt.Sprintf("%s/%d", feed, id), func() error {
			var e error
			if feed == "shifts" {
				body, e = r.d.Feed.ShiftCharts(ctx, id)
			} else {
				body, e = r.d.Feed.RawFeed(ctx, id, gamecenterFeed(feed))
			}
			return e
		})
		switch {
		case err == nil:
			if err := r.d.Archive.Put(ctx, key, body); err != nil {
				return fmt.Errorf("archive %s: %w", key, err)
			}
			r.stats.Fetched++
		case isNotFound(err):
			r.stats.Missing++
			slog.Warn("backfill feed absent", "gameId", id, "feed", feed)
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return err
		default:
			r.stats.Failed++
			r.failures = append(r.failures, fmt.Errorf("game %d %s: %w", id, feed, err))
			slog.Error("backfill feed failed", "gameId", id, "feed", feed, "err", err)
		}
	}
	return nil
}

// gamecenterFeed maps the archive's feed name onto the API path segment.
func gamecenterFeed(feed string) string {
	if feed == "pbp" {
		return "play-by-play"
	}
	return feed
}

// fetch runs one paced NHL request with retries. Not-found is returned at
// once; anything else is retried with doubling backoff up to MaxAttempts.
func (r *runner) fetch(ctx context.Context, what string, do func() error) error {
	var err error
	for attempt := 1; attempt <= r.cfg.MaxAttempts; attempt++ {
		if attempt > 1 {
			wait := r.cfg.Backoff << (attempt - 2)
			slog.Warn("backfill retry", "what", what, "attempt", attempt, "wait", wait, "err", err)
			if err := r.d.Sleep(ctx, wait); err != nil {
				return err
			}
		}
		if err = r.pace(ctx); err != nil {
			return err
		}
		err = do()
		if err == nil || isNotFound(err) {
			return err
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
	}
	return err
}

// pace enforces Config.Interval between consecutive requests.
func (r *runner) pace(ctx context.Context) error {
	if r.cfg.Interval > 0 && !r.last.IsZero() {
		if wait := r.cfg.Interval - r.d.Now().Sub(r.last); wait > 0 {
			if err := r.d.Sleep(ctx, wait); err != nil {
				return err
			}
		}
	}
	r.last = r.d.Now()
	return nil
}

func isNotFound(err error) bool {
	var se *nhl.StatusError
	return errors.As(err, &se) && se.Code == 404
}
