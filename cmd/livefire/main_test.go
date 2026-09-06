package main

import (
	"testing"

	"hockeytrack/internal/events"
	"hockeytrack/internal/poller"
)

func TestReplayConfigZeroesPollIntervals(t *testing.T) {
	got := replayConfig()
	want := poller.DefaultConfig()

	if got.LiveInterval != 0 {
		t.Errorf("LiveInterval = %v, want 0", got.LiveInterval)
	}
	if got.PregameInterval != 0 {
		t.Errorf("PregameInterval = %v, want 0", got.PregameInterval)
	}
	if got.LeaseTTL != want.LeaseTTL {
		t.Errorf("LeaseTTL = %v, want %v (unchanged from DefaultConfig)", got.LeaseTTL, want.LeaseTTL)
	}
	if got.MaxChains != want.MaxChains {
		t.Errorf("MaxChains = %v, want %v (unchanged from DefaultConfig)", got.MaxChains, want.MaxChains)
	}
}

// The two tests below cover the lines the whole safety argument rests on.
// A final-review mutation pass showed both could be turned into no-ops with
// the suite still green, which meant the guarantees were asserted in prose
// and nowhere else.

func TestValidGameIDAcceptsOnlyTenDigitIDs(t *testing.T) {
	cases := []struct {
		id   int64
		want bool
		why  string
	}{
		{0, false, "unset"},
		{-2024021299, false, "negative"},
		{999999999, false, "nine digits, one below the range"},
		{1000000000, true, "lowest ten-digit id"},
		{2024021299, true, "a real game"},
		{1917020001, true, "the oldest game the API serves"},
		{9999999999, true, "highest ten-digit id"},
		{10000000000, false, "eleven digits: already a synthetic id"},
		{2024021299 + syntheticOffset, false, "a synthetic id must not be re-offset"},
	}
	for _, c := range cases {
		if got := validGameID(c.id); got != c.want {
			t.Errorf("validGameID(%d) = %v, want %v (%s)", c.id, got, c.want, c.why)
		}
	}
}

// Every accepted id, offset, must land clear of the real range, or a
// synthetic event could be mistaken for a real game.
func TestSyntheticIDsNeverCollideWithRealOnes(t *testing.T) {
	for _, id := range []int64{minGameID, 2024021299, maxGameID} {
		synthetic := id + syntheticOffset
		if synthetic <= maxGameID {
			t.Errorf("%d + offset = %d, which is still inside the real id range", id, synthetic)
		}
		if validGameID(synthetic) {
			t.Errorf("synthetic id %d reads back as a valid real game id", synthetic)
		}
		if synthetic-syntheticOffset != id {
			t.Errorf("offset is not reversible for %d", id)
		}
	}
}

func TestDefaultSourceIsSyntheticAndOnlyAsPollerChangesIt(t *testing.T) {
	if got := sourceFor(false); got != events.SourceSynthetic {
		t.Errorf("default source = %q, want %q — a synthetic run must match no notification rule", got, events.SourceSynthetic)
	}
	if got := sourceFor(true); got != events.Source {
		t.Errorf("-as-poller source = %q, want %q", got, events.Source)
	}
	if events.SourceSynthetic == events.Source {
		t.Error("the synthetic and real sources are equal; source isolation cannot work")
	}
}
