package synth

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Source is the read side of the raw archive. store.S3Archive and
// store.FakeArchive both satisfy it.
type Source interface {
	List(ctx context.Context, prefix string) ([]string, error)
	Get(ctx context.Context, key string) ([]byte, error)
}

// SeasonOf derives the season id from a game id: the first four digits are
// the season's opening year, and a season id is that year followed by the
// next. 2024021299 belongs to season 20242025.
func SeasonOf(gameID int64) int64 {
	year := gameID / 1000000
	return year*10000 + year + 1
}

// FinalPBP fetches a game's archived final play-by-play. The archive key
// carries the game date, which the game id does not, so the game is located
// by listing its season prefix. When cacheDir is non-empty the body is read
// from and written to <cacheDir>/<gameID>.json, which makes repeat runs and
// offline work possible.
func FinalPBP(ctx context.Context, src Source, gameID int64, cacheDir string) ([]byte, error) {
	if cacheDir != "" {
		if b, err := os.ReadFile(cachePath(cacheDir, gameID)); err == nil {
			return b, nil
		}
	}
	season := SeasonOf(gameID)
	keys, err := src.List(ctx, fmt.Sprintf("raw/%d/", season))
	if err != nil {
		return nil, fmt.Errorf("list season %d: %w", season, err)
	}
	wantID := fmt.Sprint(gameID)
	key := ""
	for _, k := range keys {
		// Match the game id as its own path segment, not a suffix: a
		// suffix match is only safe because every real game id happens to
		// be exactly ten digits.
		segs := strings.Split(k, "/")
		if len(segs) >= 3 &&
			segs[len(segs)-1] == "pbp.json" &&
			segs[len(segs)-2] == "final" &&
			segs[len(segs)-3] == wantID {
			key = k
			break
		}
	}
	if key == "" {
		return nil, fmt.Errorf("game %d has no final/pbp.json under raw/%d/ (%d keys listed)", gameID, season, len(keys))
	}
	body, err := src.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	if cacheDir != "" {
		if err := os.MkdirAll(cacheDir, 0o755); err == nil {
			// A cache write failure is not worth failing the run over.
			writeCacheAtomic(cacheDir, gameID, body)
		}
	}
	return body, nil
}

// writeCacheAtomic writes body to the game's cache file by writing a temp
// file and renaming it into place. os.WriteFile is not atomic: a process
// killed mid-write would leave a truncated file that the read path (which
// only checks err == nil) would then serve as a valid cache hit forever
// after. A failure here is silently swallowed, matching the caller's
// existing policy that a cache-write failure must never fail the run.
//
// os.CreateTemp makes the temp file 0600; the rename carries that mode
// through to the cache file, so cached finals went from 0644 to 0600 when
// this was introduced. That is kept deliberately — a private cache is the
// better default, not an accident of the temp-file API.
func writeCacheAtomic(cacheDir string, gameID int64, body []byte) {
	tmp, err := os.CreateTemp(cacheDir, ".final-*.json")
	if err != nil {
		return
	}
	_, werr := tmp.Write(body)
	cerr := tmp.Close()
	if werr == nil && cerr == nil {
		// Rename is atomic within a directory, so a reader sees either the
		// whole file or no file.
		if rerr := os.Rename(tmp.Name(), cachePath(cacheDir, gameID)); rerr == nil {
			return
		}
	}
	// Either the write/close failed or the rename did: either way the temp
	// file must not linger, or every failed write leaves another
	// .final-*.json in the cache directory forever.
	_ = os.Remove(tmp.Name())
}

func cachePath(dir string, gameID int64) string {
	return filepath.Join(dir, fmt.Sprintf("%d.json", gameID))
}

// DefaultCacheDir is where replays keep downloaded finals.
func DefaultCacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "hockeytrack", "finals")
}
