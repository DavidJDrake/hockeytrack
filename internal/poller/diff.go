// Package poller contains the runtime-agnostic game polling loop and the
// play-by-play diff logic that turns snapshots into discrete events.
package poller

import (
	"sort"

	"hockeytrack/internal/events"
	"hockeytrack/internal/nhl"
)

func NewPlays(plays []nhl.Play, lastSortOrder int64) []nhl.Play {
	var out []nhl.Play
	for _, p := range plays {
		if p.SortOrder > lastSortOrder {
			out = append(out, p)
		}
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

func BuildPlayEvent(pbp *nhl.PlayByPlay, p nhl.Play, score map[string]int) events.PlayEvent {
	acting := teamAbbrev(pbp, p.ParsedDetails().EventOwnerTeamID)
	e := events.PlayEvent{
		SchemaVersion: events.SchemaVersion,
		GameID:        pbp.ID,
		Seq:           p.SortOrder,
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
