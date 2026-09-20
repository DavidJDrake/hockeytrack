// Package poller contains the runtime-agnostic game polling loop and the
// play-by-play diff logic that turns snapshots into discrete events.
package poller

import (
	"sort"

	"hockeytrack/internal/events"
	"hockeytrack/internal/nhl"
)

// ProvisionalSortOrder is where the NHL parks plays it has not placed yet. At
// the start of a period it appends the period-start and the opening faceoff
// with sort orders in the 9000s and renumbers them to their real positions
// within a minute or so (seen in this project's own archive: 9003 and 9004
// becoming 283 and 284 forty-one seconds later). Real sort orders run to a
// few thousand at most.
const ProvisionalSortOrder = 9000

// NewPlays returns the plays not yet sent, in sort order.
//
// "Not yet sent" is decided by eventId, the one thing about a play the NHL
// does not change. It used to be decided by a highest-sort-order mark, and
// one poll that caught a provisional 9004 set that mark above every real
// play for the rest of the game: every goal and penalty after it was skipped
// as already sent. A set has no such single point of failure, and it also
// picks up plays the NHL inserts late, below numbers already sent.
//
// A play still carrying a provisional number is held back until it has its
// real one, so that seq means something to whoever receives it -- except at
// the final, when whatever is left goes regardless: nothing is lost for the
// sake of a tidy sequence.
func NewPlays(plays []nhl.Play, sent map[int64]bool, final bool) []nhl.Play {
	var out []nhl.Play
	for _, p := range plays {
		if sent[p.EventID] {
			continue
		}
		if p.SortOrder >= ProvisionalSortOrder && !final {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SortOrder < out[j].SortOrder })
	return out
}

// RunningScore returns the score after this play. Goal plays carry
// authoritative awayScore/homeScore in details; other plays keep prev.
func RunningScore(pbp *nhl.PlayByPlay, p nhl.Play, prev map[string]int) map[string]int {
	d := p.ParsedDetails()
	if d.AwayScore == nil || d.HomeScore == nil {
		return prev
	}
	return map[string]int{
		pbp.AwayTeam.Abbrev: *d.AwayScore,
		pbp.HomeTeam.Abbrev: *d.HomeScore,
	}
}

func teamAbbrev(pbp *nhl.PlayByPlay, teamID int64) string {
	switch teamID {
	case pbp.HomeTeam.ID:
		return pbp.HomeTeam.Abbrev
	case pbp.AwayTeam.ID:
		return pbp.AwayTeam.Abbrev
	}
	return ""
}

// SeedSent works out what a game already under way has sent, from the mark
// the old code kept. Everything at or below the mark was either sent or --
// if the mark was poisoned -- lost; sending it now would replay a game's
// goals as news. Returns nil for a game that has sent nothing.
func SeedSent(plays []nhl.Play, lastSortOrder int64) map[int64]bool {
	if lastSortOrder <= 0 {
		return nil
	}
	sent := map[int64]bool{}
	for _, p := range plays {
		if p.SortOrder <= lastSortOrder {
			sent[p.EventID] = true
		}
	}
	return sent
}

func BuildPlayEvent(pbp *nhl.PlayByPlay, p nhl.Play, score map[string]int) events.PlayEvent {
	acting := teamAbbrev(pbp, p.ParsedDetails().EventOwnerTeamID)
	e := events.PlayEvent{
		SchemaVersion: events.SchemaVersion,
		GameID:        pbp.ID,
		Seq:           p.SortOrder,
		EventID:       p.EventID,
		PlayType:      p.TypeDescKey,
		HomeTeam:      pbp.HomeTeam.Abbrev,
		AwayTeam:      pbp.AwayTeam.Abbrev,
		ActingTeam:    acting,
		Period:        p.PeriodDescriptor.Number,
		TimeInPeriod:  p.TimeInPeriod,
		Score:         score,
		Raw:           p.Raw,
	}
	if p.TypeDescKey == "goal" {
		e.ScoringTeam = acting
	}
	return e
}

func IsFinalState(s string) bool { return s == "FINAL" || s == "OFF" }

func IsLiveState(s string) bool { return s == "LIVE" || s == "CRIT" }
