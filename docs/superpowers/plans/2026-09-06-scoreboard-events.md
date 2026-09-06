# Scoreboard Events (clock heartbeat + roster) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add two additive detail-types to the HockeyTrack event contract — a per-poll `nhl.game.clock` heartbeat while a game is live, and a `nhl.game.roster` event when a game's roster is first seen or changes — so downstream consumers (the scoreboard) can render a running clock, shots, situation and sweater numbers without calling the NHL.

**Architecture:** The poller already fetches the play-by-play document every 5 s while live; it currently publishes only on new plays and state changes. This plan parses four more fields the document already carries (`clock`, `situationCode`, per-team `sog`, `rosterSpots`), publishes a `ClockEvent` every live poll, and publishes a `RosterEvent` whenever a hash of the roster changes (stored in the existing `PollerState.SnapshotHashes` map under key `"roster"`, so it survives hand-offs). Nothing existing changes shape.

**Tech Stack:** Go 1.27, stdlib tests with the real fixture `internal/nhl/testdata/pbp.json`, EventBridge via the existing `events.Publisher`.

**Spec:** `/home/jay/projects/hockeytrack-scoreboard/docs/superpowers/specs/2026-09-06-scoreboard-design.md` §6 (and §3.3 for what the consumer does with these).

## Global Constraints

- `events.SchemaVersion` stays `1`; both events are additive.
- Source stays `hockeytrack.poller`; new detail-types are exactly `nhl.game.clock` and `nhl.game.roster`.
- No new AWS calls; tests use fakes and the real fixture only (`make test` runs govulncheck first and must stay green).
- `gofmt`, `go vet ./...` clean. Go is at `~/.local/share/go/bin` (`export PATH="$HOME/.local/share/go/bin:$PATH"`).
- Heartbeat only while `IsLiveState(gameState)` (LIVE or CRIT). Never during PRE/FUT or after FINAL/OFF.
- The play-event path must not change: `TestRunToFinal`'s seq/status/final assertions still hold.
- README "The event contract" section documents both new detail-types.

---

### Task 1: Parse clock, situation, shots and roster from the play-by-play feed

**Files:**
- Modify: `internal/nhl/types.go:95-99` (PBPTeam) and `:146-154` (PlayByPlay)
- Test: `internal/nhl/client_test.go` (append)

**Interfaces:**
- Produces: `nhl.PBPTeam.SOG int` (json `sog`), `nhl.PlayByPlay.Clock nhl.Clock`, `nhl.PlayByPlay.PeriodDescriptor nhl.PeriodDescriptor`, `nhl.PlayByPlay.SituationCode string` (top-level `situationCode`; may be empty pre-game), `nhl.PlayByPlay.RosterSpots []nhl.RosterSpot`.
- `type Clock struct { TimeRemaining string; SecondsRemaining int; Running bool; InIntermission bool }` (json `timeRemaining`, `secondsRemaining`, `running`, `inIntermission`).
- `type RosterSpot struct { TeamID int64; PlayerID int64; SweaterNumber int; PositionCode string }` (json `teamId`, `playerId`, `sweaterNumber`, `positionCode`).

- [ ] **Step 1: Write the failing test**

Append to `internal/nhl/client_test.go`:

```go
func TestPlayByPlayCarriesClockRosterAndShots(t *testing.T) {
	b, err := os.ReadFile("testdata/pbp.json")
	if err != nil {
		t.Fatal(err)
	}
	var p PlayByPlay
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	if p.Clock.TimeRemaining != "00:00" || p.Clock.Running || p.Clock.InIntermission {
		t.Errorf("clock = %+v, want final-state clock 00:00 not running", p.Clock)
	}
	if p.AwayTeam.SOG != 19 || p.HomeTeam.SOG != 37 {
		t.Errorf("sog = %d/%d, want 19/37", p.AwayTeam.SOG, p.HomeTeam.SOG)
	}
	if p.PeriodDescriptor.Number != 3 || p.PeriodDescriptor.PeriodType != "REG" {
		t.Errorf("periodDescriptor = %+v", p.PeriodDescriptor)
	}
	if len(p.RosterSpots) < 36 {
		t.Fatalf("rosterSpots = %d, want a full two-team roster", len(p.RosterSpots))
	}
	var found bool
	for _, r := range p.RosterSpots {
		if r.PlayerID == 8473419 {
			found = r.TeamID == 13 && r.SweaterNumber == 63 && r.PositionCode == "L"
		}
	}
	if !found {
		t.Error("roster spot for 8473419 (FLA #63, L) not parsed")
	}
}
```

