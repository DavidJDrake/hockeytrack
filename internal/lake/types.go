// Package lake derives queryable, typed documents from the raw feeds in the
// archive: one summary per game and one rollup per season. Everything here
// is a pure function of archive bytes, so the index it produces can be
// thrown away and rebuilt at any time.
//
// Every raw field is third-party JSON and is decoded as untrusted: a value
// of an unexpected type blanks that field and nothing else, and only
// malformed JSON fails a game.
package lake

import (
	"context"
	"time"
)

// Version is written into every index document so a reader can tell a
// stale layout from a current one.
const Version = 1

// Source is the read side of an archive: store.S3Archive, store.DirArchive
// and store.FakeArchive all satisfy it.
type Source interface {
	List(ctx context.Context, prefix string) ([]string, error)
	Get(ctx context.Context, key string) ([]byte, error)
}

// Store is a Source that can also be written, which the index needs.
type Store interface {
	Source
	Put(ctx context.Context, key string, body []byte) error
}

// TeamRef identifies a team in a game, with its score and shots.
type TeamRef struct {
	ID     int64  `json:"id"`
	Abbrev string `json:"abbrev"`
	Name   string `json:"name,omitempty"`
	Score  int    `json:"score"`
	SOG    int    `json:"sog"`
}

// GameRow is one game's header: what a schedule or results list shows.
type GameRow struct {
	ID             int64     `json:"id"`
	Season         int64     `json:"season"`
	Type           int       `json:"type"` // 1 preseason, 2 regular season, 3 playoffs
	Date           string    `json:"date"` // YYYY-MM-DD, the NHL's gameDate
	Start          time.Time `json:"start"`
	State          string    `json:"state"`
	Venue          string    `json:"venue,omitempty"`
	Away           TeamRef   `json:"away"`
	Home           TeamRef   `json:"home"`
	Periods        int       `json:"periods"`
	LastPeriodType string    `json:"lastPeriodType,omitempty"` // REG, OT, SO
	Plays          int       `json:"plays"`
	// Live is set when the row was built from an in-progress snapshot
	// rather than the final feed.
	Live bool `json:"live,omitempty"`
}

// Final reports whether the game is over.
func (g GameRow) Final() bool { return g.State == "FINAL" || g.State == "OFF" }

// TeamLine is one team's totals for one game.
type TeamLine struct {
	ID            int64  `json:"id"`
	Abbrev        string `json:"abbrev"`
	Name          string `json:"name,omitempty"`
	Home          bool   `json:"home"`
	Goals         int    `json:"goals"`
	SOG           int    `json:"sog"`
	Hits          int    `json:"hits"`
	Blocked       int    `json:"blocked"`
	PIM           int    `json:"pim"`
	PPGoals       int    `json:"ppGoals"`
	FaceoffWins   int    `json:"faceoffWins"`
	FaceoffLosses int    `json:"faceoffLosses"`
	Giveaways     int    `json:"giveaways"`
	Takeaways     int    `json:"takeaways"`
	// Result is W, L, OTL, SOL or T; empty while the game is live.
	Result string `json:"result,omitempty"`
}

// SkaterStats are the counting statistics a skater accumulates; the same
// struct is one game's line and a season's total.
type SkaterStats struct {
	Goals         int `json:"goals"`
	Assists       int `json:"assists"`
	Points        int `json:"points"`
	PlusMinus     int `json:"plusMinus"`
	PIM           int `json:"pim"`
	Hits          int `json:"hits"`
	SOG           int `json:"sog"`
	Blocked       int `json:"blocked"`
	PPGoals       int `json:"ppGoals"`
	Giveaways     int `json:"giveaways"`
	Takeaways     int `json:"takeaways"`
	Shifts        int `json:"shifts"`
	TOISeconds    int `json:"toiSeconds"`
	FaceoffWins   int `json:"faceoffWins"`
	FaceoffLosses int `json:"faceoffLosses"`
}

