package synth

import (
	"context"
	"time"

	"hockeytrack/internal/nhl"
)

// Pacer returns a Feed.Before hook that spaces snapshots out the way the
// game itself was spaced, divided by speed. An intermission or a period
// break can be a long gap in game time, so each wait is capped: the point
// is to reproduce the rhythm consumers see, not to sit idle.
//
// speed is a multiplier over game time (60 means a minute of hockey per
// second). sleep is injected so tests observe the waits instead of taking
// them.
func Pacer(speed float64, maxWait time.Duration, sleep func(context.Context, time.Duration) error) func(context.Context, Snapshot, int) error {
	// Written as !(speed > 0) rather than speed <= 0 so NaN is also caught:
	// every comparison with NaN is false, so speed <= 0 would let NaN pass
	// through, and a NaN speed makes every computed wait NaN, which
	// degrades the whole run to unpaced.
	if !(speed > 0) {
		speed = 1
	}
	prev := -1
	var prevElapsed int
	return func(ctx context.Context, s Snapshot, i int) error {
		elapsed := gameElapsed(s.PBP)
		if prev < 0 {
			prev, prevElapsed = i, elapsed
			return sleep(ctx, 0)
		}
		delta := elapsed - prevElapsed
		prev, prevElapsed = i, elapsed
		if delta <= 0 {
			return sleep(ctx, 0)
		}
		wait := time.Duration(float64(delta) * float64(time.Second) / speed)
		if maxWait > 0 && wait > maxWait {
			wait = maxWait
		}
		return sleep(ctx, wait)
	}
}

// gameElapsed is total elapsed game time in seconds, counting each finished
// period as a full twenty minutes. It only has to increase monotonically,
// which is all the pacer needs; overtime lengths do not have to be exact.
func gameElapsed(p *nhl.PlayByPlay) int {
	period := p.PeriodDescriptor.Number
	if period < 1 {
		period = 1
	}
	inPeriod := 1200 - p.Clock.SecondsRemaining
	if inPeriod < 0 {
		inPeriod = 0
	}
	return (period-1)*1200 + inPeriod
}