Add `"os"` to the test file's imports if it is not already there.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/nhl/ -run TestPlayByPlayCarriesClockRosterAndShots -v`
Expected: FAIL to compile — `p.Clock undefined`, `p.AwayTeam.SOG undefined`, `p.RosterSpots undefined`.

- [ ] **Step 3: Add the fields**

In `internal/nhl/types.go` change `PBPTeam` and `PlayByPlay`, and add the two new types next to `PeriodDescriptor`:

```go
type PBPTeam struct {
	ID     int64  `json:"id"`
	Abbrev string `json:"abbrev"`
	Score  int    `json:"score"`
	SOG    int    `json:"sog"`
}

// Clock is the game clock as the feed reports it at fetch time.
type Clock struct {
	TimeRemaining    string `json:"timeRemaining"`
	SecondsRemaining int    `json:"secondsRemaining"`
	Running          bool   `json:"running"`
	InIntermission   bool   `json:"inIntermission"`
}

// RosterSpot maps a player to a sweater number for one game.
type RosterSpot struct {
	TeamID        int64  `json:"teamId"`
	PlayerID      int64  `json:"playerId"`
	SweaterNumber int    `json:"sweaterNumber"`
	PositionCode  string `json:"positionCode"`
}

type PlayByPlay struct {
	ID               int64            `json:"id"`
	Season           int64            `json:"season"`
	GameDate         string           `json:"gameDate"`
	GameState        string           `json:"gameState"`
	AwayTeam         PBPTeam          `json:"awayTeam"`
	HomeTeam         PBPTeam          `json:"homeTeam"`
	PeriodDescriptor PeriodDescriptor `json:"periodDescriptor"`
	Clock            Clock            `json:"clock"`
	SituationCode    string           `json:"situationCode"`
	RosterSpots      []RosterSpot     `json:"rosterSpots"`
	Plays            []Play           `json:"plays"`
}
```

- [ ] **Step 4: Run the test and the package**

Run: `go test ./internal/nhl/ -v -run TestPlayByPlayCarriesClockRosterAndShots && go test ./...`
Expected: PASS; every package still ok (the added fields are ignored by existing code).

- [ ] **Step 5: Commit**

```bash
git add internal/nhl/types.go internal/nhl/client_test.go
git commit -m "nhl: parse clock, situation code, shots and roster spots from play-by-play"
```

---

### Task 2: Define the ClockEvent and RosterEvent types

**Files:**
- Modify: `internal/events/events.go`
- Test: `internal/events/events_test.go` (create if absent; check with `ls internal/events`)

**Interfaces:**
- Produces: constants `events.DTClock = "nhl.game.clock"`, `events.DTRoster = "nhl.game.roster"`.
- `events.ClockEvent{SchemaVersion int; GameID int64; GameState string; Period int; PeriodType string; SecondsRemaining int; TimeRemaining string; Running bool; InIntermission bool; SituationCode string; HomeTeam, AwayTeam string; Score map[string]int; Shots map[string]int; ObservedAt time.Time}` with json tags `schemaVersion, gameId, gameState, period, periodType, secondsRemaining, timeRemaining, running, inIntermission, situationCode, homeTeam, awayTeam, score, shots, observedAt`.
- `events.RosterEvent{SchemaVersion int; GameID int64; HomeTeam, AwayTeam string; Players []events.RosterPlayer}` with `events.RosterPlayer{PlayerID int64; Team string; Number int; Position string}`; json tags `schemaVersion, gameId, homeTeam, awayTeam, players` and `playerId, team, number, position`.

- [ ] **Step 1: Write the failing test**

