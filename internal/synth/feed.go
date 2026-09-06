package synth

import (
	"context"
	"fmt"

	"hockeytrack/internal/nhl"
)

// Feed serves synthesized snapshots in order through the poller's Feed
// interface. Once the sequence is exhausted it repeats the last snapshot,
// because the poller polls until it observes a final state and the last
// snapshot is always FINAL.
type Feed struct {
	snaps []Snapshot
	i     int

	// Before runs immediately before each snapshot is served, with the
	// index of the snapshot about to be returned. Live-fire uses it to
	// pace the run against the wall clock; offline replay leaves it nil.
	Before func(ctx context.Context, s Snapshot, i int) error
}

func NewFeed(snaps []Snapshot) *Feed { return &Feed{snaps: snaps} }

// Len reports how many distinct snapshots the feed holds.
func (f *Feed) Len() int { return len(f.snaps) }

func (f *Feed) PlayByPlay(ctx context.Context, _ int64) (*nhl.PlayByPlay, []byte, error) {
	if len(f.snaps) == 0 {
		return nil, nil, fmt.Errorf("synth: feed has no snapshots")
	}
	i := f.i
	if i >= len(f.snaps) {
		i = len(f.snaps) - 1
	}
	s := f.snaps[i]
	if f.Before != nil {
		if err := f.Before(ctx, s, i); err != nil {
			return nil, nil, err
		}
	}
	f.i++
	return s.PBP, s.Raw, nil
}

// RawFeed and ShiftCharts are stubs: the archive holds these feeds too, but
// nothing in the event contract is derived from them, and serving a
// placeholder keeps a replay free of network calls.
func (f *Feed) RawFeed(_ context.Context, _ int64, feed string) ([]byte, error) {
	return []byte(fmt.Sprintf(`{"synthStub":%q}`, feed)), nil
}

func (f *Feed) ShiftCharts(_ context.Context, _ int64) ([]byte, error) {
	return []byte(`{"synthStub":"shifts"}`), nil
}
