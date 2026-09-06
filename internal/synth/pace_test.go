package synth

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestPacerSleepsProportionalToGameTime(t *testing.T) {
	s, err := Snapshots(fixture(t, "2025020001"), Options{Interval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	var slept []time.Duration
	p := Pacer(60, time.Minute, func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	})
	for i := range s {
		if err := p(context.Background(), s[i], i); err != nil {
			t.Fatal(err)
		}
	}
	if len(slept) != len(s) {
		t.Fatalf("sleeps = %d, want %d", len(slept), len(s))
	}
	if slept[0] != 0 {
		t.Errorf("first sleep = %v, want 0", slept[0])
	}
	var total time.Duration
	for _, d := range slept {
		if d < 0 {
			t.Fatalf("negative sleep %v", d)
		}
		total += d
	}
	if total == 0 {
		t.Error("paced run never slept")
	}
	// A full game at 60x should take on the order of minutes, not hours.
	if total > 30*time.Minute {
		t.Errorf("total pacing = %v, implausible at 60x", total)
	}
}

func TestPacerCapsLongGaps(t *testing.T) {
	s, err := Snapshots(fixture(t, "2025020001"), Options{Interval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	var maxSleep time.Duration
	p := Pacer(1, 2*time.Second, func(_ context.Context, d time.Duration) error {
		if d > maxSleep {
			maxSleep = d
		}
		return nil
	})
	for i := range s {
		if err := p(context.Background(), s[i], i); err != nil {
			t.Fatal(err)
		}
	}
	if maxSleep > 2*time.Second {
		t.Errorf("max sleep = %v, want the 2s cap", maxSleep)
	}
}

// A NaN speed would make every computed wait NaN, which silently degrades to
// an unpaced run. The guard is written !(speed > 0) precisely so NaN is
// caught; a mutation pass showed nothing tested that.
func TestPacerRejectsNaNAndNonPositiveSpeeds(t *testing.T) {
	s, err := Snapshots(fixture(t, "1917020001"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, speed := range []float64{math.NaN(), 0, -1} {
		var slept []time.Duration
		p := Pacer(speed, time.Minute, func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		})
		for i := range s {
			if err := p(context.Background(), s[i], i); err != nil {
				t.Fatal(err)
			}
		}
		for _, d := range slept {
			if d < 0 || d != d { // d != d is true only for NaN
				t.Fatalf("speed %v produced an invalid wait %v; the guard let it through", speed, d)
			}
		}
	}
}