Create `internal/events/events_test.go`:

```go
package events

import (
	"encoding/json"
	"testing"
	"time"
)

func TestClockAndRosterEventsSerialise(t *testing.T) {
	if DTClock != "nhl.game.clock" || DTRoster != "nhl.game.roster" {
		t.Fatalf("detail types = %q, %q", DTClock, DTRoster)
	}
	at := time.Date(2026, 10, 1, 23, 47, 12, 0, time.UTC)
	c := ClockEvent{
		SchemaVersion: SchemaVersion, GameID: 2026020001, GameState: "LIVE",
		Period: 2, PeriodType: "REG", SecondsRemaining: 872, TimeRemaining: "14:32",
		Running: true, SituationCode: "1451", HomeTeam: "NYR", AwayTeam: "TBL",
		Score: map[string]int{"TBL": 2, "NYR": 1}, Shots: map[string]int{"TBL": 17, "NYR": 22},
		ObservedAt: at,
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	for _, k := range []string{"schemaVersion", "gameId", "gameState", "period", "periodType", "secondsRemaining", "timeRemaining", "running", "inIntermission", "situationCode", "homeTeam", "awayTeam", "score", "shots", "observedAt"} {
		if _, ok := m[k]; !ok {
			t.Errorf("clock event missing json key %q", k)
		}
	}
	if m["observedAt"] != "2026-10-01T23:47:12Z" {
		t.Errorf("observedAt = %v", m["observedAt"])
	}

	r := RosterEvent{SchemaVersion: SchemaVersion, GameID: 2026020001, HomeTeam: "NYR", AwayTeam: "TBL",
		Players: []RosterPlayer{{PlayerID: 8478010, Team: "TBL", Number: 86, Position: "R"}}}
	b, _ = json.Marshal(r)
	json.Unmarshal(b, &m)
	players := m["players"].([]any)
	p := players[0].(map[string]any)
	if p["playerId"] != float64(8478010) || p["team"] != "TBL" || p["number"] != float64(86) || p["position"] != "R" {
		t.Errorf("roster player json = %v", p)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/events/ -v`
Expected: FAIL to compile — `DTClock`, `ClockEvent`, `RosterEvent`, `RosterPlayer` undefined.

- [ ] **Step 3: Add the constants and types**

In `internal/events/events.go`, add `"time"` to the imports, extend the const block, and add the types after `FinalEvent`:

```go
const (
	SchemaVersion = 1
	Source        = "hockeytrack.poller"
	DTPlay        = "nhl.game.play"
	DTStatus      = "nhl.game.status"
	DTFinal       = "nhl.game.final"
	DTClock       = "nhl.game.clock"
	DTRoster      = "nhl.game.roster"
	DTAlert       = "hockeytrack.alert"
)

// ClockEvent is the per-poll heartbeat while a game is live: the clock,
// period, shots and situation as the feed reported them at ObservedAt.
// Consumers run their own clock from SecondsRemaining/Running/ObservedAt
// between heartbeats.
type ClockEvent struct {
	SchemaVersion    int            `json:"schemaVersion"`
	GameID           int64          `json:"gameId"`
	GameState        string         `json:"gameState"`
	Period           int            `json:"period"`
	PeriodType       string         `json:"periodType"`
	SecondsRemaining int            `json:"secondsRemaining"`
	TimeRemaining    string         `json:"timeRemaining"`
	Running          bool           `json:"running"`
	InIntermission   bool           `json:"inIntermission"`
	SituationCode    string         `json:"situationCode"`
	HomeTeam         string         `json:"homeTeam"`
	AwayTeam         string         `json:"awayTeam"`
	Score            map[string]int `json:"score"`
	Shots            map[string]int `json:"shots"`
	ObservedAt       time.Time      `json:"observedAt"`
}

// RosterPlayer maps a player id to the sweater number worn in this game.
type RosterPlayer struct {
	PlayerID int64  `json:"playerId"`
	Team     string `json:"team"`
	Number   int    `json:"number"`
	Position string `json:"position"`
}

// RosterEvent is published the first time a game's roster is seen and
// whenever it changes, so consumers can print numbers without an NHL call.
type RosterEvent struct {
	SchemaVersion int            `json:"schemaVersion"`
	GameID        int64          `json:"gameId"`
	HomeTeam      string         `json:"homeTeam"`
	AwayTeam      string         `json:"awayTeam"`
	Players       []RosterPlayer `json:"players"`
}
```