func (s *SkaterStats) add(o SkaterStats) {
	s.Goals += o.Goals
	s.Assists += o.Assists
	s.Points += o.Points
	s.PlusMinus += o.PlusMinus
	s.PIM += o.PIM
	s.Hits += o.Hits
	s.SOG += o.SOG
	s.Blocked += o.Blocked
	s.PPGoals += o.PPGoals
	s.Giveaways += o.Giveaways
	s.Takeaways += o.Takeaways
	s.Shifts += o.Shifts
	s.TOISeconds += o.TOISeconds
	s.FaceoffWins += o.FaceoffWins
	s.FaceoffLosses += o.FaceoffLosses
}

// SkaterLine is one skater's line in one game.
type SkaterLine struct {
	PlayerID int64  `json:"playerId"`
	Team     string `json:"team"`
	Name     string `json:"name"`
	Number   int    `json:"number,omitempty"`
	Position string `json:"position,omitempty"`
	SkaterStats
}

// GoalieLine is one goalie's line in one game.
type GoalieLine struct {
	PlayerID     int64   `json:"playerId"`
	Team         string  `json:"team"`
	Name         string  `json:"name"`
	Number       int     `json:"number,omitempty"`
	ShotsAgainst int     `json:"shotsAgainst"`
	Saves        int     `json:"saves"`
	GoalsAgainst int     `json:"goalsAgainst"`
	SavePct      float64 `json:"savePct"`
	TOISeconds   int     `json:"toiSeconds"`
	Starter      bool    `json:"starter"`
	Decision     string  `json:"decision,omitempty"` // W, L, O
	Shutout      bool    `json:"shutout"`
}

// PlayerRef names a player in a scoring summary or star list.
type PlayerRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name,omitempty"`
}

// Goal is one line of the scoring summary.
type Goal struct {
	EventID      int64       `json:"eventId"`
	Period       int         `json:"period"`
	PeriodType   string      `json:"periodType"`
	TimeInPeriod string      `json:"timeInPeriod"`
	Team         string      `json:"team"`
	Scorer       PlayerRef   `json:"scorer"`
	Assists      []PlayerRef `json:"assists"`
	Strength     string      `json:"strength"` // ev, pp, sh
	EmptyNet     bool        `json:"emptyNet,omitempty"`
	ShotType     string      `json:"shotType,omitempty"`
	AwayScore    int         `json:"awayScore"`
	HomeScore    int         `json:"homeScore"`
}

// Star is one of the game's three stars.
type Star struct {
	Star     int    `json:"star"`
	PlayerID int64  `json:"playerId"`
	Team     string `json:"team"`
	Name     string `json:"name"`
	Position string `json:"position,omitempty"`
	Goals    int    `json:"goals"`
	Assists  int    `json:"assists"`
	Points   int    `json:"points"`
}

// GameSummary is the index document for one game.
type GameSummary struct {
	Version int          `json:"version"`
	Game    GameRow      `json:"game"`
	Away    TeamLine     `json:"away"`
	Home    TeamLine     `json:"home"`
	Skaters []SkaterLine `json:"skaters"`
	Goalies []GoalieLine `json:"goalies"`
	Goals   []Goal       `json:"goals"`
	Stars   []Star       `json:"stars,omitempty"`
	// Feeds lists which of pbp, boxscore, landing and shifts were read.
	Feeds []string `json:"feeds"`
}

// SkaterSeason is one skater's totals for one team in one season. A traded
// player has one row per team.
type SkaterSeason struct {
	PlayerID int64  `json:"playerId"`
	Team     string `json:"team"`
	Name     string `json:"name"`
	Number   int    `json:"number,omitempty"`
	Position string `json:"position,omitempty"`
	GP       int    `json:"gp"`
	SkaterStats
}

// GoalieSeason is one goalie's totals for one team in one season.
type GoalieSeason struct {
	PlayerID     int64  `json:"playerId"`
	Team         string `json:"team"`
	Name         string `json:"name"`
	Number       int    `json:"number,omitempty"`
	GP           int    `json:"gp"`
	Starts       int    `json:"starts"`
	Wins         int    `json:"wins"`
	Losses       int    `json:"losses"`
	OTLosses     int    `json:"otLosses"`
	Shutouts     int    `json:"shutouts"`
	ShotsAgainst int    `json:"shotsAgainst"`
	Saves        int    `json:"saves"`
	GoalsAgainst int    `json:"goalsAgainst"`
	TOISeconds   int    `json:"toiSeconds"`
}

