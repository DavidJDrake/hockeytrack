package api

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"hockeytrack/internal/lake"
)

// handleGames lists the games the index knows, with results.
func (s *Server) handleGames(r *http.Request) (any, time.Duration, error) {
	season, err := s.querySeason(r)
	if err != nil {
		return nil, 0, err
	}
	limit, offset, err := paging(r)
	if err != nil {
		return nil, 0, err
	}
	date, _, err := queryDate(r, "date", s.deps.Now())
	if err != nil {
		return nil, 0, err
	}
	team, err := queryTeam(r, "team")
	if err != nil {
		return nil, 0, err
	}
	gameType, hasType, err := queryInt(r, "type")
	if err != nil {
		return nil, 0, err
	}
	ro, err := s.rollup(r.Context(), season)
	if err != nil {
		return nil, 0, err
	}
	var items []lake.GameRow
	for _, g := range ro.Games {
		if date != "" && g.Date != date {
			continue
		}
		if team != "" && g.Away.Abbrev != team && g.Home.Abbrev != team {
			continue
		}
		if hasType && g.Type != gameType {
			continue
		}
		items = append(items, g)
	}
	return slicePage(season, items, limit, offset), ttlIndex, nil
}

// gameDetail is what GET /v1/games/{id} returns: the index document when
// the game is finished and indexed, otherwise a summary built on the spot
// from the newest feeds, which is how a live game is served.
func (s *Server) gameDetail(r *http.Request) (*lake.GameSummary, time.Duration, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, 0, err
	}
	ctx := r.Context()
	g, err := s.gameSummary(ctx, id)
	if err == nil {
		return g, ttlFinal, nil
	}
	if !errors.Is(err, errNotFound) {
		return nil, 0, err
	}
	feeds, err := s.gameFeeds(ctx, id)
	if err != nil {
		return nil, 0, err
	}
	bodies, err := lake.Load(ctx, s.deps.Raw, feeds)
	if err != nil {
		return nil, 0, err
	}
	g, err = lake.Summarize(bodies)
	if err != nil {
		return nil, 0, badGateway("game %d: %v", id, err)
	}
	if g.Game.ID != id {
		return nil, 0, badGateway("the archive holds game %d under %d", g.Game.ID, id)
	}
	ttl := ttlLive
	if feeds.Final {
		ttl = ttlIndex // finished but not yet indexed; the indexer is minutes away
	}
	return g, ttl, nil
}

func (s *Server) handleGame(r *http.Request) (any, time.Duration, error) {
	g, ttl, err := s.gameDetail(r)
	if err != nil {
		return nil, 0, err
	}
	return g, ttl, nil
}

func (s *Server) handleBoxscore(r *http.Request) (any, time.Duration, error) {
	g, ttl, err := s.gameDetail(r)
	if err != nil {
		return nil, 0, err
	}
	return map[string]any{
		"game": g.Game, "away": g.Away, "home": g.Home,
		"skaters": g.Skaters, "goalies": g.Goalies, "feeds": g.Feeds,
	}, ttl, nil
}

// handlePlays returns a game's plays, filtered.
func (s *Server) handlePlays(r *http.Request) (any, time.Duration, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, 0, err
	}
	limit, offset, err := paging(r)
	if err != nil {
		return nil, 0, err
	}
	q := r.URL.Query()
	types := map[string]bool{}
	for _, t := range strings.Split(q.Get("type"), ",") {
		if t = strings.TrimSpace(strings.ToLower(t)); t != "" {
			types[t] = true
		}
	}
	team, err := queryTeam(r, "team")
	if err != nil {
		return nil, 0, err
	}
	period, hasPeriod, err := queryInt(r, "period")
	if err != nil {
		return nil, 0, err
	}
	player, hasPlayer, err := queryInt64(r, "player")
	if err != nil {
		return nil, 0, err
	}
	ctx := r.Context()
	feeds, err := s.gameFeeds(ctx, id)
	if err != nil {
		return nil, 0, err
	}
	body, err := s.deps.Raw.Get(ctx, feeds.PBP)
	if err != nil {
		return nil, 0, err
	}
	plays, err := lake.Plays(body)
	if err != nil {
		return nil, 0, badGateway("game %d: %v", id, err)
	}
	var items []lake.Play
	for _, p := range plays {
		if len(types) > 0 && !types[p.Type] {
			continue
		}
		if team != "" && p.Team != team {
			continue
		}
		if hasPeriod && p.Period != period {
			continue
		}
		if hasPlayer && !names(p, player) {
			continue
		}
		items = append(items, p)
	}
	ttl := ttlLive
	if feeds.Final {
		ttl = ttlFinal
	}
	return slicePage(lake.SeasonOf(id), items, limit, offset), ttl, nil
}

