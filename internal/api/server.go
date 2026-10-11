// Package api serves the archive over HTTP: schedule, games, plays,
// boxscores, shifts, standings, season statistics, players and search.
//
// It is read-only and public. Every path segment that reaches a lookup is
// validated first (numeric ids, letter-only abbreviations, ISO dates, a
// fixed feed list), and no query parameter is ever interpolated into a
// key. Responses carry Cache-Control so CloudFront does the heavy lifting.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"hockeytrack/internal/lake"
	"hockeytrack/internal/schedsync"
)

// Deps are the three read-only stores the API serves from.
type Deps struct {
	Raw   lake.Source // the raw archive bucket
	Index lake.Source // the index bucket (summaries and rollups)
	Site  lake.Source // the site bucket, for data/schedule.json; may be nil
	Now   func() time.Time
	// Version is reported by GET /v1; the image tag in production.
	Version string
}

// Server holds the dependencies and the in-process cache.
type Server struct {
	deps Deps
	memo *memo
	mux  *http.ServeMux
}

// New builds the handler.
func New(d Deps) *Server {
	if d.Now == nil {
		d.Now = time.Now
	}
	s := &Server{deps: d, memo: newMemo(d.Now), mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, 0, map[string]string{"status": "ok"})
	})
	m.HandleFunc("GET /v1", s.wrap(s.handleRoot))
	m.HandleFunc("GET /v1/{$}", s.wrap(s.handleRoot))
	m.HandleFunc("GET /v1/openapi.json", s.handleOpenAPI)
	m.HandleFunc("GET /v1/seasons", s.wrap(s.handleSeasons))
	m.HandleFunc("GET /v1/seasons/{season}", s.wrap(s.handleSeason))
	m.HandleFunc("GET /v1/schedule", s.wrap(s.handleSchedule))
	m.HandleFunc("GET /v1/teams", s.wrap(s.handleTeams))
	m.HandleFunc("GET /v1/teams/{team}", s.wrap(s.handleTeam))
	m.HandleFunc("GET /v1/teams/{team}/games", s.wrap(s.handleTeamGames))
	m.HandleFunc("GET /v1/teams/{team}/roster", s.wrap(s.handleTeamRoster))
	m.HandleFunc("GET /v1/standings", s.wrap(s.handleStandings))
	m.HandleFunc("GET /v1/games", s.wrap(s.handleGames))
	m.HandleFunc("GET /v1/games/{id}", s.wrap(s.handleGame))
	m.HandleFunc("GET /v1/games/{id}/plays", s.wrap(s.handlePlays))
	m.HandleFunc("GET /v1/games/{id}/boxscore", s.wrap(s.handleBoxscore))
	m.HandleFunc("GET /v1/games/{id}/shifts", s.wrap(s.handleShifts))
	m.HandleFunc("GET /v1/games/{id}/raw/{feed}", s.handleRaw)
	m.HandleFunc("GET /v1/stats/skaters", s.wrap(s.handleSkaters))
	m.HandleFunc("GET /v1/stats/goalies", s.wrap(s.handleGoalies))
	m.HandleFunc("GET /v1/stats/teams", s.wrap(s.handleTeamStats))
	m.HandleFunc("GET /v1/players/{id}", s.wrap(s.handlePlayer))
	m.HandleFunc("GET /v1/players/{id}/games", s.wrap(s.handlePlayerGames))
	m.HandleFunc("GET /v1/search", s.wrap(s.handleSearch))
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, r, &apiError{Status: http.StatusMethodNotAllowed, Code: "method_not_allowed", Message: "this API is read-only"})
			return
		}
		writeError(w, r, notFound("no such endpoint; see /v1/openapi.json"))
	})
}

// ---- loaders ----

// missing reports whether a Get failed because the object does not exist,
// for S3, a directory and the in-memory fake alike.
func missing(err error) bool {
	var nsk *types.NoSuchKey
	var nf *types.NotFound
	return errors.As(err, &nsk) || errors.As(err, &nf) || os.IsNotExist(err) || strings.Contains(err.Error(), "no such key")
}