- [ ] **Step 4: Run the test**

Run: `go test ./internal/events/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/events/events.go internal/events/events_test.go
git commit -m "events: ClockEvent and RosterEvent detail-types (additive, schema v1)"
```

---

### Task 3: Build the events from a play-by-play document (pure functions)

**Files:**
- Create: `internal/poller/scoreboard.go`
- Test: `internal/poller/scoreboard_test.go`

**Interfaces:**
- Consumes: `nhl.PlayByPlay` fields from Task 1; `events.ClockEvent`, `events.RosterEvent` from Task 2.
- Produces: `func BuildClockEvent(pbp *nhl.PlayByPlay, observedAt time.Time) events.ClockEvent`; `func BuildRosterEvent(pbp *nhl.PlayByPlay) events.RosterEvent` (players sorted by team abbrev then number, deterministic); `func RosterHash(pbp *nhl.PlayByPlay) string` (sha256 hex of the sorted `teamId:playerId:number` triples; empty string when there are no roster spots).

- [ ] **Step 1: Write the failing tests**

Create `internal/poller/scoreboard_test.go`:

```go
package poller

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"hockeytrack/internal/nhl"
)

func fixturePBP(t *testing.T) *nhl.PlayByPlay {
	t.Helper()
	b, err := os.ReadFile("testdata/pbp.json")
	if err != nil {
		t.Fatal(err)
	}
	var p nhl.PlayByPlay
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

func TestBuildClockEvent(t *testing.T) {
	p := fixturePBP(t)
	p.GameState = "LIVE"
	p.Clock = nhl.Clock{TimeRemaining: "14:32", SecondsRemaining: 872, Running: true}
	p.SituationCode = "1451"
	at := time.Date(2025, 10, 7, 21, 30, 0, 0, time.UTC)

	e := BuildClockEvent(p, at)
	if e.GameID != 2025020001 || e.GameState != "LIVE" || e.Period != 3 || e.PeriodType != "REG" {
		t.Errorf("header = %+v", e)
	}
	if e.SecondsRemaining != 872 || e.TimeRemaining != "14:32" || !e.Running || e.InIntermission {
		t.Errorf("clock = %+v", e)
	}
	if e.SituationCode != "1451" || e.HomeTeam != "FLA" || e.AwayTeam != "CHI" {
		t.Errorf("teams/situation = %+v", e)
	}
	if e.Score["CHI"] != 2 || e.Score["FLA"] != 3 || e.Shots["CHI"] != 19 || e.Shots["FLA"] != 37 {
		t.Errorf("score/shots = %v / %v", e.Score, e.Shots)
	}
	if !e.ObservedAt.Equal(at) || e.SchemaVersion != 1 {
		t.Errorf("observedAt/schema = %v / %d", e.ObservedAt, e.SchemaVersion)
	}
}

func TestBuildRosterEventIsSortedAndComplete(t *testing.T) {
	p := fixturePBP(t)
	e := BuildRosterEvent(p)
	if e.GameID != 2025020001 || e.HomeTeam != "FLA" || e.AwayTeam != "CHI" {
		t.Errorf("header = %+v", e)
	}
	if len(e.Players) != len(p.RosterSpots) {
		t.Fatalf("players = %d, want %d", len(e.Players), len(p.RosterSpots))
	}
	for i := 1; i < len(e.Players); i++ {
		a, b := e.Players[i-1], e.Players[i]
		if a.Team > b.Team || (a.Team == b.Team && a.Number > b.Number) {
			t.Fatalf("players not sorted at %d: %+v then %+v", i, a, b)
		}
	}
	var marchand bool
	for _, pl := range e.Players {
		if pl.PlayerID == 8473419 {
			marchand = pl.Team == "FLA" && pl.Number == 63 && pl.Position == "L"
		}
	}
	if !marchand {
		t.Error("8473419 should be FLA #63 L")
	}
}

func TestRosterHashChangesWithRoster(t *testing.T) {
	p := fixturePBP(t)
	h1 := RosterHash(p)
	if h1 == "" || len(h1) != 64 {
		t.Fatalf("hash = %q", h1)
	}
	if RosterHash(p) != h1 {
		t.Error("hash not deterministic")
	}
	p.RosterSpots[0].SweaterNumber++
	if RosterHash(p) == h1 {
		t.Error("hash did not change when a number changed")
	}
	p.RosterSpots = nil
	if RosterHash(p) != "" {
		t.Error("empty roster must hash to empty string")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/poller/ -run 'TestBuildClockEvent|TestBuildRosterEvent|TestRosterHash' -v`
