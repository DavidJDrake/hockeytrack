package synth

import (
	"context"
	"testing"
)

func TestFeedServesSnapshotsThenRepeatsTheLast(t *testing.T) {
	s, err := Snapshots(fixture(t, "1917020001"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	f := NewFeed(s)
	ctx := context.Background()
	seen := 0
	for i := 0; i < len(s)+3; i++ {
		p, raw, err := f.PlayByPlay(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) == 0 || p == nil {
			t.Fatalf("call %d returned nothing", i)
		}
		seen++
	}
	if seen != len(s)+3 {
		t.Errorf("served %d, want %d", seen, len(s)+3)
	}
	// Past the end it must keep reporting the finished game, or the poller
	// would never observe a final state.
	p, _, _ := f.PlayByPlay(ctx, 0)
	if p.GameState != "FINAL" {
		t.Errorf("exhausted feed state = %q, want FINAL", p.GameState)
	}
}

func TestFeedCallsBeforeHookOncePerSnapshot(t *testing.T) {
	s, err := Snapshots(fixture(t, "1917020001"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	f := NewFeed(s)
	calls := 0
	f.Before = func(context.Context, Snapshot, int) error { calls++; return nil }
	for i := 0; i < 5; i++ {
		if _, _, err := f.PlayByPlay(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 5 {
		t.Errorf("Before called %d times, want 5", calls)
	}
}

func TestFeedStubsTheOtherEndpoints(t *testing.T) {
	f := NewFeed([]Snapshot{{Raw: []byte(`{}`)}})
	b, err := f.RawFeed(context.Background(), 1, "boxscore")
	if err != nil || len(b) == 0 {
		t.Errorf("RawFeed = %q, %v", b, err)
	}
	if b, err := f.ShiftCharts(context.Background(), 1); err != nil || len(b) == 0 {
		t.Errorf("ShiftCharts = %q, %v", b, err)
	}
}
