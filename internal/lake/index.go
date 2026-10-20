package lake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SeasonsPrefix is where the index keeps everything; a rollup sits directly
// under it and the per-game summaries one level down.
const SeasonsPrefix = "v1/seasons/"

// SummaryKey is the index key of one game's summary.
func SummaryKey(season, gameID int64) string {
	return fmt.Sprintf("%s%d/games/%d.json", SeasonsPrefix, season, gameID)
}

// SummariesPrefix is the index prefix holding a season's game summaries.
func SummariesPrefix(season int64) string {
	return fmt.Sprintf("%s%d/games/", SeasonsPrefix, season)
}

// RollupKey is the index key of a season's rollup.
func RollupKey(season int64) string {
	return fmt.Sprintf("%s%d.json", SeasonsPrefix, season)
}

// GamePrefix is the archive prefix of one game, as store.GamePrefix lays
// it out. Duplicated here so the package does not import the writer.
func GamePrefix(season int64, date string, gameID int64) string {
	return fmt.Sprintf("raw/%d/%s/%d/", season, date, gameID)
}

// ParseGamePrefix reads season, date and game id back out of a game prefix
// ("raw/20252026/2025-10-07/2025020001/"), tolerating a missing trailing
// slash and a trailing object key.
func ParseGamePrefix(prefix string) (season int64, date string, gameID int64, ok bool) {
	parts := strings.Split(strings.Trim(prefix, "/"), "/")
	if len(parts) < 4 || parts[0] != "raw" {
		return 0, "", 0, false
	}
	season, err1 := strconv.ParseInt(parts[1], 10, 64)
	gameID, err2 := strconv.ParseInt(parts[3], 10, 64)
	if err1 != nil || err2 != nil || len(parts[2]) != 10 {
		return 0, "", 0, false
	}
	return season, parts[2], gameID, true
}

// ErrNotFinal is returned when a game asked to be indexed has no final feed
// yet; the daily sync will pick it up once the sweep lands.
var ErrNotFinal = errors.New("game has no final feed yet")

// Indexer derives summaries and rollups from the raw archive into the
// index store.
type Indexer struct {
	Raw   Source
	Index Store
	Now   func() time.Time
	// Concurrency bounds parallel reads of summaries and feeds; 16 if zero.
	Concurrency int
}

func (ix *Indexer) workers() int {
	if ix.Concurrency <= 0 {
		return 16
	}
	return ix.Concurrency
}

func (ix *Indexer) now() time.Time {
	if ix.Now == nil {
		return time.Now()
	}
	return ix.Now()
}

