package poller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"time"

	"hockeytrack/internal/events"
	"hockeytrack/internal/nhl"
	"hockeytrack/internal/store"
)

type Feed interface {
	PlayByPlay(ctx context.Context, gameID int64) (*nhl.PlayByPlay, []byte, error)
	RawFeed(ctx context.Context, gameID int64, feed string) ([]byte, error)
	ShiftCharts(ctx context.Context, gameID int64) ([]byte, error)
}

type Config struct {
	LiveInterval    time.Duration
	PregameInterval time.Duration
	LeaseTTL        time.Duration
	MaxChains       int
}

func DefaultConfig() Config {
	return Config{LiveInterval: 5 * time.Second, PregameInterval: 30 * time.Second, LeaseTTL: 60 * time.Second, MaxChains: 30}
}

type Deps struct {
	Feed    Feed
	Store   store.GameStore
	Archive store.Archive
	Pub     events.Publisher
	Now     func() time.Time
	Sleep   func(context.Context, time.Duration) error
}

type Outcome int

const (
	OutcomeFinal Outcome = iota
	OutcomeHandOff
	OutcomeLeaseHeld
	OutcomeAlreadyDone
	OutcomeStale
	OutcomeNotScheduled
)

func hashOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func Run(ctx context.Context, d Deps, cfg Config, gameID int64, owner string, shouldHandOff func() bool) (Outcome, error) {
	rec, err := d.Store.Get(ctx, gameID)
	if err != nil {
		return 0, err
	}
	if rec == nil {
		return OutcomeNotScheduled, nil
	}
	if rec.Done || IsFinalState(rec.GameState) {
		return OutcomeAlreadyDone, nil
	}
	ok, err := d.Store.AcquireLease(ctx, gameID, owner, d.Now().Add(cfg.LeaseTTL))
	if err != nil {
		return 0, err
	}
	if !ok {
		return OutcomeLeaseHeld, nil
	}

	chain := rec.ChainCount + 1
	state := store.PollerState{
		LastPlaySortOrder: rec.LastPlaySortOrder,
		SnapshotHashes:    map[string]string{},
		ChainCount:        chain,
		GameState:         rec.GameState,
	}
	for k, v := range rec.SnapshotHashes {
		state.SnapshotHashes[k] = v
	}
	if chain > cfg.MaxChains {
		_ = d.Pub.Publish(ctx, events.DTAlert, events.AlertEvent{
			SchemaVersion: events.SchemaVersion, GameID: gameID, Reason: "max chain links exceeded",
		})
		state.Done = true
		_ = d.Store.UpdatePollerState(ctx, gameID, state)
		_ = d.Store.ReleaseLease(ctx, gameID, owner)
		return OutcomeStale, nil
	}
	if err := d.Store.UpdatePollerState(ctx, gameID, state); err != nil {
		return 0, err
	}

	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if shouldHandOff() {
			_ = d.Store.UpdatePollerState(ctx, gameID, state)
			_ = d.Store.ReleaseLease(ctx, gameID, owner)
			return OutcomeHandOff, nil
		}

		pbp, raw, err := d.Feed.PlayByPlay(ctx, gameID)
		if err != nil {
			slog.Warn("play-by-play fetch failed", "gameId", gameID, "err", err)
			if err := d.Sleep(ctx, min(cfg.LiveInterval*2, 30*time.Second)); err != nil {
				return 0, err
			}
			continue
		}

		if h := hashOf(raw); h != state.SnapshotHashes["pbp"] {
			key := store.SnapshotKey(pbp.Season, pbp.GameDate, gameID, "pbp", d.Now())
			if err := d.Archive.Put(ctx, key, raw); err != nil {
				slog.Warn("pbp snapshot write failed", "gameId", gameID, "err", err)
			} else {
				state.SnapshotHashes["pbp"] = h
			}
		}
		if box, err := d.Feed.RawFeed(ctx, gameID, "boxscore"); err != nil {
			slog.Warn("boxscore fetch failed", "gameId", gameID, "err", err)
		} else if h := hashOf(box); h != state.SnapshotHashes["boxscore"] {
			key := store.SnapshotKey(pbp.Season, pbp.GameDate, gameID, "boxscore", d.Now())
			if err := d.Archive.Put(ctx, key, box); err != nil {
				slog.Warn("boxscore snapshot write failed", "gameId", gameID, "err", err)
			} else {
				state.SnapshotHashes["boxscore"] = h
			}
		}

		score := map[string]int{pbp.HomeTeam.Abbrev: pbp.HomeTeam.Score, pbp.AwayTeam.Abbrev: pbp.AwayTeam.Score}
		if pbp.GameState != state.GameState {
			err := d.Pub.Publish(ctx, events.DTStatus, events.StatusEvent{
				SchemaVersion: events.SchemaVersion, GameID: gameID,
				PrevState: state.GameState, GameState: pbp.GameState, Score: score,
			})
			if err == nil {
				state.GameState = pbp.GameState
			}
		}

		running := score
		for _, p := range NewPlays(pbp.Plays, state.LastPlaySortOrder) {
			running = RunningScore(pbp, p, running)
			if err := d.Pub.Publish(ctx, events.DTPlay, BuildPlayEvent(pbp, p, running)); err != nil {
				slog.Warn("play publish failed; will retry next cycle", "gameId", gameID, "seq", p.SortOrder, "err", err)
				break // do not advance the mark past a failed publish
			}
			state.LastPlaySortOrder = p.SortOrder
		}

		if err := d.Store.UpdatePollerState(ctx, gameID, state); err != nil {
			return 0, err
		}
		renewed, err := d.Store.RenewLease(ctx, gameID, owner, d.Now().Add(cfg.LeaseTTL))
		if err != nil {
			return 0, err
		}
		if !renewed {
			return OutcomeLeaseHeld, nil
		}

		if IsFinalState(pbp.GameState) {
			archiveFinal(ctx, d, pbp, gameID, raw)
			_ = d.Pub.Publish(ctx, events.DTFinal, events.FinalEvent{
				SchemaVersion: events.SchemaVersion, GameID: gameID,
				HomeTeam: pbp.HomeTeam.Abbrev, AwayTeam: pbp.AwayTeam.Abbrev,
				Score:    score,
				S3Prefix: store.GamePrefix(pbp.Season, pbp.GameDate, gameID),
			})
			state.Done = true
			if err := d.Store.UpdatePollerState(ctx, gameID, state); err != nil {
				return 0, err
			}
			_ = d.Store.ReleaseLease(ctx, gameID, owner)
			return OutcomeFinal, nil
		}

		interval := cfg.PregameInterval
		if IsLiveState(pbp.GameState) {
			interval = cfg.LiveInterval
		}
		if err := d.Sleep(ctx, interval); err != nil {
			return 0, err
		}
	}
}

// archiveFinal writes the end-of-game sweep; individual failures are logged,
// never fatal — the raw snapshots already preserve the game.
func archiveFinal(ctx context.Context, d Deps, pbp *nhl.PlayByPlay, gameID int64, finalPBP []byte) {
	put := func(feed string, body []byte, err error) {
		if err != nil {
			slog.Warn("final sweep fetch failed", "gameId", gameID, "feed", feed, "err", err)
			return
		}
		key := store.FinalKey(pbp.Season, pbp.GameDate, gameID, feed)
		if err := d.Archive.Put(ctx, key, body); err != nil {
			slog.Warn("final sweep write failed", "gameId", gameID, "feed", feed, "err", err)
		}
	}
	put("pbp", finalPBP, nil)
	box, err := d.Feed.RawFeed(ctx, gameID, "boxscore")
	put("boxscore", box, err)
	landing, err := d.Feed.RawFeed(ctx, gameID, "landing")
	put("landing", landing, err)
	shifts, err := d.Feed.ShiftCharts(ctx, gameID)
	put("shifts", shifts, err)
}
