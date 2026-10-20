package api

import (
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"hockeytrack/internal/lake"
)

const searchCap = 25

type searchHit struct {
	Type   string `json:"type"` // team, player, venue, game
	ID     string `json:"id"`
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
	Href   string `json:"href"`
}

type searchResult struct {
	Query   string      `json:"query"`
	Season  int64       `json:"season"`
	Teams   []searchHit `json:"teams"`
	Players []searchHit `json:"players"`
	Venues  []searchHit `json:"venues"`
	Games   []searchHit `json:"games"`
}

// handleSearch is a case-insensitive substring search over the names the
// schedule document and the season rollup know: teams, players, venues,
// and games by id or date.
func (s *Server) handleSearch(r *http.Request) (any, time.Duration, error) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) < 2 || len(q) > 64 {
		return nil, 0, badRequest("q must be between 2 and 64 characters")
	}
	season, err := s.querySeason(r)
	if err != nil {
		return nil, 0, err
	}
	ctx := r.Context()
	needle := strings.ToLower(q)
	res := searchResult{Query: q, Season: season, Teams: []searchHit{}, Players: []searchHit{}, Venues: []searchHit{}, Games: []searchHit{}}

	names, err := s.teamNames(r)
	if err != nil {
		return nil, 0, err
	}
	for abbrev, name := range names {
		if strings.Contains(strings.ToLower(abbrev), needle) || strings.Contains(strings.ToLower(name), needle) {
			res.Teams = append(res.Teams, searchHit{Type: "team", ID: abbrev, Label: name, Detail: abbrev, Href: "/v1/teams/" + abbrev})
		}
	}
	sort.Slice(res.Teams, func(i, j int) bool { return res.Teams[i].ID < res.Teams[j].ID })

	ro, err := s.rollup(ctx, season)
	if err != nil && !errors.Is(err, errNotFound) {
		return nil, 0, err
	}
	if ro != nil {
		seen := map[int64]bool{}
		add := func(id int64, name, team, pos string) {
			if seen[id] || !strings.Contains(strings.ToLower(name), needle) {
				return
			}
			seen[id] = true
			res.Players = append(res.Players, searchHit{Type: "player", ID: strconv.FormatInt(id, 10), Label: name,
				Detail: strings.TrimSpace(team + " " + pos), Href: "/v1/players/" + strconv.FormatInt(id, 10) + "?season=" + strconv.FormatInt(season, 10)})
		}
		for _, sk := range ro.Skaters {
			add(sk.PlayerID, sk.Name, sk.Team, sk.Position)
		}
		for _, g := range ro.Goalies {
			add(g.PlayerID, g.Name, g.Team, "G")
		}
		sort.SliceStable(res.Players, func(i, j int) bool { return res.Players[i].Label < res.Players[j].Label })

		// A game id or a date finds games directly.
		if id, err := strconv.ParseInt(q, 10, 64); err == nil && len(q) == 10 {
			for _, g := range ro.Games {
				if g.ID == id {
					res.Games = append(res.Games, gameHit(g))
				}
			}
		}
		if _, err := time.Parse("2006-01-02", q); err == nil {
			for _, g := range ro.Games {
				if g.Date == q {
					res.Games = append(res.Games, gameHit(g))
				}
			}
		}
	}

	if sched, err := s.schedule(ctx); err == nil {
		venues := map[string]int{}
		for _, g := range sched.Games {
			if g.Venue != "" && strings.Contains(strings.ToLower(g.Venue), needle) {
				venues[g.Venue]++
			}
			// A team pair ("TBL BOS", "bos@tbl") finds the matchup.
			if matchup(needle, g.Away, g.Home) {
				res.Games = append(res.Games, searchHit{Type: "game", ID: strconv.FormatInt(g.ID, 10),
					Label: g.Away + " @ " + g.Home + " " + g.Date, Href: "/v1/games/" + strconv.FormatInt(g.ID, 10)})
			}
		}
		for v, n := range venues {
			res.Venues = append(res.Venues, searchHit{Type: "venue", ID: v, Label: v, Detail: strconv.Itoa(n) + " games",
				Href: "/v1/schedule?venue=" + url.QueryEscape(v)})
		}
		sort.Slice(res.Venues, func(i, j int) bool { return res.Venues[i].Label < res.Venues[j].Label })
	} else if !errors.Is(err, errNotFound) {
		return nil, 0, err
	}

	res.Teams = capHits(res.Teams)
	res.Players = capHits(res.Players)
	res.Venues = capHits(res.Venues)
	res.Games = capHits(res.Games)
	return res, ttlIndex, nil
}

func gameHit(g lake.GameRow) searchHit {
	id := strconv.FormatInt(g.ID, 10)
	detail := ""
	if g.Final() {
		detail = strconv.Itoa(g.Away.Score) + "-" + strconv.Itoa(g.Home.Score)
		if g.LastPeriodType == "OT" || g.LastPeriodType == "SO" {
			detail += " " + g.LastPeriodType
		}
	}
	return searchHit{Type: "game", ID: id, Label: g.Away.Abbrev + " @ " + g.Home.Abbrev + " " + g.Date, Detail: detail, Href: "/v1/games/" + id}
}

// matchup reports whether the query names both teams of a game, in either
// order, separated by a space, "@", "at", "vs" or "-".
func matchup(needle, away, home string) bool {
	f := strings.FieldsFunc(needle, func(c rune) bool { return c == ' ' || c == '@' || c == '-' || c == ',' })
	var teams []string
	for _, w := range f {
		if w == "at" || w == "vs" || w == "v" {
			continue
		}
		teams = append(teams, strings.ToUpper(w))
	}
	if len(teams) != 2 {
		return false
	}
	return (teams[0] == away && teams[1] == home) || (teams[0] == home && teams[1] == away)
}

func capHits(h []searchHit) []searchHit {
	if len(h) > searchCap {
		return h[:searchCap]
	}
	return h
}
