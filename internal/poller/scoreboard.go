package poller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"hockeytrack/internal/events"
	"hockeytrack/internal/nhl"
)

// BuildClockEvent snapshots the feed's clock, period, shots and situation
// as observed at observedAt.
func BuildClockEvent(pbp *nhl.PlayByPlay, observedAt time.Time) events.ClockEvent {
	return events.ClockEvent{
		SchemaVersion:    events.SchemaVersion,
		GameID:           pbp.ID,
		GameState:        pbp.GameState,
		Period:           pbp.PeriodDescriptor.Number,
		PeriodType:       pbp.PeriodDescriptor.PeriodType,
		SecondsRemaining: pbp.Clock.SecondsRemaining,
		TimeRemaining:    pbp.Clock.TimeRemaining,
		Running:          pbp.Clock.Running,
		InIntermission:   pbp.Clock.InIntermission,
		SituationCode:    pbp.SituationCode,
		HomeTeam:         pbp.HomeTeam.Abbrev,
		AwayTeam:         pbp.AwayTeam.Abbrev,
		Score:            map[string]int{pbp.HomeTeam.Abbrev: pbp.HomeTeam.Score, pbp.AwayTeam.Abbrev: pbp.AwayTeam.Score},
		Shots:            map[string]int{pbp.HomeTeam.Abbrev: pbp.HomeTeam.SOG, pbp.AwayTeam.Abbrev: pbp.AwayTeam.SOG},
		ObservedAt:       observedAt.UTC(),
	}
}

// BuildRosterEvent lists every roster spot with its sweater number, sorted
// by team then number so equal rosters produce equal events.
func BuildRosterEvent(pbp *nhl.PlayByPlay) events.RosterEvent {
	team := func(id int64) string {
		if id == pbp.HomeTeam.ID {
			return pbp.HomeTeam.Abbrev
		}
		return pbp.AwayTeam.Abbrev
	}
	players := make([]events.RosterPlayer, 0, len(pbp.RosterSpots))
	for _, r := range pbp.RosterSpots {
		players = append(players, events.RosterPlayer{PlayerID: r.PlayerID, Team: team(r.TeamID), Number: r.SweaterNumber, Position: r.PositionCode})
	}
	sort.Slice(players, func(i, j int) bool {
		if players[i].Team != players[j].Team {
			return players[i].Team < players[j].Team
		}
		if players[i].Number != players[j].Number {
			return players[i].Number < players[j].Number
		}
		return players[i].PlayerID < players[j].PlayerID
	})
	return events.RosterEvent{
		SchemaVersion: events.SchemaVersion, GameID: pbp.ID,
		HomeTeam: pbp.HomeTeam.Abbrev, AwayTeam: pbp.AwayTeam.Abbrev, Players: players,
	}
}

// RosterHash identifies a roster so the poller publishes it only when it
// changes. Empty when the feed has no roster yet (pre-game).
func RosterHash(pbp *nhl.PlayByPlay) string {
	if len(pbp.RosterSpots) == 0 {
		return ""
	}
	keys := make([]string, 0, len(pbp.RosterSpots))
	for _, r := range pbp.RosterSpots {
		keys = append(keys, fmt.Sprintf("%d:%d:%d", r.TeamID, r.PlayerID, r.SweaterNumber))
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}