Expected: FAIL to compile — `BuildClockEvent`, `BuildRosterEvent`, `RosterHash` undefined.

- [ ] **Step 3: Implement**

Create `internal/poller/scoreboard.go`:

```go
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
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/poller/ -v -run 'TestBuildClockEvent|TestBuildRosterEvent|TestRosterHash'`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/poller/scoreboard.go internal/poller/scoreboard_test.go
git commit -m "poller: build clock heartbeat and roster events from play-by-play"
```

---

### Task 4: Publish the heartbeat every live poll and the roster on change

**Files:**
- Modify: `internal/poller/poller.go:141-159` (after the status publish, before the plays loop)
- Test: `internal/poller/poller_test.go` (append)

**Interfaces:**
- Consumes: Task 3 functions; `state.SnapshotHashes` (existing `map[string]string` on `store.PollerState`, persisted by `UpdatePollerState`, so the roster hash survives chain hand-offs).
- Produces: on every poll where `IsLiveState(pbp.GameState)`, one `events.DTClock` publish; whenever `RosterHash(pbp) != state.SnapshotHashes["roster"]` and the hash is non-empty, one `events.DTRoster` publish, then the hash is stored. Roster is published regardless of game state (rosters appear pre-game), clock only while live.

- [ ] **Step 1: Write the failing test**

Append to `internal/poller/poller_test.go`:

```go
func TestRunPublishesClockHeartbeatAndRosterOnce(t *testing.T) {
	// Three live polls with the same roster, then final: expect a clock event
	// per live poll and exactly one roster event.
	feed := &scriptedFeed{snapshots: [][]byte{
		truncatedSnapshot(t, 5, "LIVE"),
		truncatedSnapshot(t, 10, "LIVE"),
		truncatedSnapshot(t, 15, "LIVE"),
		truncatedSnapshot(t, 1<<30, "OFF"),
	}}
	d, gs, _, pub := testDeps(feed)
	seedGame(t, gs)

	if _, err := Run(context.Background(), d, DefaultConfig(), 2025020001, "link1", func() bool { return false }); err != nil {
		t.Fatal(err)
	}
	var clocks, rosters int
	for _, e := range pub.Published {
		switch e.DetailType {
		case events.DTClock:
			clocks++
			c := e.Detail.(events.ClockEvent)
			if c.GameID != 2025020001 || c.Shots["FLA"] != 37 || c.ObservedAt.IsZero() {
				t.Errorf("clock event = %+v", c)
			}
			if !IsLiveState(c.GameState) {
				t.Errorf("clock heartbeat published in state %q", c.GameState)
			}
		case events.DTRoster:
			rosters++
			r := e.Detail.(events.RosterEvent)
			if len(r.Players) == 0 || r.HomeTeam != "FLA" {
				t.Errorf("roster event = %+v", r)
			}
		}
	}
	if clocks != 3 {
		t.Errorf("clock events = %d, want 3 (one per LIVE poll, none for OFF)", clocks)
	}
	if rosters != 1 {
		t.Errorf("roster events = %d, want 1 (roster unchanged across polls)", rosters)
	}
	rec, _ := gs.Get(context.Background(), 2025020001)
	if rec.SnapshotHashes["roster"] == "" {
		t.Error("roster hash not persisted in poller state")
	}
}