// TeamSeason is one team's record and totals, computed from the games in
// the archive rather than taken from the NHL's standings.
type TeamSeason struct {
	ID            int64  `json:"id"`
	Abbrev        string `json:"abbrev"`
	Name          string `json:"name,omitempty"`
	GP            int    `json:"gp"`
	Wins          int    `json:"wins"`
	Losses        int    `json:"losses"`
	OTLosses      int    `json:"otLosses"`
	Ties          int    `json:"ties"`
	Points        int    `json:"points"`
	ROW           int    `json:"row"` // regulation plus overtime wins
	GoalsFor      int    `json:"goalsFor"`
	GoalsAgainst  int    `json:"goalsAgainst"`
	ShotsFor      int    `json:"shotsFor"`
	ShotsAgainst  int    `json:"shotsAgainst"`
	PIM           int    `json:"pim"`
	PPGoals       int    `json:"ppGoals"`
	Hits          int    `json:"hits"`
	Blocked       int    `json:"blocked"`
	FaceoffWins   int    `json:"faceoffWins"`
	FaceoffLosses int    `json:"faceoffLosses"`
	HomeWins      int    `json:"homeWins"`
	HomeLosses    int    `json:"homeLosses"`
	HomeOTLosses  int    `json:"homeOtLosses"`
	AwayWins      int    `json:"awayWins"`
	AwayLosses    int    `json:"awayLosses"`
	AwayOTLosses  int    `json:"awayOtLosses"`
}

// SeasonRollup is the index document for one season.
type SeasonRollup struct {
	Version     int            `json:"version"`
	Season      int64          `json:"season"`
	GeneratedAt time.Time      `json:"generatedAt"`
	Games       []GameRow      `json:"games"`
	Skaters     []SkaterSeason `json:"skaters"`
	Goalies     []GoalieSeason `json:"goalies"`
	Teams       []TeamSeason   `json:"teams"`
}

// Play is one play-by-play event with the fields a query filters on typed,
// and the NHL's details object passed through untouched.
type Play struct {
	Seq           int64        `json:"seq"`
	EventID       int64        `json:"eventId"`
	Period        int          `json:"period"`
	PeriodType    string       `json:"periodType"`
	TimeInPeriod  string       `json:"timeInPeriod"`
	TimeRemaining string       `json:"timeRemaining"`
	Type          string       `json:"type"`
	TypeCode      int          `json:"typeCode"`
	SituationCode string       `json:"situationCode,omitempty"`
	Team          string       `json:"team,omitempty"`
	Players       []PlayPlayer `json:"players,omitempty"`
	X             *float64     `json:"x,omitempty"`
	Y             *float64     `json:"y,omitempty"`
	Zone          string       `json:"zone,omitempty"`
	AwayScore     *int         `json:"awayScore,omitempty"`
	HomeScore     *int         `json:"homeScore,omitempty"`
	// Details is the NHL's own details object, verbatim and untrusted.
	Details map[string]any `json:"details,omitempty"`
}

// PlayPlayer is a player the play names, with the role the NHL gave them.
type PlayPlayer struct {
	Role string `json:"role"`
	ID   int64  `json:"id"`
	Name string `json:"name,omitempty"`
}

// Shift is one row of the shift chart.
type Shift struct {
	ID          int64  `json:"id"`
	PlayerID    int64  `json:"playerId"`
	Name        string `json:"name"`
	Team        string `json:"team"`
	Period      int    `json:"period"`
	ShiftNumber int    `json:"shiftNumber"`
	StartTime   string `json:"startTime"`
	EndTime     string `json:"endTime"`
	Duration    string `json:"duration"`
	Seconds     int    `json:"seconds"`
}
