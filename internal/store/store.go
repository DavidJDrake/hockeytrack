// Package store persists game schedule records and poller bookkeeping.
package store

import (
	"context"
	"sync"
	"time"
)

type GameRecord struct {
	GameID            int64
	Season            int64
	GameDate          string // YYYY-MM-DD
	StartTimeUTC      time.Time
	HomeAbbrev        string
	AwayAbbrev        string
	Venue             string
	GameState         string
	ScheduleEntryName string
	LastPlaySortOrder int64
	SnapshotHashes    map[string]string
	ChainCount        int
	LeaseOwner        string
	LeaseExpiresAt    time.Time
	Done              bool
}

type PollerState struct {
	LastPlaySortOrder int64
	SnapshotHashes    map[string]string
	ChainCount        int
	GameState         string
	Done              bool
}

type GameStore interface {
	UpsertSchedule(ctx context.Context, rec GameRecord) error
	Get(ctx context.Context, gameID int64) (*GameRecord, error)
	ListByDate(ctx context.Context, date string) ([]GameRecord, error)
	AcquireLease(ctx context.Context, gameID int64, owner string, until time.Time) (bool, error)
	RenewLease(ctx context.Context, gameID int64, owner string, until time.Time) (bool, error)
	ReleaseLease(ctx context.Context, gameID int64, owner string) error
	UpdatePollerState(ctx context.Context, gameID int64, st PollerState) error
}

// FakeGameStore is an in-memory GameStore mirroring DynamoDB's conditional
// write semantics, for tests.
type FakeGameStore struct {
	mu    sync.Mutex
	games map[int64]*GameRecord
	Now   func() time.Time
}

func NewFakeGameStore() *FakeGameStore {
	return &FakeGameStore{games: map[int64]*GameRecord{}, Now: time.Now}
}

func (f *FakeGameStore) UpsertSchedule(_ context.Context, rec GameRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if existing, ok := f.games[rec.GameID]; ok {
		existing.Season = rec.Season
		existing.GameDate = rec.GameDate
		existing.StartTimeUTC = rec.StartTimeUTC
		existing.HomeAbbrev = rec.HomeAbbrev
		existing.AwayAbbrev = rec.AwayAbbrev
		existing.Venue = rec.Venue
		existing.GameState = rec.GameState
		existing.ScheduleEntryName = rec.ScheduleEntryName
		return nil
	}
	r := rec
	f.games[rec.GameID] = &r
	return nil
}

func (f *FakeGameStore) Get(_ context.Context, gameID int64) (*GameRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.games[gameID]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, nil
}

func (f *FakeGameStore) ListByDate(_ context.Context, date string) ([]GameRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []GameRecord
	for _, r := range f.games {
		if r.GameDate == date {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (f *FakeGameStore) AcquireLease(_ context.Context, gameID int64, owner string, until time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.games[gameID]
	if !ok {
		return false, nil
	}
	free := r.LeaseOwner == "" || !r.LeaseExpiresAt.After(f.Now()) || r.LeaseOwner == owner
	if !free {
		return false, nil
	}
	r.LeaseOwner = owner
	r.LeaseExpiresAt = until
	return true, nil
}

func (f *FakeGameStore) RenewLease(_ context.Context, gameID int64, owner string, until time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.games[gameID]
	if !ok || r.LeaseOwner != owner {
		return false, nil
	}
	r.LeaseExpiresAt = until
	return true, nil
}

func (f *FakeGameStore) ReleaseLease(_ context.Context, gameID int64, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.games[gameID]; ok && r.LeaseOwner == owner {
		r.LeaseExpiresAt = time.Time{}
	}
	return nil
}

func (f *FakeGameStore) UpdatePollerState(_ context.Context, gameID int64, st PollerState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.games[gameID]; ok {
		r.LastPlaySortOrder = st.LastPlaySortOrder
		r.SnapshotHashes = st.SnapshotHashes
		r.ChainCount = st.ChainCount
		r.GameState = st.GameState
		r.Done = st.Done
	}
	return nil
}