func TestRunRepublishesRosterWhenItChanges(t *testing.T) {
	first := truncatedSnapshot(t, 5, "LIVE")
	// Change one sweater number in the second snapshot.
	var m map[string]any
	json.Unmarshal(first, &m)
	spots := m["rosterSpots"].([]any)
	spots[0].(map[string]any)["sweaterNumber"] = float64(99)
	second, _ := json.Marshal(m)
	feed := &scriptedFeed{snapshots: [][]byte{first, second, truncatedSnapshot(t, 1<<30, "OFF")}}
	d, gs, _, pub := testDeps(feed)
	seedGame(t, gs)

	if _, err := Run(context.Background(), d, DefaultConfig(), 2025020001, "link1", func() bool { return false }); err != nil {
		t.Fatal(err)
	}
	var rosters int
	for _, e := range pub.Published {
		if e.DetailType == events.DTRoster {
			rosters++
		}
	}
	if rosters != 2 {
		t.Errorf("roster events = %d, want 2 (initial + changed)", rosters)
	}
}
```

If `testDeps` returns a `GameStore` whose `Get` does not expose `SnapshotHashes`, read `store.PollerState` the way `TestRunToFinal` reads `rec.Done` (the record returned by `gs.Get` carries the poller state fields in this repo; check `internal/store/store.go` `GameRecord`).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/poller/ -run 'TestRunPublishesClockHeartbeatAndRosterOnce|TestRunRepublishesRosterWhenItChanges' -v`
Expected: FAIL — `clock events = 0, want 3`, `roster events = 0, want 1` (and 0 want 2).

- [ ] **Step 3: Publish in the poll loop**

In `internal/poller/poller.go`, immediately after the `pbp.GameState != state.GameState` status-publish block (currently ending around line 150) and before `running := score`, insert:

```go
		// Roster: publish when first seen and whenever it changes. The hash
		// rides in SnapshotHashes so a hand-off does not republish it.
		if rh := RosterHash(pbp); rh != "" && rh != state.SnapshotHashes["roster"] {
			if err := d.Pub.Publish(ctx, events.DTRoster, BuildRosterEvent(pbp)); err != nil {
				slog.Warn("roster publish failed; will retry next cycle", "gameId", gameID, "err", err)
			} else {
				state.SnapshotHashes["roster"] = rh
			}
		}

		// Clock heartbeat: one per poll while the game is live, so consumers
		// can run a local clock between samples.
		if IsLiveState(pbp.GameState) {
			if err := d.Pub.Publish(ctx, events.DTClock, BuildClockEvent(pbp, d.Now())); err != nil {
				slog.Warn("clock publish failed", "gameId", gameID, "err", err)
			}
		}
```

Check that `state.SnapshotHashes` is never nil at this point: find where `state` is initialised at the top of `Run` (search for `SnapshotHashes: map[string]string{}` or the load from the store); if a loaded state can have a nil map, add `if state.SnapshotHashes == nil { state.SnapshotHashes = map[string]string{} }` right after it is loaded.

- [ ] **Step 4: Run the whole poller package**

Run: `go test ./internal/poller/ -v`
Expected: PASS for the two new tests and every existing one (`TestRunToFinal` still sees 1 final, ≥2 status, ordered seqs).

- [ ] **Step 5: Run everything**

Run: `export PATH="$HOME/.local/share/go/bin:$PATH" && gofmt -l . && go vet ./... && make test`
Expected: gofmt prints nothing; vet clean; "No vulnerabilities found."; all packages ok.

- [ ] **Step 6: Commit**

```bash
git add internal/poller/poller.go internal/poller/poller_test.go
git commit -m "poller: publish nhl.game.clock every live poll and nhl.game.roster on change"
```

---

### Task 5: Replay harness prints the new events; README documents the contract

**Files:**
- Modify: `cmd/replay/main.go` (only if it switches on detail-type; check with `grep -n DTPlay cmd/replay/main.go`)
- Modify: `README.md` "The event contract" section (the bullet list under `Bus \`hockeytrack\`, source \`hockeytrack.poller\``)

