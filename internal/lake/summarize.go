package lake

import (
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Summarize builds a game's index document from its feeds. Only the
// play-by-play is required: the boxscore supplies the official skater and
// goalie lines, the landing the three stars, and the shift chart the shift
// counts and time on ice when the boxscore lacks them. What an era does
// not have comes out zero or absent, never as an error.
func Summarize(b Bodies) (*GameSummary, error) {
	var pbp rawPBP
	if err := decode("pbp", b.PBP, &pbp); err != nil {
		return nil, err
	}
	if pbp.ID == 0 {
		return nil, errors.New("pbp has no game id")
	}
	r := newRoster(&pbp)
	s := &GameSummary{Version: Version, Feeds: []string{"pbp"}}
	s.Game = gameRow(&pbp)
	s.Away = teamLine(pbp.AwayTeam, false)
	s.Home = teamLine(pbp.HomeTeam, true)

	derived := walkPlays(&pbp, r, s)

	if len(b.Boxscore) > 0 {
		var box rawBoxscore
		if err := decode("boxscore", b.Boxscore, &box); err == nil {
			s.Feeds = append(s.Feeds, "boxscore")
			s.Skaters, s.Goalies = boxLines(&box, &pbp, r, derived)
		}
	}
	if s.Skaters == nil {
		s.Skaters = derivedLines(r, derived)
	}
	if len(b.Shifts) > 0 {
		if shifts, err := Shifts(b.Shifts); err == nil {
			s.Feeds = append(s.Feeds, "shifts")
			fillShifts(s.Skaters, shifts)
		}
	}
	if len(b.Landing) > 0 {
		var landing rawLanding
		if err := decode("landing", b.Landing, &landing); err == nil {
			s.Feeds = append(s.Feeds, "landing")
			for _, st := range landing.Summary.ThreeStars {
				name := r.name[st.PlayerID]
				if name == "" {
					name = st.Name.Default
				}
				s.Stars = append(s.Stars, Star{
					Star: st.Star, PlayerID: st.PlayerID, Team: st.TeamAbbrev, Name: name,
					Position: st.Position, Goals: st.Goals, Assists: st.Assists, Points: st.Points,
				})
			}
			sort.Slice(s.Stars, func(i, j int) bool { return s.Stars[i].Star < s.Stars[j].Star })
		}
	}

	if s.Game.Final() {
		s.Away.Result, s.Home.Result = results(s.Game)
	}
	sortSkaters(s.Skaters)
	sort.SliceStable(s.Goalies, func(i, j int) bool {
		if s.Goalies[i].Team != s.Goalies[j].Team {
			return s.Goalies[i].Team < s.Goalies[j].Team
		}
		return s.Goalies[i].TOISeconds > s.Goalies[j].TOISeconds
	})
	return s, nil
}

func gameRow(p *rawPBP) GameRow {
	row := GameRow{
		ID: p.ID, Season: p.Season, Type: p.GameType, Date: p.GameDate, State: p.GameState,
		Venue:          p.Venue.Default,
		Away:           TeamRef{ID: p.AwayTeam.ID, Abbrev: p.AwayTeam.Abbrev, Name: p.AwayTeam.name(), Score: p.AwayTeam.Score, SOG: p.AwayTeam.SOG},
		Home:           TeamRef{ID: p.HomeTeam.ID, Abbrev: p.HomeTeam.Abbrev, Name: p.HomeTeam.name(), Score: p.HomeTeam.Score, SOG: p.HomeTeam.SOG},
		Periods:        p.PeriodDescriptor.Number,
		LastPeriodType: p.GameOutcome.LastPeriodType,
		Plays:          len(p.Plays),
	}
	if t, err := time.Parse(time.RFC3339, p.StartTimeUTC); err == nil {
		row.Start = t.UTC()
	}
	if row.Season == 0 {
		row.Season = SeasonOf(row.ID)
	}
	for _, pl := range p.Plays {
		if pl.PeriodDescriptor.Number > row.Periods {
			row.Periods = pl.PeriodDescriptor.Number
		}
	}
	if !row.Final() {
		row.Live = true
	}
	return row
}

func teamLine(t rawTeam, home bool) TeamLine {
	return TeamLine{ID: t.ID, Abbrev: t.Abbrev, Name: t.name(), Home: home, Goals: t.Score, SOG: t.SOG}
}

// results returns the away and home team's result codes for a final game.
func results(g GameRow) (away, home string) {
	loss := "L"
	switch g.LastPeriodType {
	case "OT":
		loss = "OTL"
	case "SO":
		loss = "SOL"
	}
	switch {
	case g.Away.Score > g.Home.Score:
		return "W", loss
	case g.Home.Score > g.Away.Score:
		return loss, "W"
	default:
		return "T", "T"
	}
}

// walkPlays accumulates everything the play-by-play says about players and
// teams: the scoring summary, faceoffs, hits, blocks, penalties and, for
// eras with no boxscore, the counting line itself. The map is keyed by
// player id.
func walkPlays(p *rawPBP, r *roster, s *GameSummary) map[int64]*SkaterStats {
	derived := map[int64]*SkaterStats{}
	stat := func(id int64) *SkaterStats {
		st, ok := derived[id]
		if !ok {
			st = &SkaterStats{}
			derived[id] = st
		}
		return st
	}
	team := func(id int64) *TeamLine {
		switch id {
		case s.Away.ID:
			return &s.Away
		case s.Home.ID:
			return &s.Home
		}
		return nil
	}
	other := func(t *TeamLine) *TeamLine {
		if t == &s.Away {
			return &s.Home
		}
		return &s.Away
	}

	for _, pl := range p.Plays {
		d := pl.Details
		ownerID, _ := idField(d, "eventOwnerTeamId")
		owner := team(ownerID)
		switch pl.TypeDescKey {
		case "goal":
			g := Goal{
				EventID: pl.EventID, Period: pl.PeriodDescriptor.Number, PeriodType: pl.PeriodDescriptor.PeriodType,
				TimeInPeriod: pl.TimeInPeriod, Team: r.teamAbbrev[ownerID], ShotType: strField(d, "shotType"),
				Assists: []PlayerRef{},
			}
			// A shootout's deciding goal is in the summary but is nobody's
			// goal in the statistics, which is how the NHL counts it.
			shootout := pl.PeriodDescriptor.PeriodType == "SO"
			if id, ok := idField(d, "scoringPlayerId"); ok {
				g.Scorer = PlayerRef{ID: id, Name: r.name[id]}
				if !shootout {
					st := stat(id)
					st.Goals++
					st.Points++
					st.SOG++ // a goal is a shot on goal; the feed does not repeat it as one
				}
			}
			for _, key := range []string{"assist1PlayerId", "assist2PlayerId"} {
				if id, ok := idField(d, key); ok {
					g.Assists = append(g.Assists, PlayerRef{ID: id, Name: r.name[id]})
					if !shootout {
						st := stat(id)
						st.Assists++
						st.Points++
					}
				}
			}
			g.Strength, g.EmptyNet = strength(pl.SituationCode, owner != nil && owner.Home)
			if n, ok := intField(d, "awayScore"); ok {
				g.AwayScore = n
			}
			if n, ok := intField(d, "homeScore"); ok {
				g.HomeScore = n
			}
			if g.Strength == "pp" && !shootout {
				if owner != nil {
					owner.PPGoals++
				}
				if g.Scorer.ID != 0 {
					stat(g.Scorer.ID).PPGoals++
				}
			}
			s.Goals = append(s.Goals, g)
		case "shot-on-goal":
			if id, ok := idField(d, "shootingPlayerId"); ok {
				stat(id).SOG++
			}
		case "faceoff":
			if id, ok := idField(d, "winningPlayerId"); ok {
				stat(id).FaceoffWins++
			}
			if id, ok := idField(d, "losingPlayerId"); ok {
				stat(id).FaceoffLosses++
			}
			if owner != nil {
				owner.FaceoffWins++
				other(owner).FaceoffLosses++
			}
		case "hit":
			if id, ok := idField(d, "hittingPlayerId"); ok {
				stat(id).Hits++
			}
			if owner != nil {
				owner.Hits++
			}
		case "blocked-shot":
			if id, ok := idField(d, "blockingPlayerId"); ok {
				stat(id).Blocked++
			}
			if owner != nil {
				owner.Blocked++
			}
		case "penalty":
			mins, _ := intField(d, "duration")
			if id, ok := idField(d, "committedByPlayerId"); ok {
				stat(id).PIM += mins
			}
			if owner != nil {
				owner.PIM += mins
			}
		case "giveaway":
			if id, ok := idField(d, "playerId"); ok {
				stat(id).Giveaways++
			}
			if owner != nil {
				owner.Giveaways++
			}
		case "takeaway":
			if id, ok := idField(d, "playerId"); ok {
				stat(id).Takeaways++
			}
			if owner != nil {
				owner.Takeaways++
			}
		}
	}
	sort.SliceStable(s.Goals, func(i, j int) bool {
		if s.Goals[i].Period != s.Goals[j].Period {
			return s.Goals[i].Period < s.Goals[j].Period
		}
		return s.Goals[i].TimeInPeriod < s.Goals[j].TimeInPeriod
	})
	if s.Goals == nil {
		s.Goals = []Goal{}
	}
	return derived
}

// strength reads the four-digit situation code (away goalie, away skaters,
// home skaters, home goalie) from the scoring team's side. Strength follows
// penalties, not pulled goalies, so the comparison is of players on the
// ice including the goalie: a 6-on-5 with the net empty is even strength,
// a 6-on-4 with the net empty is still a power play. EmptyNet means the
// opponent had no goalie in.
func strength(code string, scorerIsHome bool) (string, bool) {
	if len(code) != 4 {
		return "ev", false
	}
	digit := func(i int) int { n, _ := strconv.Atoi(code[i : i+1]); return n }
	awayGoalie, awaySkaters, homeSkaters, homeGoalie := digit(0), digit(1), digit(2), digit(3)
	own, against, oppGoalie := awaySkaters+awayGoalie, homeSkaters+homeGoalie, homeGoalie
	if scorerIsHome {
		own, against, oppGoalie = homeSkaters+homeGoalie, awaySkaters+awayGoalie, awayGoalie
	}
	emptyNet := oppGoalie == 0
	switch {
	case own > against:
		return "pp", emptyNet
	case own < against:
		return "sh", emptyNet
	}
	return "ev", emptyNet
}

// boxLines builds the skater and goalie lines from the boxscore, adding the
// faceoff counts only the play-by-play knows.
func boxLines(box *rawBoxscore, p *rawPBP, r *roster, derived map[int64]*SkaterStats) ([]SkaterLine, []GoalieLine) {
	var skaters []SkaterLine
	var goalies []GoalieLine
	sides := []struct {
		team rawBoxTeam
		abbr string
	}{{box.PlayerByGameStats.AwayTeam, p.AwayTeam.Abbrev}, {box.PlayerByGameStats.HomeTeam, p.HomeTeam.Abbrev}}
	for _, side := range sides {
		for _, group := range [][]rawSkater{side.team.Forwards, side.team.Defense} {
			for _, sk := range group {
				if sk.PlayerID == 0 {
					continue
				}
				line := SkaterLine{
					PlayerID: sk.PlayerID, Team: side.abbr, Name: playerName(r, sk.PlayerID, sk.Name.Default),
					Number: sk.SweaterNumber, Position: sk.Position,
					SkaterStats: SkaterStats{
						Goals: sk.Goals, Assists: sk.Assists, Points: sk.Points, PlusMinus: sk.PlusMinus,
						PIM: sk.PIM, Hits: sk.Hits, SOG: sk.SOG, Blocked: sk.BlockedShots, PPGoals: sk.PowerPlayGoals,
						Giveaways: sk.Giveaways, Takeaways: sk.Takeaways, Shifts: sk.Shifts,
						TOISeconds: max(clockSeconds(sk.TOI), 0),
					},
				}
				if d := derived[sk.PlayerID]; d != nil {
					line.FaceoffWins, line.FaceoffLosses = d.FaceoffWins, d.FaceoffLosses
				}
				skaters = append(skaters, line)
			}
		}
		for _, g := range side.team.Goalies {
			if g.PlayerID == 0 {
				continue
			}
			line := GoalieLine{
				PlayerID: g.PlayerID, Team: side.abbr, Name: playerName(r, g.PlayerID, g.Name.Default),
				Number: g.SweaterNumber, ShotsAgainst: g.ShotsAgainst, Saves: g.Saves, GoalsAgainst: g.GoalsAgainst,
				TOISeconds: max(clockSeconds(g.TOI), 0), Starter: g.Starter, Decision: g.Decision,
			}
			if line.ShotsAgainst == 0 && g.SaveShotsAgainst != "" {
				if sv, sa, ok := strings.Cut(g.SaveShotsAgainst, "/"); ok {
					line.Saves, _ = strconv.Atoi(sv)
					line.ShotsAgainst, _ = strconv.Atoi(sa)
				}
			}
			if line.ShotsAgainst > 0 {
				line.SavePct = float64(line.Saves) / float64(line.ShotsAgainst)
			}
			line.Shutout = line.GoalsAgainst == 0 && line.Decision == "W"
			goalies = append(goalies, line)
		}
	}
	if skaters == nil {
		return nil, goalies // an empty boxscore: fall back to the play-by-play
	}
	return skaters, goalies
}

// derivedLines builds skater lines from the play-by-play alone, for games
// with no boxscore statistics. Goalies get no line: the feed does not say
// which shots they faced in a way that survives every era.
func derivedLines(r *roster, derived map[int64]*SkaterStats) []SkaterLine {
	lines := make([]SkaterLine, 0, len(derived))
	for id, st := range derived {
		if r.position[id] == "G" {
			continue
		}
		lines = append(lines, SkaterLine{
			PlayerID: id, Team: r.team[id], Name: r.name[id], Number: r.number[id], Position: r.position[id],
			SkaterStats: *st,
		})
	}
	return lines
}

// fillShifts supplies shift counts and time on ice from the chart where the
// boxscore gave none.
func fillShifts(skaters []SkaterLine, shifts []Shift) {
	count := map[int64]int{}
	secs := map[int64]int{}
	for _, sh := range shifts {
		count[sh.PlayerID]++
		secs[sh.PlayerID] += sh.Seconds
	}
	for i := range skaters {
		if skaters[i].Shifts == 0 {
			skaters[i].Shifts = count[skaters[i].PlayerID]
		}
		if skaters[i].TOISeconds == 0 {
			skaters[i].TOISeconds = secs[skaters[i].PlayerID]
		}
	}
}

func playerName(r *roster, id int64, fallback string) string {
	if n := r.name[id]; n != "" {
		return n
	}
	return fallback
}

func sortSkaters(s []SkaterLine) {
	sort.SliceStable(s, func(i, j int) bool {
		if s[i].Team != s[j].Team {
			return s[i].Team < s[j].Team
		}
		if s[i].Points != s[j].Points {
			return s[i].Points > s[j].Points
		}
		if s[i].Number != s[j].Number {
			return s[i].Number < s[j].Number
		}
		return s[i].PlayerID < s[j].PlayerID
	})
}