// seasons lists the indexed seasons, newest first.
func (s *Server) seasons(ctx context.Context) ([]int64, error) {
	v, err := s.memo.get("seasons", ttlIndex, func() (any, error) {
		return lake.ListSeasons(ctx, s.deps.Index)
	})
	if err != nil {
		return nil, err
	}
	return v.([]int64), nil
}

// currentSeason is the newest indexed season, or the calendar's when the
// index is empty (every lookup then reports the season as not indexed).
func (s *Server) currentSeason(ctx context.Context) (int64, error) {
	seasons, err := s.seasons(ctx)
	if err != nil {
		return 0, err
	}
	if len(seasons) == 0 {
		return lake.CurrentSeason(s.deps.Now()), nil
	}
	return seasons[0], nil
}

func (s *Server) rollup(ctx context.Context, season int64) (*lake.SeasonRollup, error) {
	v, err := s.memo.get(fmt.Sprintf("rollup:%d", season), ttlIndex, func() (any, error) {
		body, err := s.deps.Index.Get(ctx, lake.RollupKey(season))
		if err != nil {
			if missing(err) {
				return nil, fmt.Errorf("season %d is not indexed: %w", season, errNotFound)
			}
			return nil, err
		}
		var r lake.SeasonRollup
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, fmt.Errorf("rollup %d: %w", season, err)
		}
		return &r, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*lake.SeasonRollup), nil
}

// schedule is the website's schedule document: the whole published season.
func (s *Server) schedule(ctx context.Context) (*schedsync.SitePayload, error) {
	v, err := s.memo.get("schedule", ttlIndex, func() (any, error) {
		if s.deps.Site == nil {
			return nil, fmt.Errorf("schedule document not configured: %w", errNotFound)
		}
		body, err := s.deps.Site.Get(ctx, schedsync.SiteKey)
		if err != nil {
			if missing(err) {
				return nil, fmt.Errorf("schedule document not published yet: %w", errNotFound)
			}
			return nil, err
		}
		var p schedsync.SitePayload
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, fmt.Errorf("schedule document: %w", err)
		}
		return &p, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*schedsync.SitePayload), nil
}

// gameSummary fetches a game's index document, or errNotFound.
func (s *Server) gameSummary(ctx context.Context, id int64) (*lake.GameSummary, error) {
	body, err := s.deps.Index.Get(ctx, lake.SummaryKey(lake.SeasonOf(id), id))
	if err != nil {
		if missing(err) {
			return nil, errNotFound
		}
		return nil, err
	}
	var g lake.GameSummary
	if err := json.Unmarshal(body, &g); err != nil || g.Game.ID != id {
		return nil, badGateway("the index document for game %d is unreadable", id)
	}
	return &g, nil
}