- [ ] **Step 1: Make replay show the new events**

Run `grep -n "DTPlay\|DTStatus\|DetailType" cmd/replay/main.go`. If the harness prints every published event generically (no switch), nothing to do. If it switches on detail-type, add cases so `nhl.game.clock` prints `clock <period> <timeRemaining> running=<bool> sit=<code> shots=<map>` and `nhl.game.roster` prints `roster <n> players`. Run it on a captured game directory to eyeball the output:

```bash
go run ./cmd/replay -game internal/poller/testdata/ 2>/dev/null | head -20
```

(If `-game` expects a snapshot directory and none is checked in, skip the eyeball step; the unit tests in Task 4 cover the behaviour.)

- [ ] **Step 2: Document the two detail-types**

In `README.md`, change the sentence `Three detail-types (plus \`hockeytrack.alert\` for operational alerts):` to `Five detail-types (plus \`hockeytrack.alert\` for operational alerts):` and add after the `nhl.game.final` bullet:

```markdown
- **`nhl.game.clock`** — a heartbeat on every poll while the game is live (about every 5 s): `gameState`, `period`/`periodType`, `secondsRemaining`/`timeRemaining`/`running`/`inIntermission`, the four-digit `situationCode` (away goalie, away skaters, home skaters, home goalie), both teams' `score` and `shots`, and `observedAt`. Consumers that show a clock should count down locally from `secondsRemaining` while `running` is true and re-sync on each heartbeat.
- **`nhl.game.roster`** — published when a game's roster is first seen and again if it changes: `players[]` with `playerId`, `team`, `number` and `position`, so a consumer can print "#86" without calling the NHL.
```

Also add one sentence to the "Extending it" list: `- **A physical scoreboard** — the [hockeytrack-scoreboard](https://github.com/DavidJDrake/hockeytrack-scoreboard) project consumes \`nhl.game.clock\`, \`nhl.game.play\` and \`nhl.game.roster\` to drive an LED bar display over MQTT.` (Leave the link in even before the repo is public; it is the intended home.)

- [ ] **Step 3: Verify and commit**

Run: `make test`
Expected: green.

```bash
git add README.md cmd/replay/main.go
git commit -m "docs: document nhl.game.clock and nhl.game.roster; replay prints them"
```

---

### Task 6: Deploy and verify on the bus

**Files:** none (operator steps; the orchestrator runs these in-session because `terraform apply` needs the user's permission context).

- [ ] **Step 1: Deploy**

```bash
export PATH="$HOME/.local/share/go/bin:$PATH"
export XDG_RUNTIME_DIR=$HOME/.cache/xdg-runtime && mkdir -p "$XDG_RUNTIME_DIR"
make deploy
```

Expected: image tagged with the new git SHA on all three Lambdas; `Apply complete` with only the three `aws_lambda_function` resources changed.

- [ ] **Step 2: Verify with the replay harness against the deployed contract**

No live games exist before 2026-09-19, so verification is by unit test plus a one-off invoke of the poller against a finished game, which publishes the roster (state OFF → no clock heartbeat, by design):

```bash
aws lambda invoke --function-name hockeytrack-poller --region us-east-1 --cli-binary-format raw-in-base64-out \
  --payload '{"gameId": 2025020001}' /dev/stdout
```

Expected: the poller reports `OutcomeAlreadyDone` or `OutcomeNotScheduled` (that game is not in this season's table). That is fine — the point of the invoke is that the new image starts cleanly. The real verification is the first preseason game (HOC-33): confirm with

```bash
aws logs filter-log-events --log-group-name /aws/lambda/hockeytrack-poller --region us-east-1 \
  --start-time $(( $(date +%s) * 1000 - 3600000 )) --filter-pattern '"clock publish failed"' --query 'length(events)'
```

Expected: 0, and an EventBridge rule on `nhl.game.clock` (the scoreboard reducer) receiving ~12 events/minute per live game.

- [ ] **Step 3: Close the ticket**

Comment on the HOC ticket with the deployed SHA and move it to Done.