func names(p lake.Play, player int64) bool {
	for _, pp := range p.Players {
		if pp.ID == player {
			return true
		}
	}
	return false
}

// handleShifts returns the shift chart as rows, filtered.
func (s *Server) handleShifts(r *http.Request) (any, time.Duration, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, 0, err
	}
	limit, offset, err := paging(r)
	if err != nil {
		return nil, 0, err
	}
	team, err := queryTeam(r, "team")
	if err != nil {
		return nil, 0, err
	}
	period, hasPeriod, err := queryInt(r, "period")
	if err != nil {
		return nil, 0, err
	}
	player, hasPlayer, err := queryInt64(r, "player")
	if err != nil {
		return nil, 0, err
	}
	ctx := r.Context()
	feeds, err := s.gameFeeds(ctx, id)
	if err != nil {
		return nil, 0, err
	}
	if feeds.Shifts == "" {
		return nil, 0, notFound("game %d has no shift chart in the archive (charts exist from 2010-11 and arrive with the final sweep)", id)
	}
	body, err := s.deps.Raw.Get(ctx, feeds.Shifts)
	if err != nil {
		return nil, 0, err
	}
	shifts, err := lake.Shifts(body)
	if err != nil {
		return nil, 0, badGateway("game %d: %v", id, err)
	}
	var items []lake.Shift
	for _, sh := range shifts {
		if team != "" && sh.Team != team {
			continue
		}
		if hasPeriod && sh.Period != period {
			continue
		}
		if hasPlayer && sh.PlayerID != player {
			continue
		}
		items = append(items, sh)
	}
	return slicePage(lake.SeasonOf(id), items, limit, offset), ttlFinal, nil
}

var rawFeeds = map[string]bool{"pbp": true, "boxscore": true, "landing": true, "shifts": true}

// handleRaw passes an archived feed through as the NHL sent it. It is
// third-party JSON and the README's warning about raw applies: validate
// what you use.
func (s *Server) handleRaw(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, r, err)
		return
	}
	feed := r.PathValue("feed")
	if !rawFeeds[feed] {
		writeError(w, r, badRequest("feed must be one of pbp, boxscore, landing, shifts"))
		return
	}
	ctx := r.Context()
	feeds, err := s.gameFeeds(ctx, id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	var key string
	switch feed {
	case "pbp":
		key = feeds.PBP
	case "boxscore":
		key = feeds.Boxscore
	case "landing":
		key = feeds.Landing
	case "shifts":
		key = feeds.Shifts
	}
	if key == "" {
		writeError(w, r, notFound("game %d has no %s feed in the archive", id, feed))
		return
	}
	body, err := s.deps.Raw.Get(ctx, key)
	if err != nil {
		writeError(w, r, err)
		return
	}
	ttl := ttlLive
	if feeds.Final {
		ttl = ttlFinal
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(ttl/time.Second)))
	h.Set("X-Archive-Key", key) // which object answered: final/ or a live snapshot
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// gamesForTeam returns a season's games involving a team, in date order.
func gamesForTeam(ro *lake.SeasonRollup, team string) []lake.GameRow {
	var out []lake.GameRow
	for _, g := range ro.Games {
		if g.Away.Abbrev == team || g.Home.Abbrev == team {
			out = append(out, g)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out
}