// gamePrefix finds the archive prefix of a game. The id gives the season;
// the date comes from the rollup, then the schedule document, and as a
// last resort from listing the season, which is slow and logged.
func (s *Server) gamePrefix(ctx context.Context, id int64) (string, error) {
	season := lake.SeasonOf(id)
	if r, err := s.rollup(ctx, season); err == nil {
		for _, g := range r.Games {
			if g.ID == id {
				return lake.GamePrefix(season, g.Date, id), nil
			}
		}
	} else if !errors.Is(err, errNotFound) {
		return "", err
	}
	if sched, err := s.schedule(ctx); err == nil {
		for _, g := range sched.Games {
			if g.ID == id {
				return lake.GamePrefix(season, g.Date, id), nil
			}
		}
	} else if !errors.Is(err, errNotFound) {
		return "", err
	}
	v, err := s.memo.get(fmt.Sprintf("prefix:%d", id), ttlFinal, func() (any, error) {
		slog.Info("locating game by listing its season", "gameId", id, "season", season)
		keys, err := s.deps.Raw.List(ctx, fmt.Sprintf("raw/%d/", season))
		if err != nil {
			return nil, err
		}
		want := fmt.Sprintf("/%d/", id)
		for _, k := range keys {
			if i := strings.Index(k, want); i >= 0 {
				return k[:i+len(want)], nil
			}
		}
		return nil, fmt.Errorf("game %d is not in the archive: %w", id, errNotFound)
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// gameFeeds locates a game's feeds in the archive.
func (s *Server) gameFeeds(ctx context.Context, id int64) (lake.Feeds, error) {
	prefix, err := s.gamePrefix(ctx, id)
	if err != nil {
		return lake.Feeds{}, err
	}
	f, err := lake.Locate(ctx, s.deps.Raw, prefix)
	if errors.Is(err, lake.ErrNoFeeds) {
		return f, fmt.Errorf("game %d has no play-by-play in the archive: %w", id, errNotFound)
	}
	return f, err
}

// ---- small endpoints ----

func (s *Server) handleRoot(r *http.Request) (any, time.Duration, error) {
	ctx := r.Context()
	season, err := s.currentSeason(ctx)
	if err != nil {
		return nil, 0, err
	}
	seasons, _ := s.seasons(ctx)
	return map[string]any{
		"name":    "HockeyTrack API",
		"version": s.deps.Version,
		"season":  season,
		"seasons": len(seasons),
		"docs":    "/v1/openapi.json",
		"links": map[string]string{
			"schedule":  "/v1/schedule?date=today",
			"standings": "/v1/standings",
			"games":     fmt.Sprintf("/v1/games?season=%d", season),
			"skaters":   fmt.Sprintf("/v1/stats/skaters?season=%d&sort=points", season),
			"goalies":   fmt.Sprintf("/v1/stats/goalies?season=%d&sort=savePct&minGp=5", season),
			"teams":     fmt.Sprintf("/v1/stats/teams?season=%d", season),
			"search":    "/v1/search?q=lightning",
		},
	}, ttlIndex, nil
}

func (s *Server) handleSeasons(r *http.Request) (any, time.Duration, error) {
	seasons, err := s.seasons(r.Context())
	if err != nil {
		return nil, 0, err
	}
	items := make([]map[string]any, 0, len(seasons))
	for _, se := range seasons {
		items = append(items, map[string]any{"season": se, "href": fmt.Sprintf("/v1/seasons/%d", se)})
	}
	return page{Total: len(items), Limit: len(items), Items: items}, ttlIndex, nil
}

type seasonMeta struct {
	Season      int64     `json:"season"`
	GeneratedAt time.Time `json:"generatedAt"`
	Games       int       `json:"games"`
	FinalGames  int       `json:"finalGames"`
	FirstDate   string    `json:"firstDate,omitempty"`
	LastDate    string    `json:"lastDate,omitempty"`
	Teams       []string  `json:"teams"`
	Skaters     int       `json:"skaters"`
	Goalies     int       `json:"goalies"`
}

func (s *Server) handleSeason(r *http.Request) (any, time.Duration, error) {
	season, err := pathID(r, "season")
	if err != nil {
		return nil, 0, err
	}
	ro, err := s.rollup(r.Context(), season)
	if err != nil {
		return nil, 0, err
	}
	m := seasonMeta{Season: ro.Season, GeneratedAt: ro.GeneratedAt, Games: len(ro.Games), Teams: []string{},
		Skaters: len(ro.Skaters), Goalies: len(ro.Goalies)}
	for _, g := range ro.Games {
		if g.Final() {
			m.FinalGames++
		}
		if m.FirstDate == "" || g.Date < m.FirstDate {
			m.FirstDate = g.Date
		}
		if g.Date > m.LastDate {
			m.LastDate = g.Date
		}
	}
	for _, t := range ro.Teams {
		m.Teams = append(m.Teams, t.Abbrev)
	}
	sort.Strings(m.Teams)
	return m, ttlIndex, nil
}
