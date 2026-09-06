package main

import (
	"testing"

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
