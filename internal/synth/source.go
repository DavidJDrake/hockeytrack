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
	want := fmt.Sprintf("/%d/final/pbp.json", gameID)
	key := ""
	for _, k := range keys {
		if strings.HasSuffix(k, want) {
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
			_ = os.WriteFile(cachePath(cacheDir, gameID), body, 0o644)
		}
	}
	return body, nil
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
