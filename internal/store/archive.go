package store

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

type Archive interface {
	Put(ctx context.Context, key string, body []byte) error
}

// Lister is an Archive that can also enumerate what it already holds, which
// lets a long job resume without re-fetching.
type Lister interface {
	Archive
	// List returns every key under prefix, in no particular order.
	List(ctx context.Context, prefix string) ([]string, error)
}

func GamePrefix(season int64, gameDate string, gameID int64) string {
	return fmt.Sprintf("raw/%d/%s/%d/", season, gameDate, gameID)
}

func SnapshotKey(season int64, gameDate string, gameID int64, feed string, ts time.Time) string {
	return fmt.Sprintf("%s%s/%s.json", GamePrefix(season, gameDate, gameID), feed, ts.UTC().Format("20060102T150405Z"))
}

func FinalKey(season int64, gameDate string, gameID int64, feed string) string {
	return fmt.Sprintf("%sfinal/%s.json", GamePrefix(season, gameDate, gameID), feed)
}

func ScheduleKey(date string) string {
	return fmt.Sprintf("raw/schedule/%s.json", date)
}

// StandingsKey is the daily standings snapshot, keyed by the run date (not
// the standings' own date, which is fixed all off-season).
func StandingsKey(date string) string {
	return fmt.Sprintf("raw/standings/%s.json", date)
}

type FakeArchive struct {
	mu      sync.Mutex
	Objects map[string][]byte
}

func NewFakeArchive() *FakeArchive {
	return &FakeArchive{Objects: map[string][]byte{}}
}

func (f *FakeArchive) Put(_ context.Context, key string, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Objects[key] = append([]byte(nil), body...)
	return nil
}

func (f *FakeArchive) List(_ context.Context, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for k := range f.Objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	return keys, nil
}
