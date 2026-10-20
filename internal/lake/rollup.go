package lake

import (
	"sort"
	"time"
)

// SeasonOf derives the season id from a game id: the first four digits are
// the season's opening year. 2024021299 belongs to 20242025.
func SeasonOf(gameID int64) int64 {
	year := gameID / 1000000
	return year*10000 + year + 1
}

// CurrentSeason is the season a date belongs to. A season opens in the
// autumn, so September onward is the new one and anything earlier in the
// year is the season that opened the previous autumn.
func CurrentSeason(now time.Time) int64 {
	y := int64(now.UTC().Year())
	if now.UTC().Month() < time.September {
		y--
	}
	return y*10000 + y + 1
}

type playerKey struct {
	id   int64
	team string
}

// Rollup sums a season's game summaries. Only final games count toward
// totals; a live game appears in the game list and nowhere else. A later
// summary for the same game id replaces an earlier one.
func Rollup(season int64, games []GameSummary, now time.Time) *SeasonRollup {
	byID := map[int64]GameSummary{}
	for _, g := range games {
		byID[g.Game.ID] = g
	}
	r := &SeasonRollup{Version: Version, Season: season, GeneratedAt: now.UTC(),
		Games: []GameRow{}, Skaters: []SkaterSeason{}, Goalies: []GoalieSeason{}, Teams: []TeamSeason{}}
	skaters := map[playerKey]*SkaterSeason{}
	goalies := map[playerKey]*GoalieSeason{}
	teams := map[string]*TeamSeason{}

	for _, g := range byID {
		r.Games = append(r.Games, g.Game)
		if !g.Game.Final() {
			continue
		}
		for _, line := range g.Skaters {
			k := playerKey{line.PlayerID, line.Team}
			s, ok := skaters[k]
			if !ok {
				s = &SkaterSeason{PlayerID: line.PlayerID, Team: line.Team}
				skaters[k] = s
			}
			s.GP++
			s.add(line.SkaterStats)
			if line.Name != "" {
				s.Name = line.Name
			}
			if line.Number != 0 {
				s.Number = line.Number
			}
			if line.Position != "" {
				s.Position = line.Position
			}
		}
		for _, line := range g.Goalies {
			// The boxscore lists the backup who never left the bench; a
			// game played needs time on ice, a shot faced or a decision.
			if line.TOISeconds == 0 && line.ShotsAgainst == 0 && line.Decision == "" && !line.Starter {
				continue
			}
			k := playerKey{line.PlayerID, line.Team}
			gl, ok := goalies[k]
			if !ok {
				gl = &GoalieSeason{PlayerID: line.PlayerID, Team: line.Team}
				goalies[k] = gl
			}
			gl.GP++
			if line.Starter {
				gl.Starts++
			}
			switch line.Decision {
			case "W":
				gl.Wins++
			case "L":
				gl.Losses++
			case "O":
				gl.OTLosses++
			}
			if line.Shutout {
				gl.Shutouts++
			}
			gl.ShotsAgainst += line.ShotsAgainst
			gl.Saves += line.Saves
			gl.GoalsAgainst += line.GoalsAgainst
			gl.TOISeconds += line.TOISeconds
			if line.Name != "" {
				gl.Name = line.Name
			}
			if line.Number != 0 {
				gl.Number = line.Number
			}
		}
		for _, pair := range [][2]TeamLine{{g.Away, g.Home}, {g.Home, g.Away}} {
			us, them := pair[0], pair[1]
			t, ok := teams[us.Abbrev]
			if !ok {
				t = &TeamSeason{ID: us.ID, Abbrev: us.Abbrev}
				teams[us.Abbrev] = t
			}
			if us.Name != "" {
				t.Name = us.Name
			}
			t.GP++
			t.GoalsFor += us.Goals
			t.GoalsAgainst += them.Goals
			t.ShotsFor += us.SOG
			t.ShotsAgainst += them.SOG
			t.PIM += us.PIM
			t.PPGoals += us.PPGoals
			t.Hits += us.Hits
			t.Blocked += us.Blocked
			t.FaceoffWins += us.FaceoffWins
			t.FaceoffLosses += us.FaceoffLosses
			switch us.Result {
			case "W":
				t.Wins++
				t.Points += 2
				if g.Game.LastPeriodType != "SO" {
					t.ROW++
				}
				if us.Home {
					t.HomeWins++
				} else {
					t.AwayWins++
				}
			case "L":
				t.Losses++
				if us.Home {
					t.HomeLosses++
				} else {
					t.AwayLosses++
				}
			case "OTL", "SOL":
				t.OTLosses++
				t.Points++
				if us.Home {
					t.HomeOTLosses++
				} else {
					t.AwayOTLosses++
				}
			case "T":
				t.Ties++
				t.Points++
			}
		}
	}

	sort.Slice(r.Games, func(i, j int) bool {
		a, b := r.Games[i], r.Games[j]
		if a.Date != b.Date {
			return a.Date < b.Date
		}
		if !a.Start.Equal(b.Start) {
			return a.Start.Before(b.Start)
		}
		return a.ID < b.ID
	})
	for _, s := range skaters {
		r.Skaters = append(r.Skaters, *s)
	}
	sort.Slice(r.Skaters, func(i, j int) bool {
		a, b := r.Skaters[i], r.Skaters[j]
		if a.Points != b.Points {
			return a.Points > b.Points
		}
		if a.Goals != b.Goals {
			return a.Goals > b.Goals
		}
		if a.PlayerID != b.PlayerID {
			return a.PlayerID < b.PlayerID
		}
		return a.Team < b.Team
	})
	for _, g := range goalies {
		r.Goalies = append(r.Goalies, *g)
	}
	sort.Slice(r.Goalies, func(i, j int) bool {
		a, b := r.Goalies[i], r.Goalies[j]
		if a.Wins != b.Wins {
			return a.Wins > b.Wins
		}
		if a.PlayerID != b.PlayerID {
			return a.PlayerID < b.PlayerID
		}
		return a.Team < b.Team
	})
	for _, t := range teams {
		r.Teams = append(r.Teams, *t)
	}
	sort.Slice(r.Teams, func(i, j int) bool {
		a, b := r.Teams[i], r.Teams[j]
		if a.Points != b.Points {
			return a.Points > b.Points
		}
		if a.ROW != b.ROW {
			return a.ROW > b.ROW
		}
		if da, db := a.GoalsFor-a.GoalsAgainst, b.GoalsFor-b.GoalsAgainst; da != db {
			return da > db
		}
		return a.Abbrev < b.Abbrev
	})
	return r
}