// BuildGame summarizes the final feeds under a game prefix and writes the
// summary. A game that has not finished is not written.
func (ix *Indexer) BuildGame(ctx context.Context, prefix string) (*GameSummary, error) {
	season, _, gameID, ok := ParseGamePrefix(prefix)
	if !ok {
		return nil, fmt.Errorf("not a game prefix: %q", prefix)
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	feeds, err := Locate(ctx, ix.Raw, prefix)
	if err != nil {
		return nil, err
	}
	if !feeds.Final {
		return nil, ErrNotFinal
	}
	bodies, err := Load(ctx, ix.Raw, feeds)
	if err != nil {
		return nil, err
	}
	s, err := Summarize(bodies)
	if err != nil {
		return nil, fmt.Errorf("summarize %s: %w", prefix, err)
	}
	if s.Game.ID != gameID {
		return nil, fmt.Errorf("prefix %s holds game %d", prefix, s.Game.ID)
	}
	body, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	if err := ix.Index.Put(ctx, SummaryKey(season, gameID), body); err != nil {
		return nil, fmt.Errorf("put summary: %w", err)
	}
	return s, nil
}

// BuildSeason reads every summary under the season and writes the rollup.
// A summary that cannot be read or parsed is logged and left out, so one
// bad object cannot block the season.
func (ix *Indexer) BuildSeason(ctx context.Context, season int64) (*SeasonRollup, error) {
	keys, err := ix.Index.List(ctx, SummariesPrefix(season))
	if err != nil {
		return nil, fmt.Errorf("list summaries: %w", err)
	}
	sort.Strings(keys)
	summaries := make([]GameSummary, len(keys))
	okay := make([]bool, len(keys))
	err = ix.each(ctx, len(keys), func(i int) error {
		body, err := ix.Index.Get(ctx, keys[i])
		if err != nil {
			slog.Warn("summary unreadable; left out of rollup", "key", keys[i], "err", err)
			return nil
		}
		var s GameSummary
		if err := json.Unmarshal(body, &s); err != nil || s.Game.ID == 0 {
			slog.Warn("summary unparsable; left out of rollup", "key", keys[i], "err", err)
			return nil
		}
		summaries[i], okay[i] = s, true
		return nil
	})
	if err != nil {
		return nil, err
	}
	var games []GameSummary
	for i, s := range summaries {
		if okay[i] {
			games = append(games, s)
		}
	}
	r := Rollup(season, games, ix.now())
	body, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	if err := ix.Index.Put(ctx, RollupKey(season), body); err != nil {
		return nil, fmt.Errorf("put rollup: %w", err)
	}
	return r, nil
}

// SyncStats summarizes a SyncSeason run.
type SyncStats struct {
	Final   int // finished games in the archive
	Built   int // summaries written this run
	Skipped int // summaries that already existed
	Failed  int // games whose feeds could not be summarized
}

func (s SyncStats) String() string {
	return fmt.Sprintf("final=%d built=%d skipped=%d failed=%d", s.Final, s.Built, s.Skipped, s.Failed)
}

// ErrGamesFailed is returned by SyncSeason when the rollup was written but
// some games could not be summarized.
var ErrGamesFailed = errors.New("some games could not be summarized")

// SyncSeason builds a summary for every finished game in the archive that
// lacks one, then rebuilds the rollup. It lists the season's whole prefix,
// live snapshots included, which is the cost of not needing a manifest.
func (ix *Indexer) SyncSeason(ctx context.Context, season int64) (SyncStats, error) {
	var st SyncStats
	rawKeys, err := ix.Raw.List(ctx, fmt.Sprintf("raw/%d/", season))
	if err != nil {
		return st, fmt.Errorf("list season: %w", err)
	}
	have, err := ix.Index.List(ctx, SummariesPrefix(season))
	if err != nil {
		return st, fmt.Errorf("list summaries: %w", err)
	}
	indexed := map[string]bool{}
	for _, k := range have {
		indexed[k] = true
	}
	var todo []string
	for _, k := range rawKeys {
		prefix, ok := strings.CutSuffix(k, "final/pbp.json")
		if !ok {
			continue
		}
		st.Final++
		if _, _, gameID, ok := ParseGamePrefix(prefix); ok && indexed[SummaryKey(season, gameID)] {
			st.Skipped++
			continue
		}
		todo = append(todo, prefix)
	}
	sort.Strings(todo)
	var mu sync.Mutex
	err = ix.each(ctx, len(todo), func(i int) error {
		_, err := ix.BuildGame(ctx, todo[i])
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			slog.Warn("game not summarized", "prefix", todo[i], "err", err)
			st.Failed++
			return nil
		}
		st.Built++
		return nil
	})
	if err != nil {
		return st, err
	}
	if _, err := ix.BuildSeason(ctx, season); err != nil {
		return st, err
	}
	if st.Failed > 0 {
		return st, ErrGamesFailed
	}
	return st, nil
}

// Seasons lists the seasons that have a rollup, newest first.
func (ix *Indexer) Seasons(ctx context.Context) ([]int64, error) {
	return ListSeasons(ctx, ix.Index)
}

// ListSeasons lists the seasons with a rollup in an index, newest first.
func ListSeasons(ctx context.Context, index Source) ([]int64, error) {
	keys, err := index.List(ctx, SeasonsPrefix)
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, k := range keys {
		rest := strings.TrimPrefix(k, SeasonsPrefix)
		id, ok := strings.CutSuffix(rest, ".json")
		if !ok || strings.Contains(id, "/") {
			continue
		}
		if n, err := strconv.ParseInt(id, 10, 64); err == nil {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] > out[j] })
	return out, nil
}

// each runs fn(i) for i in [0, n) on a bounded pool, stopping at the first
// error or cancellation.
func (ix *Indexer) each(ctx context.Context, n int, fn func(i int) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, ix.workers())
	errc := make(chan error, 1)
	var wg sync.WaitGroup
loop:
	for i := 0; i < n; i++ {
		select {
		case <-ctx.Done():
			break loop
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(i); err != nil {
				select {
				case errc <- err:
				default:
				}
				cancel()
			}
		}(i)
	}
	wg.Wait()
	select {
	case err := <-errc:
		return err
	default:
	}
	return ctx.Err()
}
