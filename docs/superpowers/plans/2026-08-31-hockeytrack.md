# HockeyTrack Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the NHL game-data ingestion pipeline: a single Go container image running as three Lambda modes (schedule-sync, game-poller, sweeper) that archives raw NHL API data to S3 and publishes discrete game events to an EventBridge bus, with DynamoDB bookkeeping and Terraform-managed infrastructure.

**Architecture:** One Go binary, `MODE` env var selects behavior. schedule-sync (daily cron) records games in DynamoDB and creates one-time EventBridge Scheduler entries per game. game-poller (fired at puck drop − 15m) polls at 5s, diffs play-by-play against a DynamoDB high-water mark, publishes events, snapshots changed raw JSON to S3, and self-chains past the 15-minute Lambda limit. sweeper (5-minute rate rule) restarts dead poller chains via lease expiry.

**Tech Stack:** Go 1.23+, aws-sdk-go-v2, aws-lambda-go, Docker (distroless), Terraform (AWS provider ~> 5.x).

**Spec:** `docs/superpowers/specs/2026-08-31-hockeytrack-design.md`

## Global Constraints

- Go module name: `hockeytrack`. Standard library `testing` only — no test frameworks.
- All AWS access through aws-sdk-go-v2; all AWS adapters behind interfaces defined in `internal/store`, `internal/events`, `internal/schedsync`, `internal/sweeper`; logic tests use in-memory fakes, never AWS.
- NHL API bases: `https://api-web.nhle.com` (main), `https://api.nhle.com` (shift charts). Confirmed live endpoints: `/v1/schedule/{YYYY-MM-DD}`, `/v1/gamecenter/{gameId}/play-by-play`, `/v1/gamecenter/{gameId}/boxscore`, `/v1/gamecenter/{gameId}/landing`, `https://api.nhle.com/stats/rest/en/shiftcharts?cayenneExp=gameId={gameId}`.
- Game states (confirmed values): `FUT`, `PRE` (pregame), `LIVE`, `CRIT` (live, late close game), `FINAL`, `OFF` (official final). Final means `FINAL` or `OFF`. `gameScheduleState` of `OK` means as scheduled; anything else (`PPD`, `SUSP`, `CNCL`) means not running as scheduled.
- Event contract: EventBridge source `hockeytrack.poller`; detail-types `nhl.game.play`, `nhl.game.status`, `nhl.game.final`, plus `hockeytrack.alert` for operational alerts. Every detail carries `schemaVersion: 1`. Delivery is at-least-once; consumers dedupe on `(gameId, seq)`.
- Defaults (all overridable via env): poll interval 5s live, 30s pregame; lease TTL 60s; max chain links 30; Lambda handoff buffer 60s before deadline.
- Fixtures are captured from the real NHL API into `testdata/` directories with curl (completed game `2025020001` works year-round). Never hand-write fixtures.
- S3 key layout (season is the NHL's native format, e.g. `20252026`):
  - `raw/{season}/{gameDate}/{gameId}/{feed}/{ts}.json` (ts = `20060102T150405Z` format)
  - `raw/{season}/{gameDate}/{gameId}/final/{feed}.json`
  - `raw/schedule/{date}.json`
- Terraform: all resources prefixed `hockeytrack`; single environment; region via variable (default `us-east-1`).
- Commit after every green test cycle. Conventional-commit style messages (`feat:`, `test:`, `chore:`, `infra:`).

---

### Task 1: Scaffolding + NHL client (schedule endpoint)

**Files:**
- Create: `go.mod`, `.gitignore`
- Create: `internal/nhl/types.go`, `internal/nhl/client.go`
- Create: `internal/nhl/testdata/schedule.json` (captured from real API)
- Test: `internal/nhl/client_test.go`

**Interfaces:**
- Consumes: nothing (first task).
- Produces: `nhl.New() *Client`, `Client.Schedule(ctx, date string) (*ScheduleResponse, []byte, error)` returning parsed struct AND raw body; types `ScheduleResponse{GameWeek []ScheduleDay}`, `ScheduleDay{Date string; Games []ScheduleGame}`, `ScheduleGame{ID, Season int64; GameType int; StartTimeUTC time.Time; GameState, GameScheduleState string; Venue Localized; AwayTeam, HomeTeam ScheduleTeam}`, `ScheduleTeam{ID int64; Abbrev string}`, `Localized{Default string}`. Client has settable `BaseURL`, `StatsBaseURL`, `HTTP *http.Client` for test injection.

- [ ] **Step 1: Initialize module and capture fixture**

```bash
cd /home/jay/projects/hockeytrack
go mod init hockeytrack
printf '*.test\n/dist/\n.terraform/\n*.tfstate*\n' > .gitignore
mkdir -p internal/nhl/testdata
curl -s "https://api-web.nhle.com/v1/schedule/2026-01-15" -o internal/nhl/testdata/schedule.json
python3 -c "import json; d=json.load(open('internal/nhl/testdata/schedule.json')); print(d['gameWeek'][0]['date'], len(d['gameWeek'][0]['games']))"
```
Expected: `2026-01-15 10`

- [ ] **Step 2: Write the failing test**

`internal/nhl/client_test.go`:
```go
package nhl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// serveFixture returns a test server that serves the named testdata file
// for any request path, and a client pointed at it.
func fixtureClient(t *testing.T, files map[string]string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := files[r.URL.Path]
		if !ok {
			t.Errorf("unexpected request path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		b, err := os.ReadFile(filepath.Join("testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	c := New()
	c.BaseURL = srv.URL
	c.StatsBaseURL = srv.URL
	return c
}

func TestSchedule(t *testing.T) {
	c := fixtureClient(t, map[string]string{"/v1/schedule/2026-01-15": "schedule.json"})
	sched, raw, err := c.Schedule(context.Background(), "2026-01-15")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Error("raw body is empty")
	}
	if len(sched.GameWeek) == 0 {
		t.Fatal("no gameWeek days")
	}
	day := sched.GameWeek[0]
	if day.Date != "2026-01-15" {
		t.Errorf("date = %q, want 2026-01-15", day.Date)
	}
	g := day.Games[0]
	if g.ID != 2025020740 {
		t.Errorf("game id = %d, want 2025020740", g.ID)
	}
	if g.HomeTeam.Abbrev != "BUF" || g.AwayTeam.Abbrev != "MTL" {
		t.Errorf("teams = %s@%s, want MTL@BUF", g.AwayTeam.Abbrev, g.HomeTeam.Abbrev)
	}
	if g.StartTimeUTC.IsZero() {
		t.Error("startTimeUTC not parsed")
	}
	if g.GameScheduleState != "OK" {
		t.Errorf("gameScheduleState = %q, want OK", g.GameScheduleState)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/nhl/ -run TestSchedule -v`
Expected: compile FAIL (`undefined: New`, `undefined: Client`)

- [ ] **Step 4: Write implementation**

`internal/nhl/types.go`:
```go
package nhl

import "time"

type Localized struct {
	Default string `json:"default"`
}

type ScheduleTeam struct {
	ID     int64  `json:"id"`
	Abbrev string `json:"abbrev"`
}

type ScheduleGame struct {
	ID                int64        `json:"id"`
	Season            int64        `json:"season"`
	GameType          int          `json:"gameType"`
	StartTimeUTC      time.Time    `json:"startTimeUTC"`
	GameState         string       `json:"gameState"`
	GameScheduleState string       `json:"gameScheduleState"`
	Venue             Localized    `json:"venue"`
	AwayTeam          ScheduleTeam `json:"awayTeam"`
	HomeTeam          ScheduleTeam `json:"homeTeam"`
}

type ScheduleDay struct {
	Date  string         `json:"date"`
	Games []ScheduleGame `json:"games"`
}

type ScheduleResponse struct {
	GameWeek []ScheduleDay `json:"gameWeek"`
}
```

`internal/nhl/client.go`:
```go
package nhl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	DefaultBaseURL      = "https://api-web.nhle.com"
	DefaultStatsBaseURL = "https://api.nhle.com"
)

type Client struct {
	BaseURL      string
	StatsBaseURL string
	HTTP         *http.Client
}

func New() *Client {
	return &Client{
		BaseURL:      DefaultBaseURL,
		StatsBaseURL: DefaultStatsBaseURL,
		HTTP:         &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *Client) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func (c *Client) Schedule(ctx context.Context, date string) (*ScheduleResponse, []byte, error) {
	raw, err := c.get(ctx, fmt.Sprintf("%s/v1/schedule/%s", c.BaseURL, date))
	if err != nil {
		return nil, nil, err
	}
	var s ScheduleResponse
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, nil, fmt.Errorf("parse schedule: %w", err)
	}
	return &s, raw, nil
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/nhl/ -run TestSchedule -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add go.mod .gitignore internal/nhl/
git commit -m "feat: NHL API client with schedule endpoint"
```

---

### Task 2: NHL client — gamecenter feeds

**Files:**
- Modify: `internal/nhl/types.go`, `internal/nhl/client.go`
- Create: `internal/nhl/testdata/pbp.json`, `internal/nhl/testdata/boxscore.json` (captured from real API)
- Test: `internal/nhl/client_test.go` (append)

**Interfaces:**
- Consumes: Task 1 `Client`, `fixtureClient` helper.
- Produces:
  - `Client.PlayByPlay(ctx, gameID int64) (*PlayByPlay, []byte, error)`
  - `Client.RawFeed(ctx, gameID int64, feed string) ([]byte, error)` — feed is `"boxscore"` or `"landing"`, returns unparsed body
  - `Client.ShiftCharts(ctx, gameID int64) ([]byte, error)` — stats API, unparsed
  - Types: `PlayByPlay{ID, Season int64; GameDate, GameState string; AwayTeam, HomeTeam PBPTeam; Plays []Play}`, `PBPTeam{ID int64; Abbrev string; Score int}`, `Play{EventID, SortOrder int64; TypeCode int; TypeDescKey, TimeInPeriod string; PeriodDescriptor PeriodDescriptor; Details json.RawMessage; Raw json.RawMessage}` (Raw = original bytes of the whole play object, populated by custom `UnmarshalJSON`), `PeriodDescriptor{Number int; PeriodType string}`, `PlayDetails{EventOwnerTeamID int64; AwayScore, HomeScore *int}` with helper `p.ParsedDetails() PlayDetails`.

- [ ] **Step 1: Capture fixtures**

```bash
curl -s "https://api-web.nhle.com/v1/gamecenter/2025020001/play-by-play" -o internal/nhl/testdata/pbp.json
curl -s "https://api-web.nhle.com/v1/gamecenter/2025020001/boxscore" -o internal/nhl/testdata/boxscore.json
python3 -c "import json; d=json.load(open('internal/nhl/testdata/pbp.json')); print(d['gameState'], len(d['plays']))"
```
Expected: `OFF` and a play count in the hundreds (~350).

- [ ] **Step 2: Write the failing tests** (append to `internal/nhl/client_test.go`)

```go
func TestPlayByPlay(t *testing.T) {
	c := fixtureClient(t, map[string]string{"/v1/gamecenter/2025020001/play-by-play": "pbp.json"})
	pbp, raw, err := c.PlayByPlay(context.Background(), 2025020001)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Error("raw body is empty")
	}
	if pbp.ID != 2025020001 || pbp.GameState != "OFF" {
		t.Errorf("id=%d state=%q, want 2025020001 OFF", pbp.ID, pbp.GameState)
	}
	if pbp.HomeTeam.Abbrev != "FLA" || pbp.AwayTeam.Abbrev != "CHI" {
		t.Errorf("teams = %s@%s, want CHI@FLA", pbp.AwayTeam.Abbrev, pbp.HomeTeam.Abbrev)
	}
	if len(pbp.Plays) < 100 {
		t.Fatalf("only %d plays parsed", len(pbp.Plays))
	}
	// Find the first goal and check details parsing + raw retention.
	var goal *Play
	for i := range pbp.Plays {
		if pbp.Plays[i].TypeDescKey == "goal" {
			goal = &pbp.Plays[i]
			break
		}
	}
	if goal == nil {
		t.Fatal("no goal play found in fixture")
	}
	if goal.SortOrder == 0 {
		t.Error("goal sortOrder not parsed")
	}
	d := goal.ParsedDetails()
	if d.EventOwnerTeamID != 16 { // CHI scored first in this game
		t.Errorf("eventOwnerTeamId = %d, want 16", d.EventOwnerTeamID)
	}
	if d.AwayScore == nil || *d.AwayScore != 1 {
		t.Error("awayScore not parsed as 1")
	}
	if len(goal.Raw) == 0 {
		t.Error("play Raw not retained")
	}
}

func TestRawFeedAndShifts(t *testing.T) {
	c := fixtureClient(t, map[string]string{
		"/v1/gamecenter/2025020001/boxscore": "boxscore.json",
		"/stats/rest/en/shiftcharts":         "boxscore.json", // any JSON body works; we only check passthrough
	})
	box, err := c.RawFeed(context.Background(), 2025020001, "boxscore")
	if err != nil {
		t.Fatal(err)
	}
	if len(box) == 0 {
		t.Error("boxscore body empty")
	}
	shifts, err := c.ShiftCharts(context.Background(), 2025020001)
	if err != nil {
		t.Fatal(err)
	}
	if len(shifts) == 0 {
		t.Error("shifts body empty")
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/nhl/ -v`
Expected: compile FAIL (`undefined: Play`, `c.PlayByPlay undefined`)

- [ ] **Step 4: Write implementation**

Append to `internal/nhl/types.go`:
```go
import "encoding/json" // add to imports

type PBPTeam struct {
	ID     int64  `json:"id"`
	Abbrev string `json:"abbrev"`
	Score  int    `json:"score"`
}

type PeriodDescriptor struct {
	Number     int    `json:"number"`
	PeriodType string `json:"periodType"`
}

type Play struct {
	EventID          int64            `json:"eventId"`
	SortOrder        int64            `json:"sortOrder"`
	TypeCode         int              `json:"typeCode"`
	TypeDescKey      string           `json:"typeDescKey"`
	TimeInPeriod     string           `json:"timeInPeriod"`
	PeriodDescriptor PeriodDescriptor `json:"periodDescriptor"`
	Details          json.RawMessage  `json:"details"`
	Raw              json.RawMessage  `json:"-"`
}

// UnmarshalJSON retains the original bytes of the play in Raw so events can
// carry the full untouched play object.
func (p *Play) UnmarshalJSON(b []byte) error {
	type alias Play
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*p = Play(a)
	p.Raw = append([]byte(nil), b...)
	return nil
}

type PlayDetails struct {
	EventOwnerTeamID int64 `json:"eventOwnerTeamId"`
	AwayScore        *int  `json:"awayScore"`
	HomeScore        *int  `json:"homeScore"`
}

// ParsedDetails extracts the commonly needed detail fields; a play with no
// details (e.g. period-start) returns the zero value.
func (p *Play) ParsedDetails() PlayDetails {
	var d PlayDetails
	if len(p.Details) > 0 {
		_ = json.Unmarshal(p.Details, &d)
	}
	return d
}

type PlayByPlay struct {
	ID        int64   `json:"id"`
	Season    int64   `json:"season"`
	GameDate  string  `json:"gameDate"`
	GameState string  `json:"gameState"`
	AwayTeam  PBPTeam `json:"awayTeam"`
	HomeTeam  PBPTeam `json:"homeTeam"`
	Plays     []Play  `json:"plays"`
}
```

Append to `internal/nhl/client.go`:
```go
func (c *Client) PlayByPlay(ctx context.Context, gameID int64) (*PlayByPlay, []byte, error) {
	raw, err := c.get(ctx, fmt.Sprintf("%s/v1/gamecenter/%d/play-by-play", c.BaseURL, gameID))
	if err != nil {
		return nil, nil, err
	}
	var p PlayByPlay
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, nil, fmt.Errorf("parse play-by-play: %w", err)
	}
	return &p, raw, nil
}

func (c *Client) RawFeed(ctx context.Context, gameID int64, feed string) ([]byte, error) {
	return c.get(ctx, fmt.Sprintf("%s/v1/gamecenter/%d/%s", c.BaseURL, gameID, feed))
}

func (c *Client) ShiftCharts(ctx context.Context, gameID int64) ([]byte, error) {
	return c.get(ctx, fmt.Sprintf("%s/stats/rest/en/shiftcharts?cayenneExp=gameId=%d", c.StatsBaseURL, gameID))
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/nhl/ -v`
Expected: PASS (all three tests)

- [ ] **Step 6: Commit**

```bash
git add internal/nhl/
git commit -m "feat: NHL client gamecenter feeds with raw play retention"
```

---

### Task 3: Events package — contract types + publisher

**Files:**
- Create: `internal/events/events.go`, `internal/events/eventbridge.go`
- Test: `internal/events/events_test.go`

**Interfaces:**
- Consumes: nothing from other packages (pure types + AWS adapter).
- Produces:
  - Constants: `SchemaVersion = 1`, `Source = "hockeytrack.poller"`, `DTPlay = "nhl.game.play"`, `DTStatus = "nhl.game.status"`, `DTFinal = "nhl.game.final"`, `DTAlert = "hockeytrack.alert"`
  - `PlayEvent{SchemaVersion int; GameID int64 "json:\"gameId\""; Seq int64 "json:\"seq\""; PlayType string "json:\"playType\""; HomeTeam, AwayTeam string; ActingTeam string "json:\"actingTeam,omitempty\""; ScoringTeam string "json:\"scoringTeam,omitempty\""; Period int; TimeInPeriod string; Score map[string]int "json:\"score\""; Raw json.RawMessage "json:\"raw\""}`
  - `StatusEvent{SchemaVersion int; GameID int64; PrevState, GameState string; Score map[string]int}`
  - `FinalEvent{SchemaVersion int; GameID int64; HomeTeam, AwayTeam string; Score map[string]int; S3Prefix string}`
  - `AlertEvent{SchemaVersion int; GameID int64; Reason string}`
  - `Publisher` interface: `Publish(ctx context.Context, detailType string, detail any) error`
  - `NewEventBridgePublisher(client *eventbridge.Client, busName string) *EventBridgePublisher` implementing `Publisher`
  - `FakePublisher{Published []PublishedEvent}` (in `events.go`, exported for reuse by poller tests) where `PublishedEvent{DetailType string; Detail any}`

- [ ] **Step 1: Write the failing test**

`internal/events/events_test.go`:
```go
package events

import (
	"context"
	"encoding/json"
	"testing"
)

func TestPlayEventJSONShape(t *testing.T) {
	e := PlayEvent{
		SchemaVersion: SchemaVersion,
		GameID:        2026020123,
		Seq:           166,
		PlayType:      "goal",
		HomeTeam:      "TBL",
		AwayTeam:      "BOS",
		ActingTeam:    "TBL",
		ScoringTeam:   "TBL",
		Period:        2,
		TimeInPeriod:  "08:41",
		Score:         map[string]int{"TBL": 2, "BOS": 1},
		Raw:           json.RawMessage(`{"eventId":258}`),
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	for _, k := range []string{"schemaVersion", "gameId", "seq", "playType", "homeTeam", "awayTeam", "scoringTeam", "period", "timeInPeriod", "score", "raw"} {
		if _, ok := m[k]; !ok {
			t.Errorf("marshaled event missing key %q", k)
		}
	}
	if m["schemaVersion"].(float64) != 1 {
		t.Errorf("schemaVersion = %v, want 1", m["schemaVersion"])
	}
}

func TestNonScoringPlayOmitsScoringTeam(t *testing.T) {
	e := PlayEvent{SchemaVersion: 1, GameID: 1, Seq: 8, PlayType: "faceoff"}
	b, _ := json.Marshal(e)
	var m map[string]any
	json.Unmarshal(b, &m)
	if _, ok := m["scoringTeam"]; ok {
		t.Error("scoringTeam should be omitted when empty")
	}
}

func TestFakePublisherRecords(t *testing.T) {
	f := &FakePublisher{}
	if err := f.Publish(context.Background(), DTStatus, StatusEvent{SchemaVersion: 1, GameID: 5, GameState: "LIVE"}); err != nil {
		t.Fatal(err)
	}
	if len(f.Published) != 1 || f.Published[0].DetailType != DTStatus {
		t.Fatalf("published = %+v", f.Published)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/events/ -v`
Expected: compile FAIL (`undefined: PlayEvent` etc.)

- [ ] **Step 3: Write implementation**

`internal/events/events.go`:
```go
// Package events defines the versioned EventBridge event contract.
// Consumers dedupe on (gameId, seq); delivery is at-least-once.
package events

import (
	"context"
	"encoding/json"
)

const (
	SchemaVersion = 1
	Source        = "hockeytrack.poller"
	DTPlay        = "nhl.game.play"
	DTStatus      = "nhl.game.status"
	DTFinal       = "nhl.game.final"
	DTAlert       = "hockeytrack.alert"
)

type PlayEvent struct {
	SchemaVersion int             `json:"schemaVersion"`
	GameID        int64           `json:"gameId"`
	Seq           int64           `json:"seq"`
	PlayType      string          `json:"playType"`
	HomeTeam      string          `json:"homeTeam"`
	AwayTeam      string          `json:"awayTeam"`
	ActingTeam    string          `json:"actingTeam,omitempty"`
	ScoringTeam   string          `json:"scoringTeam,omitempty"`
	Period        int             `json:"period"`
	TimeInPeriod  string          `json:"timeInPeriod"`
	Score         map[string]int  `json:"score"`
	Raw           json.RawMessage `json:"raw"`
}

type StatusEvent struct {
	SchemaVersion int            `json:"schemaVersion"`
	GameID        int64          `json:"gameId"`
	PrevState     string         `json:"prevState"`
	GameState     string         `json:"gameState"`
	Score         map[string]int `json:"score"`
}

type FinalEvent struct {
	SchemaVersion int            `json:"schemaVersion"`
	GameID        int64          `json:"gameId"`
	HomeTeam      string         `json:"homeTeam"`
	AwayTeam      string         `json:"awayTeam"`
	Score         map[string]int `json:"score"`
	S3Prefix      string         `json:"s3Prefix"`
}

type AlertEvent struct {
	SchemaVersion int    `json:"schemaVersion"`
	GameID        int64  `json:"gameId"`
	Reason        string `json:"reason"`
}

type Publisher interface {
	Publish(ctx context.Context, detailType string, detail any) error
}

type PublishedEvent struct {
	DetailType string
	Detail     any
}

// FakePublisher records events in memory for tests.
type FakePublisher struct {
	Published []PublishedEvent
	Err       error
}

func (f *FakePublisher) Publish(_ context.Context, detailType string, detail any) error {
	if f.Err != nil {
		return f.Err
	}
	f.Published = append(f.Published, PublishedEvent{DetailType: detailType, Detail: detail})
	return nil
}
```

`internal/events/eventbridge.go`:
```go
package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
)

type EventBridgePublisher struct {
	client  *eventbridge.Client
	busName string
}

func NewEventBridgePublisher(client *eventbridge.Client, busName string) *EventBridgePublisher {
	return &EventBridgePublisher{client: client, busName: busName}
}

func (p *EventBridgePublisher) Publish(ctx context.Context, detailType string, detail any) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	out, err := p.client.PutEvents(ctx, &eventbridge.PutEventsInput{
		Entries: []types.PutEventsRequestEntry{{
			EventBusName: aws.String(p.busName),
			Source:       aws.String(Source),
			DetailType:   aws.String(detailType),
			Detail:       aws.String(string(b)),
		}},
	})
	if err != nil {
		return err
	}
	if out.FailedEntryCount > 0 {
		return fmt.Errorf("eventbridge put failed: %s", aws.ToString(out.Entries[0].ErrorMessage))
	}
	return nil
}
```

Then: `go get github.com/aws/aws-sdk-go-v2/service/eventbridge github.com/aws/aws-sdk-go-v2/aws`

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/events/ -v && go build ./...`
Expected: PASS, clean build

- [ ] **Step 5: Commit**

```bash
git add internal/events/ go.mod go.sum
git commit -m "feat: event contract types and EventBridge publisher"
```

---

### Task 4: Store — game records, lease semantics, DynamoDB adapter

**Files:**
- Create: `internal/store/store.go` (types + interface + in-memory fake), `internal/store/dynamo.go`
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: nothing from other packages.
- Produces:
  - `GameRecord{GameID, Season int64; GameDate string; StartTimeUTC time.Time; HomeAbbrev, AwayAbbrev, Venue, GameState, ScheduleEntryName string; LastPlaySortOrder int64; SnapshotHashes map[string]string; ChainCount int; LeaseOwner string; LeaseExpiresAt time.Time; Done bool}`
  - `PollerState{LastPlaySortOrder int64; SnapshotHashes map[string]string; ChainCount int; GameState string; Done bool}`
  - `GameStore` interface:
    - `UpsertSchedule(ctx, rec GameRecord) error` — writes schedule fields only, preserves poller fields on existing records
    - `Get(ctx, gameID int64) (*GameRecord, error)` — (nil, nil) when absent
    - `ListByDate(ctx, date string) ([]GameRecord, error)`
    - `AcquireLease(ctx, gameID int64, owner string, until time.Time) (bool, error)` — succeeds iff no lease, expired lease, or same owner
    - `RenewLease(ctx, gameID int64, owner string, until time.Time) (bool, error)` — succeeds iff owner matches
    - `ReleaseLease(ctx, gameID int64, owner string) error` — zeroes expiry iff owner matches; no-op otherwise
    - `UpdatePollerState(ctx, gameID int64, st PollerState) error`
  - `FakeGameStore` implementing `GameStore` with `NewFakeGameStore()`, plus `Now func() time.Time` field for lease-expiry tests
  - `NewDynamoStore(client *dynamodb.Client, table string) *DynamoStore` implementing `GameStore`

- [ ] **Step 1: Write the failing tests**

`internal/store/store_test.go`:
```go
package store

import (
	"context"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 15, 23, 0, 0, 0, time.UTC)

func newFake(now time.Time) *FakeGameStore {
	f := NewFakeGameStore()
	f.Now = func() time.Time { return now }
	return f
}

func TestUpsertPreservesPollerFields(t *testing.T) {
	ctx := context.Background()
	f := newFake(t0)
	rec := GameRecord{GameID: 1, GameDate: "2026-01-15", HomeAbbrev: "BUF", AwayAbbrev: "MTL", StartTimeUTC: t0}
	if err := f.UpsertSchedule(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := f.UpdatePollerState(ctx, 1, PollerState{LastPlaySortOrder: 50, ChainCount: 2, GameState: "LIVE"}); err != nil {
		t.Fatal(err)
	}
	rec.Venue = "KeyBank Center" // schedule-sync runs again with updated info
	if err := f.UpsertSchedule(ctx, rec); err != nil {
		t.Fatal(err)
	}
	got, _ := f.Get(ctx, 1)
	if got.LastPlaySortOrder != 50 || got.ChainCount != 2 {
		t.Errorf("poller fields clobbered: %+v", got)
	}
	if got.Venue != "KeyBank Center" {
		t.Errorf("schedule field not updated: %q", got.Venue)
	}
}

func TestLeaseSemantics(t *testing.T) {
	ctx := context.Background()
	f := newFake(t0)
	f.UpsertSchedule(ctx, GameRecord{GameID: 1, GameDate: "2026-01-15"})

	ok, err := f.AcquireLease(ctx, 1, "workerA", t0.Add(60*time.Second))
	if err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v", ok, err)
	}
	// Second worker cannot steal an unexpired lease.
	if ok, _ := f.AcquireLease(ctx, 1, "workerB", t0.Add(60*time.Second)); ok {
		t.Error("workerB acquired unexpired lease held by workerA")
	}
	// Same owner can re-acquire.
	if ok, _ := f.AcquireLease(ctx, 1, "workerA", t0.Add(90*time.Second)); !ok {
		t.Error("workerA could not re-acquire its own lease")
	}
	// Renew only for the owner.
	if ok, _ := f.RenewLease(ctx, 1, "workerB", t0.Add(120*time.Second)); ok {
		t.Error("workerB renewed a lease it does not own")
	}
	if ok, _ := f.RenewLease(ctx, 1, "workerA", t0.Add(120*time.Second)); !ok {
		t.Error("workerA could not renew")
	}
	// After expiry another worker can take it.
	f.Now = func() time.Time { return t0.Add(3 * time.Minute) }
	if ok, _ := f.AcquireLease(ctx, 1, "workerB", t0.Add(4*time.Minute)); !ok {
		t.Error("workerB could not acquire expired lease")
	}
}

func TestReleaseLease(t *testing.T) {
	ctx := context.Background()
	f := newFake(t0)
	f.UpsertSchedule(ctx, GameRecord{GameID: 1, GameDate: "2026-01-15"})
	f.AcquireLease(ctx, 1, "workerA", t0.Add(time.Minute))
	if err := f.ReleaseLease(ctx, 1, "workerA"); err != nil {
		t.Fatal(err)
	}
	// Released lease is immediately acquirable by another worker.
	if ok, _ := f.AcquireLease(ctx, 1, "workerB", t0.Add(time.Minute)); !ok {
		t.Error("lease not acquirable after release")
	}
	// Releasing someone else's lease is a no-op, not an error.
	if err := f.ReleaseLease(ctx, 1, "workerA"); err != nil {
		t.Errorf("stale release errored: %v", err)
	}
}

func TestListByDateAndMissingGet(t *testing.T) {
	ctx := context.Background()
	f := newFake(t0)
	f.UpsertSchedule(ctx, GameRecord{GameID: 1, GameDate: "2026-01-15"})
	f.UpsertSchedule(ctx, GameRecord{GameID: 2, GameDate: "2026-01-15"})
	f.UpsertSchedule(ctx, GameRecord{GameID: 3, GameDate: "2026-01-16"})
	games, err := f.ListByDate(ctx, "2026-01-15")
	if err != nil || len(games) != 2 {
		t.Fatalf("ListByDate: %d games, err=%v", len(games), err)
	}
	got, err := f.Get(ctx, 999)
	if err != nil || got != nil {
		t.Errorf("missing game: got=%v err=%v, want nil,nil", got, err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/store/ -v`
Expected: compile FAIL (`undefined: FakeGameStore`)

- [ ] **Step 3: Write implementation**

`internal/store/store.go`:
```go
// Package store persists game schedule records and poller bookkeeping.
package store

import (
	"context"
	"sync"
	"time"
)

type GameRecord struct {
	GameID            int64
	Season            int64
	GameDate          string // YYYY-MM-DD
	StartTimeUTC      time.Time
	HomeAbbrev        string
	AwayAbbrev        string
	Venue             string
	GameState         string
	ScheduleEntryName string
	LastPlaySortOrder int64
	SnapshotHashes    map[string]string
	ChainCount        int
	LeaseOwner        string
	LeaseExpiresAt    time.Time
	Done              bool
}

type PollerState struct {
	LastPlaySortOrder int64
	SnapshotHashes    map[string]string
	ChainCount        int
	GameState         string
	Done              bool
}

type GameStore interface {
	UpsertSchedule(ctx context.Context, rec GameRecord) error
	Get(ctx context.Context, gameID int64) (*GameRecord, error)
	ListByDate(ctx context.Context, date string) ([]GameRecord, error)
	AcquireLease(ctx context.Context, gameID int64, owner string, until time.Time) (bool, error)
	RenewLease(ctx context.Context, gameID int64, owner string, until time.Time) (bool, error)
	ReleaseLease(ctx context.Context, gameID int64, owner string) error
	UpdatePollerState(ctx context.Context, gameID int64, st PollerState) error
}

// FakeGameStore is an in-memory GameStore mirroring DynamoDB's conditional
// write semantics, for tests.
type FakeGameStore struct {
	mu    sync.Mutex
	games map[int64]*GameRecord
	Now   func() time.Time
}

func NewFakeGameStore() *FakeGameStore {
	return &FakeGameStore{games: map[int64]*GameRecord{}, Now: time.Now}
}

func (f *FakeGameStore) UpsertSchedule(_ context.Context, rec GameRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if existing, ok := f.games[rec.GameID]; ok {
		existing.Season = rec.Season
		existing.GameDate = rec.GameDate
		existing.StartTimeUTC = rec.StartTimeUTC
		existing.HomeAbbrev = rec.HomeAbbrev
		existing.AwayAbbrev = rec.AwayAbbrev
		existing.Venue = rec.Venue
		existing.GameState = rec.GameState
		existing.ScheduleEntryName = rec.ScheduleEntryName
		return nil
	}
	r := rec
	f.games[rec.GameID] = &r
	return nil
}

func (f *FakeGameStore) Get(_ context.Context, gameID int64) (*GameRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.games[gameID]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, nil
}

func (f *FakeGameStore) ListByDate(_ context.Context, date string) ([]GameRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []GameRecord
	for _, r := range f.games {
		if r.GameDate == date {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (f *FakeGameStore) AcquireLease(_ context.Context, gameID int64, owner string, until time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.games[gameID]
	if !ok {
		return false, nil
	}
	free := r.LeaseOwner == "" || !r.LeaseExpiresAt.After(f.Now()) || r.LeaseOwner == owner
	if !free {
		return false, nil
	}
	r.LeaseOwner = owner
	r.LeaseExpiresAt = until
	return true, nil
}

func (f *FakeGameStore) RenewLease(_ context.Context, gameID int64, owner string, until time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.games[gameID]
	if !ok || r.LeaseOwner != owner {
		return false, nil
	}
	r.LeaseExpiresAt = until
	return true, nil
}

func (f *FakeGameStore) ReleaseLease(_ context.Context, gameID int64, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.games[gameID]; ok && r.LeaseOwner == owner {
		r.LeaseExpiresAt = time.Time{}
	}
	return nil
}

func (f *FakeGameStore) UpdatePollerState(_ context.Context, gameID int64, st PollerState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.games[gameID]; ok {
		r.LastPlaySortOrder = st.LastPlaySortOrder
		r.SnapshotHashes = st.SnapshotHashes
		r.ChainCount = st.ChainCount
		r.GameState = st.GameState
		r.Done = st.Done
	}
	return nil
}
```

`internal/store/dynamo.go` — real adapter. Table PK `gameId` (N); GSI `byGameDate` with PK `gameDate` (S). Times stored as RFC3339 strings; hashes as a map attribute.
```go
package store

import (
	"context"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type DynamoStore struct {
	client *dynamodb.Client
	table  string
	Now    func() time.Time
}

func NewDynamoStore(client *dynamodb.Client, table string) *DynamoStore {
	return &DynamoStore{client: client, table: table, Now: time.Now}
}

// ddbGame is the marshaled shape of a GameRecord.
type ddbGame struct {
	GameID            int64             `dynamodbav:"gameId"`
	Season            int64             `dynamodbav:"season"`
	GameDate          string            `dynamodbav:"gameDate"`
	StartTimeUTC      string            `dynamodbav:"startTimeUTC"`
	HomeAbbrev        string            `dynamodbav:"homeAbbrev"`
	AwayAbbrev        string            `dynamodbav:"awayAbbrev"`
	Venue             string            `dynamodbav:"venue"`
	GameState         string            `dynamodbav:"gameState"`
	ScheduleEntryName string            `dynamodbav:"scheduleEntryName"`
	LastPlaySortOrder int64             `dynamodbav:"lastPlaySortOrder"`
	SnapshotHashes    map[string]string `dynamodbav:"snapshotHashes,omitempty"`
	ChainCount        int               `dynamodbav:"chainCount"`
	LeaseOwner        string            `dynamodbav:"leaseOwner,omitempty"`
	LeaseExpiresAt    string            `dynamodbav:"leaseExpiresAt,omitempty"`
	Done              bool              `dynamodbav:"done"`
}

func toRecord(g ddbGame) GameRecord {
	start, _ := time.Parse(time.RFC3339, g.StartTimeUTC)
	lease, _ := time.Parse(time.RFC3339, g.LeaseExpiresAt)
	return GameRecord{
		GameID: g.GameID, Season: g.Season, GameDate: g.GameDate,
		StartTimeUTC: start, HomeAbbrev: g.HomeAbbrev, AwayAbbrev: g.AwayAbbrev,
		Venue: g.Venue, GameState: g.GameState, ScheduleEntryName: g.ScheduleEntryName,
		LastPlaySortOrder: g.LastPlaySortOrder, SnapshotHashes: g.SnapshotHashes,
		ChainCount: g.ChainCount, LeaseOwner: g.LeaseOwner, LeaseExpiresAt: lease,
		Done: g.Done,
	}
}

func (d *DynamoStore) key(gameID int64) map[string]types.AttributeValue {
	k, _ := attributevalue.MarshalMap(map[string]int64{"gameId": gameID})
	return k
}

func (d *DynamoStore) UpsertSchedule(ctx context.Context, rec GameRecord) error {
	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(d.table),
		Key:       d.key(rec.GameID),
		UpdateExpression: aws.String(
			"SET season=:se, gameDate=:gd, startTimeUTC=:st, homeAbbrev=:h, awayAbbrev=:a, " +
				"venue=:v, gameState=:gs, scheduleEntryName=:sn, " +
				"lastPlaySortOrder=if_not_exists(lastPlaySortOrder,:zero), " +
				"chainCount=if_not_exists(chainCount,:zero), done=if_not_exists(done,:f)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":se":   &types.AttributeValueMemberN{Value: itoa(rec.Season)},
			":gd":   &types.AttributeValueMemberS{Value: rec.GameDate},
			":st":   &types.AttributeValueMemberS{Value: rec.StartTimeUTC.UTC().Format(time.RFC3339)},
			":h":    &types.AttributeValueMemberS{Value: rec.HomeAbbrev},
			":a":    &types.AttributeValueMemberS{Value: rec.AwayAbbrev},
			":v":    &types.AttributeValueMemberS{Value: rec.Venue},
			":gs":   &types.AttributeValueMemberS{Value: rec.GameState},
			":sn":   &types.AttributeValueMemberS{Value: rec.ScheduleEntryName},
			":zero": &types.AttributeValueMemberN{Value: "0"},
			":f":    &types.AttributeValueMemberBOOL{Value: false},
		},
	})
	return err
}

func (d *DynamoStore) Get(ctx context.Context, gameID int64) (*GameRecord, error) {
	out, err := d.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(d.table), Key: d.key(gameID), ConsistentRead: aws.Bool(true),
	})
	if err != nil || out.Item == nil {
		return nil, err
	}
	var g ddbGame
	if err := attributevalue.UnmarshalMap(out.Item, &g); err != nil {
		return nil, err
	}
	rec := toRecord(g)
	return &rec, nil
}

func (d *DynamoStore) ListByDate(ctx context.Context, date string) ([]GameRecord, error) {
	out, err := d.client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(d.table),
		IndexName:              aws.String("byGameDate"),
		KeyConditionExpression: aws.String("gameDate = :d"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":d": &types.AttributeValueMemberS{Value: date},
		},
	})
	if err != nil {
		return nil, err
	}
	var recs []GameRecord
	for _, item := range out.Items {
		var g ddbGame
		if err := attributevalue.UnmarshalMap(item, &g); err != nil {
			return nil, err
		}
		recs = append(recs, toRecord(g))
	}
	return recs, nil
}

func (d *DynamoStore) AcquireLease(ctx context.Context, gameID int64, owner string, until time.Time) (bool, error) {
	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String(d.table),
		Key:                 d.key(gameID),
		UpdateExpression:    aws.String("SET leaseOwner=:o, leaseExpiresAt=:u"),
		ConditionExpression: aws.String("attribute_exists(gameId) AND (attribute_not_exists(leaseOwner) OR leaseExpiresAt < :now OR leaseOwner = :o)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":o":   &types.AttributeValueMemberS{Value: owner},
			":u":   &types.AttributeValueMemberS{Value: until.UTC().Format(time.RFC3339)},
			":now": &types.AttributeValueMemberS{Value: d.Now().UTC().Format(time.RFC3339)},
		},
	})
	return leaseResult(err)
}

func (d *DynamoStore) RenewLease(ctx context.Context, gameID int64, owner string, until time.Time) (bool, error) {
	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String(d.table),
		Key:                 d.key(gameID),
		UpdateExpression:    aws.String("SET leaseExpiresAt=:u"),
		ConditionExpression: aws.String("leaseOwner = :o"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":o": &types.AttributeValueMemberS{Value: owner},
			":u": &types.AttributeValueMemberS{Value: until.UTC().Format(time.RFC3339)},
		},
	})
	return leaseResult(err)
}

func (d *DynamoStore) ReleaseLease(ctx context.Context, gameID int64, owner string) error {
	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String(d.table),
		Key:                 d.key(gameID),
		UpdateExpression:    aws.String("SET leaseExpiresAt=:zero"),
		ConditionExpression: aws.String("leaseOwner = :o"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":o":    &types.AttributeValueMemberS{Value: owner},
			":zero": &types.AttributeValueMemberS{Value: time.Time{}.UTC().Format(time.RFC3339)},
		},
	})
	if ok, err2 := leaseResult(err); !ok && err2 == nil {
		return nil // stale release: condition failed, treat as no-op
	}
	return err
}

func (d *DynamoStore) UpdatePollerState(ctx context.Context, gameID int64, st PollerState) error {
	hashes := st.SnapshotHashes
	if hashes == nil {
		hashes = map[string]string{}
	}
	hv, err := attributevalue.Marshal(hashes)
	if err != nil {
		return err
	}
	_, err = d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:        aws.String(d.table),
		Key:              d.key(gameID),
		UpdateExpression: aws.String("SET lastPlaySortOrder=:so, snapshotHashes=:sh, chainCount=:cc, gameState=:gs, done=:dn"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":so": &types.AttributeValueMemberN{Value: itoa(st.LastPlaySortOrder)},
			":sh": hv,
			":cc": &types.AttributeValueMemberN{Value: itoa(int64(st.ChainCount))},
			":gs": &types.AttributeValueMemberS{Value: st.GameState},
			":dn": &types.AttributeValueMemberBOOL{Value: st.Done},
		},
	})
	return err
}

// leaseResult maps ConditionalCheckFailed to (false, nil).
func leaseResult(err error) (bool, error) {
	if err == nil {
		return true, nil
	}
	var ccf *types.ConditionalCheckFailedException
	if errors.As(err, &ccf) {
		return false, nil
	}
	return false, err
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
```
(Add `"strconv"` to imports.) Then: `go get github.com/aws/aws-sdk-go-v2/service/dynamodb github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue`

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/store/ -v && go build ./...`
Expected: PASS (fake tests), clean build (compile-checks the DynamoDB adapter)

- [ ] **Step 5: Commit**

```bash
git add internal/store/ go.mod go.sum
git commit -m "feat: game store with lease semantics; DynamoDB adapter"
```

---

### Task 5: Archive — S3 key layout + adapter

**Files:**
- Create: `internal/store/archive.go`, `internal/store/s3.go`
- Test: `internal/store/archive_test.go`

**Interfaces:**
- Consumes: nothing from other packages.
- Produces:
  - `Archive` interface: `Put(ctx context.Context, key string, body []byte) error`
  - Pure key builders (tested):
    - `SnapshotKey(season int64, gameDate string, gameID int64, feed string, ts time.Time) string`
    - `FinalKey(season int64, gameDate string, gameID int64, feed string) string`
    - `ScheduleKey(date string) string`
    - `GamePrefix(season int64, gameDate string, gameID int64) string` (used in FinalEvent.S3Prefix)
  - `FakeArchive{Objects map[string][]byte}` with `NewFakeArchive()`
  - `NewS3Archive(client *s3.Client, bucket string) *S3Archive` implementing `Archive`

- [ ] **Step 1: Write the failing test**

`internal/store/archive_test.go`:
```go
package store

import (
	"context"
	"testing"
	"time"
)

func TestKeyLayout(t *testing.T) {
	ts := time.Date(2026, 1, 15, 23, 45, 5, 0, time.UTC)
	cases := []struct{ got, want string }{
		{SnapshotKey(20252026, "2026-01-15", 2025020740, "pbp", ts),
			"raw/20252026/2026-01-15/2025020740/pbp/20260115T234505Z.json"},
		{FinalKey(20252026, "2026-01-15", 2025020740, "shifts"),
			"raw/20252026/2026-01-15/2025020740/final/shifts.json"},
		{ScheduleKey("2026-01-15"), "raw/schedule/2026-01-15.json"},
		{GamePrefix(20252026, "2026-01-15", 2025020740),
			"raw/20252026/2026-01-15/2025020740/"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

func TestFakeArchive(t *testing.T) {
	f := NewFakeArchive()
	if err := f.Put(context.Background(), "raw/x.json", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if string(f.Objects["raw/x.json"]) != `{}` {
		t.Error("object not stored")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run 'TestKeyLayout|TestFakeArchive' -v`
Expected: compile FAIL (`undefined: SnapshotKey`)

- [ ] **Step 3: Write implementation**

`internal/store/archive.go`:
```go
package store

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type Archive interface {
	Put(ctx context.Context, key string, body []byte) error
}

func GamePrefix(season int64, gameDate string, gameID int64) string {
	return fmt.Sprintf("raw/%d/%s/%d/", season, gameDate, gameID)
}

func SnapshotKey(season int64, gameDate string, gameID int64, feed string, ts time.Time) string {
	return fmt.Sprintf("%s%s/%s.json", GamePrefix(season, gameDate, gameID), feed, ts.UTC().Format("20060102T150405Z"))
}

func FinalKey(season int64, gameDate string, gameID int64, feed string) string {
	return fmt.Sprintf("%sfinal/%s.json", GamePrefix(season, gameDate, gameID), feed)
}

func ScheduleKey(date string) string {
	return fmt.Sprintf("raw/schedule/%s.json", date)
}

type FakeArchive struct {
	mu      sync.Mutex
	Objects map[string][]byte
}

func NewFakeArchive() *FakeArchive {
	return &FakeArchive{Objects: map[string][]byte{}}
}

func (f *FakeArchive) Put(_ context.Context, key string, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Objects[key] = append([]byte(nil), body...)
	return nil
}
```

`internal/store/s3.go`:
```go
package store

import (
	"bytes"
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type S3Archive struct {
	client *s3.Client
	bucket string
}

func NewS3Archive(client *s3.Client, bucket string) *S3Archive {
	return &S3Archive{client: client, bucket: bucket}
}

func (a *S3Archive) Put(ctx context.Context, key string, body []byte) error {
	_, err := a.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(a.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("application/json"),
	})
	return err
}
```
Then: `go get github.com/aws/aws-sdk-go-v2/service/s3`

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/store/ -v && go build ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/store/ go.mod go.sum
git commit -m "feat: S3 archive with tested key layout"
```

---

### Task 6: Poller diff logic (golden tests on real data)

**Files:**
- Create: `internal/poller/diff.go`
- Create: `internal/poller/testdata/` (symlink-free copy: reuse `internal/nhl/testdata/pbp.json` via `../../nhl/testdata` — no, copy it: `cp internal/nhl/testdata/pbp.json internal/poller/testdata/pbp.json`)
- Test: `internal/poller/diff_test.go`

**Interfaces:**
- Consumes: `nhl.PlayByPlay`, `nhl.Play`, `nhl.PlayDetails` (Task 2); `events.PlayEvent`, `events.StatusEvent` (Task 3).
- Produces:
  - `NewPlays(plays []nhl.Play, lastSortOrder int64) []nhl.Play` — plays with `SortOrder > lastSortOrder`, ascending by SortOrder
  - `BuildPlayEvent(pbp *nhl.PlayByPlay, p nhl.Play, score map[string]int) events.PlayEvent` — score is the running score AFTER this play (caller threads it); `ActingTeam` from `eventOwnerTeamId` matched against home/away team IDs; `ScoringTeam` set only when `TypeDescKey == "goal"`
  - `RunningScore(pbp *nhl.PlayByPlay, p nhl.Play, prev map[string]int) map[string]int` — returns `{homeAbbrev: n, awayAbbrev: m}` from the play's details when present (goals carry awayScore/homeScore), else `prev` unchanged
  - `IsFinalState(s string) bool` — true for `FINAL`, `OFF`
  - `IsLiveState(s string) bool` — true for `LIVE`, `CRIT`

- [ ] **Step 1: Copy fixture and write the failing test**

```bash
mkdir -p internal/poller/testdata
cp internal/nhl/testdata/pbp.json internal/poller/testdata/pbp.json
```

`internal/poller/diff_test.go`:
```go
package poller

import (
	"encoding/json"
	"os"
	"testing"

	"hockeytrack/internal/nhl"
)

func loadPBP(t *testing.T) *nhl.PlayByPlay {
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

func TestNewPlaysHighWaterMark(t *testing.T) {
	pbp := loadPBP(t)
	all := NewPlays(pbp.Plays, 0)
	if len(all) != len(pbp.Plays) {
		t.Fatalf("from zero: %d plays, want all %d", len(all), len(pbp.Plays))
	}
	// Ascending order.
	for i := 1; i < len(all); i++ {
		if all[i].SortOrder <= all[i-1].SortOrder {
			t.Fatalf("not ascending at %d: %d then %d", i, all[i-1].SortOrder, all[i].SortOrder)
		}
	}
	// Simulate: we've seen the first half; only the rest come back.
	mid := all[len(all)/2].SortOrder
	rest := NewPlays(pbp.Plays, mid)
	if len(rest) != len(all)-len(all)/2-1 {
		t.Errorf("after mark %d: got %d plays, want %d", mid, len(rest), len(all)-len(all)/2-1)
	}
	for _, p := range rest {
		if p.SortOrder <= mid {
			t.Errorf("play %d at or below mark %d", p.SortOrder, mid)
		}
	}
	// Nothing new when mark is at the end.
	if got := NewPlays(pbp.Plays, all[len(all)-1].SortOrder); len(got) != 0 {
		t.Errorf("expected 0 new plays, got %d", len(got))
	}
}

func TestGoldenEventSequence(t *testing.T) {
	// Replay the entire game through the diff; check goal events carry
	// correct teams and running score (CHI@FLA finished 2-3).
	pbp := loadPBP(t)
	score := map[string]int{pbp.HomeTeam.Abbrev: 0, pbp.AwayTeam.Abbrev: 0}
	var goals []string
	var last int64
	for _, p := range NewPlays(pbp.Plays, last) {
		score = RunningScore(pbp, p, score)
		e := BuildPlayEvent(pbp, p, score)
		if e.SchemaVersion != 1 || e.GameID != 2025020001 {
			t.Fatalf("bad envelope: %+v", e)
		}
		if p.TypeDescKey == "goal" {
			if e.ScoringTeam == "" {
				t.Error("goal event missing scoringTeam")
			}
			goals = append(goals, e.ScoringTeam)
		} else if e.ScoringTeam != "" {
			t.Errorf("%s event has scoringTeam %q", p.TypeDescKey, e.ScoringTeam)
		}
		last = p.SortOrder
	}
	if len(goals) != 5 { // 2+3 total goals in this game
		t.Errorf("saw %d goal events, want 5: %v", len(goals), goals)
	}
	if score["FLA"] != 3 || score["CHI"] != 2 {
		t.Errorf("final running score = %v, want FLA 3 CHI 2", score)
	}
}

func TestStateClassifiers(t *testing.T) {
	for _, s := range []string{"FINAL", "OFF"} {
		if !IsFinalState(s) {
			t.Errorf("IsFinalState(%s) = false", s)
		}
	}
	for _, s := range []string{"FUT", "PRE", "LIVE", "CRIT"} {
		if IsFinalState(s) {
			t.Errorf("IsFinalState(%s) = true", s)
		}
	}
	if !IsLiveState("LIVE") || !IsLiveState("CRIT") || IsLiveState("FUT") {
		t.Error("IsLiveState misclassifies")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/poller/ -v`
Expected: compile FAIL (`undefined: NewPlays`)

- [ ] **Step 3: Write implementation**

`internal/poller/diff.go`:
```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/poller/ -v`
Expected: PASS. If `TestGoldenEventSequence` fails on the goal count, print the actual count and verify against the fixture (`python3 -c "import json; print(sum(1 for p in json.load(open('internal/poller/testdata/pbp.json'))['plays'] if p['typeDescKey']=='goal'))"`) — adjust the expected constant to the fixture's true value, never the logic to force a number.

- [ ] **Step 5: Commit**

```bash
git add internal/poller/
git commit -m "feat: play-by-play diff logic with golden tests on real game data"
```

---

### Task 7: Poller loop

**Files:**
- Create: `internal/poller/poller.go`
- Test: `internal/poller/poller_test.go`

**Interfaces:**
- Consumes: `nhl` client methods (Task 2) via a local `Feed` interface; `store.GameStore`, `store.PollerState`, `store.FakeGameStore` (Task 4); `store.Archive`, `store.FakeArchive`, key builders (Task 5); `events.Publisher`, `events.FakePublisher`, event types (Task 3); diff functions (Task 6).
- Produces:
  - `Feed` interface (subset of nhl.Client, lets tests script snapshots): `PlayByPlay(ctx, gameID int64) (*nhl.PlayByPlay, []byte, error)`, `RawFeed(ctx, gameID int64, feed string) ([]byte, error)`, `ShiftCharts(ctx, gameID int64) ([]byte, error)`
  - `Config{LiveInterval, PregameInterval, LeaseTTL time.Duration; MaxChains int}` with `DefaultConfig() Config` (5s, 30s, 60s, 30)
  - `Deps{Feed Feed; Store store.GameStore; Archive store.Archive; Pub events.Publisher; Now func() time.Time; Sleep func(context.Context, time.Duration) error}`
  - `Outcome` enum: `OutcomeFinal`, `OutcomeHandOff`, `OutcomeLeaseHeld`, `OutcomeAlreadyDone`, `OutcomeStale`, `OutcomeNotScheduled`
  - `Run(ctx context.Context, d Deps, cfg Config, gameID int64, owner string, shouldHandOff func() bool) (Outcome, error)`

**Behavior of `Run` (implement exactly):**
1. `Get` record; if nil → `OutcomeNotScheduled`. If `Done` or `IsFinalState(rec.GameState)` → `OutcomeAlreadyDone`.
2. `AcquireLease(gameID, owner, Now()+LeaseTTL)`; false → `OutcomeLeaseHeld`.
3. `chain := rec.ChainCount + 1`; if `chain > cfg.MaxChains`: publish `AlertEvent{Reason: "max chain links exceeded"}` (DTAlert), `UpdatePollerState` with `Done: true`, return `OutcomeStale`.
4. Loop: on each iteration —
   a. If ctx canceled → return ctx.Err().
   b. If `shouldHandOff()` → persist state, `ReleaseLease`, return `OutcomeHandOff`.
   c. Fetch `PlayByPlay`. On error: sleep min(interval×2, 30s) and continue (transient = missed cycle).
   d. Hash raw body (sha256 hex). If != `hashes["pbp"]`: `Archive.Put(SnapshotKey(...,"pbp", Now()))`, update hash.
   e. Fetch boxscore via `RawFeed`; same hash-compare → `SnapshotKey(...,"boxscore",...)`. Fetch errors here are logged and skipped, never fatal.
   f. If `pbp.GameState != lastState`: publish `StatusEvent{PrevState: lastState, GameState: pbp.GameState, Score: {home: pbp.HomeTeam.Score, away: pbp.AwayTeam.Score}}`; update lastState.
   g. For each play in `NewPlays(pbp.Plays, lastSort)`: thread `RunningScore`, publish `BuildPlayEvent`; advance lastSort after each successful publish (a failed publish stops the batch so the play is re-emitted next cycle — at-least-once).
   h. Persist `UpdatePollerState{lastSort, hashes, chain, pbp.GameState, false}`; `RenewLease(owner, Now()+LeaseTTL)` — if renew returns false, return `OutcomeLeaseHeld` (someone stole it; stop).
   i. If `IsFinalState(pbp.GameState)`: archive sweep — final pbp raw + `RawFeed` boxscore/landing + `ShiftCharts`, each to `FinalKey(...)`, individual fetch errors logged and skipped; publish `FinalEvent{Score, S3Prefix: GamePrefix(...)}`; `UpdatePollerState{..., Done: true}`; `ReleaseLease`; return `OutcomeFinal`.
   j. Sleep `LiveInterval` if `IsLiveState`, else `PregameInterval`.
- Season/gameDate for keys come from `pbp.Season` and `pbp.GameDate`.

- [ ] **Step 1: Write the failing test**

`internal/poller/poller_test.go`:
```go
package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"hockeytrack/internal/events"
	"hockeytrack/internal/nhl"
	"hockeytrack/internal/store"
)

// scriptedFeed returns successive play-by-play snapshots, then repeats the last.
type scriptedFeed struct {
	snapshots [][]byte // marshaled PlayByPlay JSON bodies
	i         int
}

func (s *scriptedFeed) PlayByPlay(_ context.Context, _ int64) (*nhl.PlayByPlay, []byte, error) {
	raw := s.snapshots[s.i]
	if s.i < len(s.snapshots)-1 {
		s.i++
	}
	var p nhl.PlayByPlay
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, nil, err
	}
	return &p, raw, nil
}

func (s *scriptedFeed) RawFeed(_ context.Context, _ int64, feed string) ([]byte, error) {
	return []byte(fmt.Sprintf(`{"feed":%q}`, feed)), nil
}

func (s *scriptedFeed) ShiftCharts(_ context.Context, _ int64) ([]byte, error) {
	return []byte(`{"data":[]}`), nil
}

// truncatedSnapshot builds a snapshot of the fixture game with only the first
// n plays and the given gameState — simulating a live game in progress.
func truncatedSnapshot(t *testing.T, n int, state string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/pbp.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	plays := m["plays"].([]any)
	if n > len(plays) {
		n = len(plays)
	}
	m["plays"] = plays[:n]
	m["gameState"] = state
	out, _ := json.Marshal(m)
	return out
}

func testDeps(feed Feed) (Deps, *store.FakeGameStore, *store.FakeArchive, *events.FakePublisher) {
	gs := store.NewFakeGameStore()
	ar := store.NewFakeArchive()
	pub := &events.FakePublisher{}
	// Advancing clock: successive snapshots must get distinct S3 keys.
	now := time.Date(2026, 1, 15, 23, 0, 0, 0, time.UTC)
	tick := func() time.Time { now = now.Add(time.Second); return now }
	gs.Now = tick
	d := Deps{
		Feed: feed, Store: gs, Archive: ar, Pub: pub,
		Now:   tick,
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	return d, gs, ar, pub
}

func seedGame(t *testing.T, gs *store.FakeGameStore) {
	t.Helper()
	err := gs.UpsertSchedule(context.Background(), store.GameRecord{
		GameID: 2025020001, Season: 20252026, GameDate: "2025-10-07",
		HomeAbbrev: "FLA", AwayAbbrev: "CHI", GameState: "FUT",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunToFinal(t *testing.T) {
	feed := &scriptedFeed{snapshots: [][]byte{
		truncatedSnapshot(t, 10, "LIVE"),
		truncatedSnapshot(t, 25, "LIVE"),
		truncatedSnapshot(t, 0, "OFF"), // 0 means all plays: adjust helper call below
	}}
	// Final snapshot carries every play.
	feed.snapshots[2] = truncatedSnapshot(t, 1<<30, "OFF")

	d, gs, ar, pub := testDeps(feed)
	seedGame(t, gs)

	out, err := Run(context.Background(), d, DefaultConfig(), 2025020001, "link1", func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if out != OutcomeFinal {
		t.Fatalf("outcome = %v, want OutcomeFinal", out)
	}

	// Every play was emitted exactly once, in order.
	var seqs []int64
	var finals, statuses int
	for _, e := range pub.Published {
		switch e.DetailType {
		case events.DTPlay:
			seqs = append(seqs, e.Detail.(events.PlayEvent).Seq)
		case events.DTFinal:
			finals++
		case events.DTStatus:
			statuses++
		}
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("duplicate or out-of-order seq at %d: %d then %d", i, seqs[i-1], seqs[i])
		}
	}
	if finals != 1 {
		t.Errorf("final events = %d, want 1", finals)
	}
	if statuses < 2 { // "" -> LIVE, LIVE -> OFF (from seeded FUT state: FUT->LIVE, LIVE->OFF)
		t.Errorf("status events = %d, want >= 2", statuses)
	}

	// Archive: pbp snapshots for each distinct body + final sweep objects.
	var pbpSnaps, finalObjs int
	for k := range ar.Objects {
		switch {
		case strings.Contains(k, "/pbp/"):
			pbpSnaps++
		case strings.Contains(k, "/final/"):
			finalObjs++
		}
	}
	if pbpSnaps != 3 {
		t.Errorf("pbp snapshots = %d, want 3 (one per distinct body)", pbpSnaps)
	}
	if finalObjs != 4 { // pbp, boxscore, landing, shifts
		t.Errorf("final objects = %d, want 4", finalObjs)
	}

	rec, _ := gs.Get(context.Background(), 2025020001)
	if !rec.Done {
		t.Error("game not marked done")
	}
}

func TestRunHandsOff(t *testing.T) {
	feed := &scriptedFeed{snapshots: [][]byte{truncatedSnapshot(t, 10, "LIVE")}}
	d, gs, _, _ := testDeps(feed)
	seedGame(t, gs)

	calls := 0
	handOff := func() bool { calls++; return calls > 2 }
	out, err := Run(context.Background(), d, DefaultConfig(), 2025020001, "link1", handOff)
	if err != nil || out != OutcomeHandOff {
		t.Fatalf("outcome=%v err=%v, want OutcomeHandOff", out, err)
	}
	rec, _ := gs.Get(context.Background(), 2025020001)
	if rec.ChainCount != 1 {
		t.Errorf("chainCount = %d, want 1", rec.ChainCount)
	}
	if rec.LastPlaySortOrder == 0 {
		t.Error("high-water mark not persisted before handoff")
	}
	// Lease released: a new link can acquire immediately.
	if ok, _ := gs.AcquireLease(context.Background(), 2025020001, "link2", time.Now().Add(time.Minute)); !ok {
		t.Error("next link cannot acquire lease after handoff")
	}
}

func TestSecondLinkResumesWithoutDuplicates(t *testing.T) {
	// Link 1 sees 10 plays then hands off; link 2 sees all plays and finishes.
	feed1 := &scriptedFeed{snapshots: [][]byte{truncatedSnapshot(t, 10, "LIVE")}}
	d, gs, _, pub := testDeps(feed1)
	seedGame(t, gs)
	calls := 0
	Run(context.Background(), d, DefaultConfig(), 2025020001, "link1", func() bool { calls++; return calls > 1 })
	firstBatch := len(pub.Published)

	d.Feed = &scriptedFeed{snapshots: [][]byte{truncatedSnapshot(t, 1<<30, "OFF")}}
	out, err := Run(context.Background(), d, DefaultConfig(), 2025020001, "link2", func() bool { return false })
	if err != nil || out != OutcomeFinal {
		t.Fatalf("link2 outcome=%v err=%v", out, err)
	}
	// No seq published by link2 may repeat one from link1.
	seen := map[int64]bool{}
	for i, e := range pub.Published {
		if e.DetailType != events.DTPlay {
			continue
		}
		seq := e.Detail.(events.PlayEvent).Seq
		if seen[seq] {
			t.Fatalf("seq %d duplicated (event %d, firstBatch=%d)", seq, i, firstBatch)
		}
		seen[seq] = true
	}
}

func TestMaxChainsGoesStale(t *testing.T) {
	feed := &scriptedFeed{snapshots: [][]byte{truncatedSnapshot(t, 10, "LIVE")}}
	d, gs, _, pub := testDeps(feed)
	seedGame(t, gs)
	gs.UpdatePollerState(context.Background(), 2025020001, store.PollerState{ChainCount: 30, GameState: "LIVE"})

	out, err := Run(context.Background(), d, DefaultConfig(), 2025020001, "link31", func() bool { return false })
	if err != nil || out != OutcomeStale {
		t.Fatalf("outcome=%v err=%v, want OutcomeStale", out, err)
	}
	var alerts int
	for _, e := range pub.Published {
		if e.DetailType == events.DTAlert {
			alerts++
		}
	}
	if alerts != 1 {
		t.Errorf("alert events = %d, want 1", alerts)
	}
}

func TestAlreadyDoneAndLeaseHeld(t *testing.T) {
	feed := &scriptedFeed{snapshots: [][]byte{truncatedSnapshot(t, 10, "LIVE")}}
	d, gs, _, _ := testDeps(feed)
	seedGame(t, gs)

	gs.AcquireLease(context.Background(), 2025020001, "other", time.Date(2026, 1, 15, 23, 5, 0, 0, time.UTC))
	if out, _ := Run(context.Background(), d, DefaultConfig(), 2025020001, "me", func() bool { return false }); out != OutcomeLeaseHeld {
		t.Errorf("outcome = %v, want OutcomeLeaseHeld", out)
	}

	gs.UpdatePollerState(context.Background(), 2025020001, store.PollerState{Done: true, GameState: "OFF"})
	if out, _ := Run(context.Background(), d, DefaultConfig(), 2025020001, "me2", func() bool { return false }); out != OutcomeAlreadyDone {
		t.Errorf("outcome = %v, want OutcomeAlreadyDone", out)
	}

	if out, _ := Run(context.Background(), d, DefaultConfig(), 999, "me3", func() bool { return false }); out != OutcomeNotScheduled {
		t.Errorf("outcome = %v, want OutcomeNotScheduled", out)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/poller/ -v`
Expected: compile FAIL (`undefined: Run`, `undefined: Deps`)

- [ ] **Step 3: Write implementation**

`internal/poller/poller.go` (implements the behavior spec above exactly):
```go
package poller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"time"

	"hockeytrack/internal/events"
	"hockeytrack/internal/nhl"
	"hockeytrack/internal/store"
)

type Feed interface {
	PlayByPlay(ctx context.Context, gameID int64) (*nhl.PlayByPlay, []byte, error)
	RawFeed(ctx context.Context, gameID int64, feed string) ([]byte, error)
	ShiftCharts(ctx context.Context, gameID int64) ([]byte, error)
}

type Config struct {
	LiveInterval    time.Duration
	PregameInterval time.Duration
	LeaseTTL        time.Duration
	MaxChains       int
}

func DefaultConfig() Config {
	return Config{LiveInterval: 5 * time.Second, PregameInterval: 30 * time.Second, LeaseTTL: 60 * time.Second, MaxChains: 30}
}

type Deps struct {
	Feed    Feed
	Store   store.GameStore
	Archive store.Archive
	Pub     events.Publisher
	Now     func() time.Time
	Sleep   func(context.Context, time.Duration) error
}

type Outcome int

const (
	OutcomeFinal Outcome = iota
	OutcomeHandOff
	OutcomeLeaseHeld
	OutcomeAlreadyDone
	OutcomeStale
	OutcomeNotScheduled
)

func hashOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func Run(ctx context.Context, d Deps, cfg Config, gameID int64, owner string, shouldHandOff func() bool) (Outcome, error) {
	rec, err := d.Store.Get(ctx, gameID)
	if err != nil {
		return 0, err
	}
	if rec == nil {
		return OutcomeNotScheduled, nil
	}
	if rec.Done || IsFinalState(rec.GameState) {
		return OutcomeAlreadyDone, nil
	}
	ok, err := d.Store.AcquireLease(ctx, gameID, owner, d.Now().Add(cfg.LeaseTTL))
	if err != nil {
		return 0, err
	}
	if !ok {
		return OutcomeLeaseHeld, nil
	}

	chain := rec.ChainCount + 1
	state := store.PollerState{
		LastPlaySortOrder: rec.LastPlaySortOrder,
		SnapshotHashes:    map[string]string{},
		ChainCount:        chain,
		GameState:         rec.GameState,
	}
	for k, v := range rec.SnapshotHashes {
		state.SnapshotHashes[k] = v
	}
	if chain > cfg.MaxChains {
		_ = d.Pub.Publish(ctx, events.DTAlert, events.AlertEvent{
			SchemaVersion: events.SchemaVersion, GameID: gameID, Reason: "max chain links exceeded",
		})
		state.Done = true
		_ = d.Store.UpdatePollerState(ctx, gameID, state)
		_ = d.Store.ReleaseLease(ctx, gameID, owner)
		return OutcomeStale, nil
	}
	if err := d.Store.UpdatePollerState(ctx, gameID, state); err != nil {
		return 0, err
	}

	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if shouldHandOff() {
			_ = d.Store.UpdatePollerState(ctx, gameID, state)
			_ = d.Store.ReleaseLease(ctx, gameID, owner)
			return OutcomeHandOff, nil
		}

		pbp, raw, err := d.Feed.PlayByPlay(ctx, gameID)
		if err != nil {
			slog.Warn("play-by-play fetch failed", "gameId", gameID, "err", err)
			if err := d.Sleep(ctx, min(cfg.LiveInterval*2, 30*time.Second)); err != nil {
				return 0, err
			}
			continue
		}

		if h := hashOf(raw); h != state.SnapshotHashes["pbp"] {
			key := store.SnapshotKey(pbp.Season, pbp.GameDate, gameID, "pbp", d.Now())
			if err := d.Archive.Put(ctx, key, raw); err != nil {
				slog.Warn("pbp snapshot write failed", "gameId", gameID, "err", err)
			} else {
				state.SnapshotHashes["pbp"] = h
			}
		}
		if box, err := d.Feed.RawFeed(ctx, gameID, "boxscore"); err != nil {
			slog.Warn("boxscore fetch failed", "gameId", gameID, "err", err)
		} else if h := hashOf(box); h != state.SnapshotHashes["boxscore"] {
			key := store.SnapshotKey(pbp.Season, pbp.GameDate, gameID, "boxscore", d.Now())
			if err := d.Archive.Put(ctx, key, box); err != nil {
				slog.Warn("boxscore snapshot write failed", "gameId", gameID, "err", err)
			} else {
				state.SnapshotHashes["boxscore"] = h
			}
		}

		score := map[string]int{pbp.HomeTeam.Abbrev: pbp.HomeTeam.Score, pbp.AwayTeam.Abbrev: pbp.AwayTeam.Score}
		if pbp.GameState != state.GameState {
			err := d.Pub.Publish(ctx, events.DTStatus, events.StatusEvent{
				SchemaVersion: events.SchemaVersion, GameID: gameID,
				PrevState: state.GameState, GameState: pbp.GameState, Score: score,
			})
			if err == nil {
				state.GameState = pbp.GameState
			}
		}

		running := score
		for _, p := range NewPlays(pbp.Plays, state.LastPlaySortOrder) {
			running = RunningScore(pbp, p, running)
			if err := d.Pub.Publish(ctx, events.DTPlay, BuildPlayEvent(pbp, p, running)); err != nil {
				slog.Warn("play publish failed; will retry next cycle", "gameId", gameID, "seq", p.SortOrder, "err", err)
				break // do not advance the mark past a failed publish
			}
			state.LastPlaySortOrder = p.SortOrder
		}

		if err := d.Store.UpdatePollerState(ctx, gameID, state); err != nil {
			return 0, err
		}
		renewed, err := d.Store.RenewLease(ctx, gameID, owner, d.Now().Add(cfg.LeaseTTL))
		if err != nil {
			return 0, err
		}
		if !renewed {
			return OutcomeLeaseHeld, nil
		}

		if IsFinalState(pbp.GameState) {
			archiveFinal(ctx, d, pbp, gameID, raw)
			_ = d.Pub.Publish(ctx, events.DTFinal, events.FinalEvent{
				SchemaVersion: events.SchemaVersion, GameID: gameID,
				HomeTeam: pbp.HomeTeam.Abbrev, AwayTeam: pbp.AwayTeam.Abbrev,
				Score:    score,
				S3Prefix: store.GamePrefix(pbp.Season, pbp.GameDate, gameID),
			})
			state.Done = true
			if err := d.Store.UpdatePollerState(ctx, gameID, state); err != nil {
				return 0, err
			}
			_ = d.Store.ReleaseLease(ctx, gameID, owner)
			return OutcomeFinal, nil
		}

		interval := cfg.PregameInterval
		if IsLiveState(pbp.GameState) {
			interval = cfg.LiveInterval
		}
		if err := d.Sleep(ctx, interval); err != nil {
			return 0, err
		}
	}
}

// archiveFinal writes the end-of-game sweep; individual failures are logged,
// never fatal — the raw snapshots already preserve the game.
func archiveFinal(ctx context.Context, d Deps, pbp *nhl.PlayByPlay, gameID int64, finalPBP []byte) {
	put := func(feed string, body []byte, err error) {
		if err != nil {
			slog.Warn("final sweep fetch failed", "gameId", gameID, "feed", feed, "err", err)
			return
		}
		key := store.FinalKey(pbp.Season, pbp.GameDate, gameID, feed)
		if err := d.Archive.Put(ctx, key, body); err != nil {
			slog.Warn("final sweep write failed", "gameId", gameID, "feed", feed, "err", err)
		}
	}
	put("pbp", finalPBP, nil)
	box, err := d.Feed.RawFeed(ctx, gameID, "boxscore")
	put("boxscore", box, err)
	landing, err := d.Feed.RawFeed(ctx, gameID, "landing")
	put("landing", landing, err)
	shifts, err := d.Feed.ShiftCharts(ctx, gameID)
	put("shifts", shifts, err)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/poller/ -v`
Expected: PASS (all six tests). Debug any failure by printing `pub.Published` detail-types in order.

- [ ] **Step 5: Commit**

```bash
git add internal/poller/
git commit -m "feat: runtime-agnostic poller loop with lease, handoff, and final sweep"
```

---

### Task 8: schedule-sync

**Files:**
- Create: `internal/schedsync/schedsync.go`, `internal/schedsync/scheduler_aws.go`
- Test: `internal/schedsync/schedsync_test.go`

**Interfaces:**
- Consumes: `nhl.Client.Schedule` (via local interface), `store.GameStore`, `store.FakeGameStore`, `store.Archive`, `store.FakeArchive`, `store.ScheduleKey` (Tasks 1, 4, 5).
- Produces:
  - `ScheduleFeed` interface: `Schedule(ctx context.Context, date string) (*nhl.ScheduleResponse, []byte, error)`
  - `SchedulerAPI` interface:
    - `Ensure(ctx context.Context, name string, fireAt time.Time, gameID int64) error` — create-or-update a one-time, self-deleting schedule entry that invokes the poller with `{"gameId": N}`
    - `Delete(ctx context.Context, name string) error` — must tolerate not-found
  - `FakeScheduler{Entries map[string]FakeEntry}` where `FakeEntry{FireAt time.Time; GameID int64}`, `NewFakeScheduler()`
  - `EntryName(gameID int64) string` → `"hockeytrack-game-{gameID}"`
  - `Deps{Feed ScheduleFeed; Store store.GameStore; Archive store.Archive; Scheduler SchedulerAPI; Now func() time.Time}`
  - `Config{PregameBuffer time.Duration}` default 15 minutes
  - `Sync(ctx context.Context, d Deps, cfg Config, date string) error` — one call covers the week starting at `date` (the API returns `gameWeek`)
  - `NewAWSScheduler(client *scheduler.Client, group, targetArn, roleArn string) *AWSScheduler` implementing `SchedulerAPI`

**Behavior of `Sync`:**
1. Fetch schedule for `date`; archive raw body to `ScheduleKey(date)`.
2. For each game in every `gameWeek` day: build `GameRecord` (`GameDate` = the day's `Date`, abbrevs, venue, `StartTimeUTC`, `GameState`, `ScheduleEntryName = EntryName(id)`), `UpsertSchedule`.
3. If `GameScheduleState == "OK"` and game not final/done and `StartTimeUTC` is in the future: `Scheduler.Ensure(EntryName(id), StartTimeUTC − PregameBuffer, id)`. (Ensure is idempotent — safe to call daily; it updates fireAt on reschedules.)
4. If `GameScheduleState != "OK"` (postponed/cancelled): `Scheduler.Delete(EntryName(id))`.
5. Skip Ensure for games already started or final (their entry has already fired and self-deleted).

- [ ] **Step 1: Write the failing test**

`internal/schedsync/schedsync_test.go`:
```go
package schedsync

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"hockeytrack/internal/nhl"
	"hockeytrack/internal/store"
)

type fakeFeed struct{ body []byte }

func (f *fakeFeed) Schedule(_ context.Context, _ string) (*nhl.ScheduleResponse, []byte, error) {
	var s nhl.ScheduleResponse
	if err := json.Unmarshal(f.body, &s); err != nil {
		return nil, nil, err
	}
	return &s, f.body, nil
}

var syncNow = time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC)

func scheduleBody(t *testing.T, games ...map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"gameWeek": []map[string]any{{"date": "2026-01-15", "games": games}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func gameJSON(id int64, start time.Time, schedState string) map[string]any {
	return map[string]any{
		"id": id, "season": 20252026, "gameType": 2,
		"startTimeUTC": start.Format(time.RFC3339), "gameState": "FUT",
		"gameScheduleState": schedState,
		"venue":             map[string]any{"default": "Test Arena"},
		"awayTeam":          map[string]any{"id": 8, "abbrev": "MTL"},
		"homeTeam":          map[string]any{"id": 7, "abbrev": "BUF"},
	}
}

func testSync(t *testing.T, body []byte) (*store.FakeGameStore, *store.FakeArchive, *FakeScheduler) {
	t.Helper()
	gs := store.NewFakeGameStore()
	ar := store.NewFakeArchive()
	sched := NewFakeScheduler()
	d := Deps{
		Feed: &fakeFeed{body: body}, Store: gs, Archive: ar, Scheduler: sched,
		Now: func() time.Time { return syncNow },
	}
	if err := Sync(context.Background(), d, Config{PregameBuffer: 15 * time.Minute}, "2026-01-15"); err != nil {
		t.Fatal(err)
	}
	return gs, ar, sched
}

func TestSyncCreatesEntriesAndRecords(t *testing.T) {
	start := syncNow.Add(10 * time.Hour)
	gs, ar, sched := testSync(t, scheduleBody(t, gameJSON(101, start, "OK")))

	rec, _ := gs.Get(context.Background(), 101)
	if rec == nil {
		t.Fatal("game not recorded")
	}
	if rec.HomeAbbrev != "BUF" || rec.GameDate != "2026-01-15" || rec.ScheduleEntryName != "hockeytrack-game-101" {
		t.Errorf("record = %+v", rec)
	}
	e, ok := sched.Entries["hockeytrack-game-101"]
	if !ok {
		t.Fatal("no scheduler entry created")
	}
	if want := start.Add(-15 * time.Minute); !e.FireAt.Equal(want) {
		t.Errorf("fireAt = %v, want %v", e.FireAt, want)
	}
	if e.GameID != 101 {
		t.Errorf("entry gameID = %d", e.GameID)
	}
	if _, ok := ar.Objects["raw/schedule/2026-01-15.json"]; !ok {
		t.Error("raw schedule not archived")
	}
}

func TestSyncDeletesPostponedEntry(t *testing.T) {
	start := syncNow.Add(10 * time.Hour)
	// First sync: OK creates the entry. Second: PPD deletes it.
	gs := store.NewFakeGameStore()
	ar := store.NewFakeArchive()
	sched := NewFakeScheduler()
	d := Deps{Feed: &fakeFeed{body: scheduleBody(t, gameJSON(101, start, "OK"))}, Store: gs, Archive: ar, Scheduler: sched, Now: func() time.Time { return syncNow }}
	cfg := Config{PregameBuffer: 15 * time.Minute}
	if err := Sync(context.Background(), d, cfg, "2026-01-15"); err != nil {
		t.Fatal(err)
	}
	d.Feed = &fakeFeed{body: scheduleBody(t, gameJSON(101, start, "PPD"))}
	if err := Sync(context.Background(), d, cfg, "2026-01-15"); err != nil {
		t.Fatal(err)
	}
	if _, ok := sched.Entries["hockeytrack-game-101"]; ok {
		t.Error("postponed game's entry not deleted")
	}
}

func TestSyncSkipsStartedGames(t *testing.T) {
	past := syncNow.Add(-2 * time.Hour)
	_, _, sched := testSync(t, scheduleBody(t, gameJSON(102, past, "OK")))
	if _, ok := sched.Entries["hockeytrack-game-102"]; ok {
		t.Error("entry created for already-started game")
	}
}

func TestSyncRescheduleMovesEntry(t *testing.T) {
	start := syncNow.Add(10 * time.Hour)
	gs := store.NewFakeGameStore()
	sched := NewFakeScheduler()
	d := Deps{Feed: &fakeFeed{body: scheduleBody(t, gameJSON(101, start, "OK"))}, Store: gs, Archive: store.NewFakeArchive(), Scheduler: sched, Now: func() time.Time { return syncNow }}
	cfg := Config{PregameBuffer: 15 * time.Minute}
	Sync(context.Background(), d, cfg, "2026-01-15")
	moved := start.Add(3 * time.Hour)
	d.Feed = &fakeFeed{body: scheduleBody(t, gameJSON(101, moved, "OK"))}
	Sync(context.Background(), d, cfg, "2026-01-15")
	if got := sched.Entries["hockeytrack-game-101"].FireAt; !got.Equal(moved.Add(-15 * time.Minute)) {
		t.Errorf("fireAt = %v, want %v", got, moved.Add(-15*time.Minute))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/schedsync/ -v`
Expected: compile FAIL (`undefined: Sync`)

- [ ] **Step 3: Write implementation**

`internal/schedsync/schedsync.go`:
```go
// Package schedsync pulls the NHL schedule and reconciles per-game
// EventBridge Scheduler entries against it.
package schedsync

import (
	"context"
	"fmt"
	"sync"
	"time"

	"hockeytrack/internal/nhl"
	"hockeytrack/internal/store"
)

type ScheduleFeed interface {
	Schedule(ctx context.Context, date string) (*nhl.ScheduleResponse, []byte, error)
}

type SchedulerAPI interface {
	Ensure(ctx context.Context, name string, fireAt time.Time, gameID int64) error
	Delete(ctx context.Context, name string) error
}

type Deps struct {
	Feed      ScheduleFeed
	Store     store.GameStore
	Archive   store.Archive
	Scheduler SchedulerAPI
	Now       func() time.Time
}

type Config struct {
	PregameBuffer time.Duration
}

func EntryName(gameID int64) string {
	return fmt.Sprintf("hockeytrack-game-%d", gameID)
}

func Sync(ctx context.Context, d Deps, cfg Config, date string) error {
	sched, raw, err := d.Feed.Schedule(ctx, date)
	if err != nil {
		return err
	}
	if err := d.Archive.Put(ctx, store.ScheduleKey(date), raw); err != nil {
		return err
	}
	for _, day := range sched.GameWeek {
		for _, g := range day.Games {
			name := EntryName(g.ID)
			rec := store.GameRecord{
				GameID: g.ID, Season: g.Season, GameDate: day.Date,
				StartTimeUTC: g.StartTimeUTC,
				HomeAbbrev:   g.HomeTeam.Abbrev, AwayAbbrev: g.AwayTeam.Abbrev,
				Venue: g.Venue.Default, GameState: g.GameState, ScheduleEntryName: name,
			}
			if err := d.Store.UpsertSchedule(ctx, rec); err != nil {
				return err
			}
			switch {
			case g.GameScheduleState != "OK":
				if err := d.Scheduler.Delete(ctx, name); err != nil {
					return err
				}
			case g.StartTimeUTC.After(d.Now()) && !isFinal(g.GameState):
				if err := d.Scheduler.Ensure(ctx, name, g.StartTimeUTC.Add(-cfg.PregameBuffer), g.ID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func isFinal(s string) bool { return s == "FINAL" || s == "OFF" }

type FakeEntry struct {
	FireAt time.Time
	GameID int64
}

type FakeScheduler struct {
	mu      sync.Mutex
	Entries map[string]FakeEntry
}

func NewFakeScheduler() *FakeScheduler {
	return &FakeScheduler{Entries: map[string]FakeEntry{}}
}

func (f *FakeScheduler) Ensure(_ context.Context, name string, fireAt time.Time, gameID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Entries[name] = FakeEntry{FireAt: fireAt, GameID: gameID}
	return nil
}

func (f *FakeScheduler) Delete(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.Entries, name)
	return nil
}
```

`internal/schedsync/scheduler_aws.go`:
```go
package schedsync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	"github.com/aws/aws-sdk-go-v2/service/scheduler/types"
)

type AWSScheduler struct {
	client    *scheduler.Client
	group     string
	targetArn string // poller Lambda ARN
	roleArn   string // role EventBridge Scheduler assumes to invoke it
}

func NewAWSScheduler(client *scheduler.Client, group, targetArn, roleArn string) *AWSScheduler {
	return &AWSScheduler{client: client, group: group, targetArn: targetArn, roleArn: roleArn}
}

func (s *AWSScheduler) Ensure(ctx context.Context, name string, fireAt time.Time, gameID int64) error {
	target := &types.Target{
		Arn:     aws.String(s.targetArn),
		RoleArn: aws.String(s.roleArn),
		Input:   aws.String(fmt.Sprintf(`{"gameId":%d}`, gameID)),
	}
	in := scheduler.CreateScheduleInput{
		Name:                  aws.String(name),
		GroupName:             aws.String(s.group),
		ScheduleExpression:    aws.String("at(" + fireAt.UTC().Format("2006-01-02T15:04:05") + ")"),
		FlexibleTimeWindow:    &types.FlexibleTimeWindow{Mode: types.FlexibleTimeWindowModeOff},
		Target:                target,
		ActionAfterCompletion: types.ActionAfterCompletionDelete,
	}
	_, err := s.client.CreateSchedule(ctx, &in)
	var conflict *types.ConflictException
	if errors.As(err, &conflict) {
		_, err = s.client.UpdateSchedule(ctx, &scheduler.UpdateScheduleInput{
			Name: in.Name, GroupName: in.GroupName, ScheduleExpression: in.ScheduleExpression,
			FlexibleTimeWindow: in.FlexibleTimeWindow, Target: in.Target,
			ActionAfterCompletion: in.ActionAfterCompletion,
		})
	}
	return err
}

func (s *AWSScheduler) Delete(ctx context.Context, name string) error {
	_, err := s.client.DeleteSchedule(ctx, &scheduler.DeleteScheduleInput{
		Name: aws.String(name), GroupName: aws.String(s.group),
	})
	var nf *types.ResourceNotFoundException
	if errors.As(err, &nf) {
		return nil
	}
	return err
}
```
Then: `go get github.com/aws/aws-sdk-go-v2/service/scheduler`

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/schedsync/ -v && go build ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/schedsync/ go.mod go.sum
git commit -m "feat: schedule-sync with scheduler entry reconciliation"
```

---

### Task 9: Sweeper

**Files:**
- Create: `internal/sweeper/sweeper.go`
- Test: `internal/sweeper/sweeper_test.go`

**Interfaces:**
- Consumes: `store.GameStore`, `store.FakeGameStore` (Task 4).
- Produces:
  - `Invoker` interface: `InvokePoller(ctx context.Context, gameID int64) error` (async Lambda invoke)
  - `FakeInvoker{Invoked []int64}` implementing it
  - `Sweep(ctx context.Context, st store.GameStore, inv Invoker, now time.Time) error`
  - `NewLambdaInvoker(client *lambda.Client, functionName string) *LambdaInvoker` implementing `Invoker` — this same type is reused by the poller handler for self-chaining in Task 10.

**Behavior of `Sweep`:** query `ListByDate` for today's UTC date AND yesterday's (games cross midnight UTC). A game is a restart candidate iff: not `Done`, not final state, `now >= StartTimeUTC − 16m` (entry should have fired), `now <= StartTimeUTC + 6h` (give up window; the max-chain alert covers pathological games), and lease absent or expired (`LeaseExpiresAt.Before(now)`). Invoke the poller for each candidate. The lease itself guarantees a duplicate invoke is harmless (`OutcomeLeaseHeld`).

- [ ] **Step 1: Write the failing test**

`internal/sweeper/sweeper_test.go`:
```go
package sweeper

import (
	"context"
	"testing"
	"time"

	"hockeytrack/internal/store"
)

var now = time.Date(2026, 1, 16, 1, 0, 0, 0, time.UTC) // 8pm ET Jan 15

func seed(t *testing.T, gs *store.FakeGameStore, id int64, start time.Time, state string, done bool, leaseUntil time.Time) {
	t.Helper()
	ctx := context.Background()
	gs.UpsertSchedule(ctx, store.GameRecord{GameID: id, GameDate: start.UTC().Format("2006-01-02"), StartTimeUTC: start})
	gs.UpdatePollerState(ctx, id, store.PollerState{GameState: state, Done: done})
	if !leaseUntil.IsZero() {
		gs.AcquireLease(ctx, id, "someworker", leaseUntil)
	}
}

func TestSweepRestartsOnlyDeadLiveGames(t *testing.T) {
	gs := store.NewFakeGameStore()
	gs.Now = func() time.Time { return now }
	inv := &FakeInvoker{}

	gameStart := now.Add(-90 * time.Minute)
	seed(t, gs, 1, gameStart, "LIVE", false, time.Time{})           // dead: no lease -> restart
	seed(t, gs, 2, gameStart, "LIVE", false, now.Add(-time.Minute)) // dead: expired lease -> restart
	seed(t, gs, 3, gameStart, "LIVE", false, now.Add(time.Minute))  // healthy lease -> skip
	seed(t, gs, 4, gameStart, "OFF", true, time.Time{})             // done -> skip
	seed(t, gs, 5, now.Add(2*time.Hour), "FUT", false, time.Time{}) // not started -> skip
	seed(t, gs, 6, now.Add(-7*time.Hour), "LIVE", false, time.Time{}) // outside give-up window -> skip

	if err := Sweep(context.Background(), gs, inv, now); err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, id := range inv.Invoked {
		got[id] = true
	}
	if !got[1] || !got[2] {
		t.Errorf("dead games not restarted: invoked=%v", inv.Invoked)
	}
	if got[3] || got[4] || got[5] || got[6] {
		t.Errorf("healthy/done/future/expired games invoked: %v", inv.Invoked)
	}
}

func TestSweepCoversYesterdayUTC(t *testing.T) {
	// A 10pm ET game starts 03:00 UTC next day but its gameDate row may be
	// keyed to the previous UTC date after midnight; sweep must check both days.
	gs := store.NewFakeGameStore()
	gs.Now = func() time.Time { return now }
	inv := &FakeInvoker{}
	yesterdayStart := now.Add(-2 * time.Hour) // Jan 15 UTC date, now is Jan 16 UTC
	seed(t, gs, 7, yesterdayStart, "LIVE", false, time.Time{})
	if err := Sweep(context.Background(), gs, inv, now); err != nil {
		t.Fatal(err)
	}
	if len(inv.Invoked) != 1 || inv.Invoked[0] != 7 {
		t.Errorf("yesterday's live game not restarted: %v", inv.Invoked)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/sweeper/ -v`
Expected: compile FAIL (`undefined: Sweep`)

- [ ] **Step 3: Write implementation**

`internal/sweeper/sweeper.go`:
```go
// Package sweeper restarts poller chains that died mid-game, detected via
// expired leases.
package sweeper

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awslambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"

	"hockeytrack/internal/store"
)

type Invoker interface {
	InvokePoller(ctx context.Context, gameID int64) error
}

type FakeInvoker struct {
	Invoked []int64
	Err     error
}

func (f *FakeInvoker) InvokePoller(_ context.Context, gameID int64) error {
	if f.Err != nil {
		return f.Err
	}
	f.Invoked = append(f.Invoked, gameID)
	return nil
}

func Sweep(ctx context.Context, st store.GameStore, inv Invoker, now time.Time) error {
	dates := []string{
		now.UTC().Format("2006-01-02"),
		now.UTC().AddDate(0, 0, -1).Format("2006-01-02"),
	}
	for _, date := range dates {
		games, err := st.ListByDate(ctx, date)
		if err != nil {
			return err
		}
		for _, g := range games {
			if g.Done || g.GameState == "FINAL" || g.GameState == "OFF" {
				continue
			}
			if now.Before(g.StartTimeUTC.Add(-16 * time.Minute)) {
				continue // entry hasn't fired yet
			}
			if now.After(g.StartTimeUTC.Add(6 * time.Hour)) {
				continue // give up; max-chain alerting covers pathological games
			}
			if g.LeaseOwner != "" && g.LeaseExpiresAt.After(now) {
				continue // a poller link is alive
			}
			slog.Info("sweeper restarting poller", "gameId", g.GameID, "state", g.GameState)
			if err := inv.InvokePoller(ctx, g.GameID); err != nil {
				return err
			}
		}
	}
	return nil
}

type LambdaInvoker struct {
	client       *awslambda.Client
	functionName string
}

func NewLambdaInvoker(client *awslambda.Client, functionName string) *LambdaInvoker {
	return &LambdaInvoker{client: client, functionName: functionName}
}

func (l *LambdaInvoker) InvokePoller(ctx context.Context, gameID int64) error {
	payload, _ := json.Marshal(map[string]int64{"gameId": gameID})
	out, err := l.client.Invoke(ctx, &awslambda.InvokeInput{
		FunctionName:   aws.String(l.functionName),
		InvocationType: lambdatypes.InvocationTypeEvent,
		Payload:        payload,
	})
	if err != nil {
		return err
	}
	if out.StatusCode < 200 || out.StatusCode >= 300 {
		return fmt.Errorf("async invoke status %d", out.StatusCode)
	}
	return nil
}
```
Then: `go get github.com/aws/aws-sdk-go-v2/service/lambda`

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/sweeper/ -v && go build ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/sweeper/ go.mod go.sum
git commit -m "feat: sweeper restarts dead poller chains via lease expiry"
```

---

### Task 10: Entrypoint — MODE dispatch and Lambda handlers

**Files:**
- Create: `cmd/ingestor/main.go`, `cmd/ingestor/handlers.go`
- Test: `cmd/ingestor/handlers_test.go`

**Interfaces:**
- Consumes: everything above. `poller.Run`, `schedsync.Sync`, `sweeper.Sweep`, `sweeper.Invoker`/`LambdaInvoker`, real AWS constructors.
- Produces: the deployable binary. Env contract (read at cold start):
  - `MODE` = `schedule-sync` | `poller` | `sweeper` (required)
  - `GAMES_TABLE`, `RAW_BUCKET`, `EVENT_BUS` (required)
  - `SCHEDULER_GROUP`, `POLLER_FUNCTION_ARN`, `SCHEDULER_ROLE_ARN` (schedule-sync only)
  - `POLLER_FUNCTION_NAME` (poller self-chain + sweeper)
  - `POLL_INTERVAL_SECONDS` (default 5), `PREGAME_INTERVAL_SECONDS` (default 30), `MAX_CHAINS` (default 30), `HANDOFF_BUFFER_SECONDS` (default 60)
  - Poller invoke payload: `{"gameId": <int64>}`

- [ ] **Step 1: Write the failing test** (pure logic only — the handoff predicate and payload parsing; handler wiring is compile-checked)

`cmd/ingestor/handlers_test.go`:
```go
package main

import (
	"context"
	"testing"
	"time"
)

func TestParsePollerPayload(t *testing.T) {
	id, err := parsePollerPayload([]byte(`{"gameId":2025020740}`))
	if err != nil || id != 2025020740 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	if _, err := parsePollerPayload([]byte(`{}`)); err == nil {
		t.Error("missing gameId should error")
	}
	if _, err := parsePollerPayload([]byte(`not json`)); err == nil {
		t.Error("bad json should error")
	}
}

func TestHandoffPredicate(t *testing.T) {
	deadline := time.Date(2026, 1, 15, 23, 15, 0, 0, time.UTC)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	early := func() time.Time { return deadline.Add(-5 * time.Minute) }
	late := func() time.Time { return deadline.Add(-30 * time.Second) }

	if shouldHandOff(ctx, 60*time.Second, early)() {
		t.Error("handed off with 5 minutes remaining")
	}
	if !shouldHandOff(ctx, 60*time.Second, late)() {
		t.Error("did not hand off with 30s remaining")
	}
	// No deadline (local runs): never hand off.
	if shouldHandOff(context.Background(), 60*time.Second, early)() {
		t.Error("handed off without a deadline")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/ingestor/ -v`
Expected: compile FAIL (`undefined: parsePollerPayload`)

- [ ] **Step 3: Write implementation**

`cmd/ingestor/handlers.go`:
```go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

func parsePollerPayload(b []byte) (int64, error) {
	var p struct {
		GameID int64 `json:"gameId"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return 0, err
	}
	if p.GameID == 0 {
		return 0, errors.New("payload missing gameId")
	}
	return p.GameID, nil
}

// shouldHandOff returns a predicate that is true when the context deadline is
// within buffer. With no deadline (local/ECS runs) it never hands off.
func shouldHandOff(ctx context.Context, buffer time.Duration, now func() time.Time) func() bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return func() bool { return false }
	}
	return func() bool { return now().Add(buffer).After(deadline) }
}
```

`cmd/ingestor/main.go`:
```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-lambda-go/lambdacontext"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	awslambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"

	"hockeytrack/internal/events"
	"hockeytrack/internal/nhl"
	"hockeytrack/internal/poller"
	"hockeytrack/internal/schedsync"
	"hockeytrack/internal/store"
	"hockeytrack/internal/sweeper"
)

type app struct {
	nhl      *nhl.Client
	store    *store.DynamoStore
	archive  *store.S3Archive
	pub      *events.EventBridgePublisher
	lambdaCl *awslambdasvc.Client
	schedCl  *scheduler.Client
	pollCfg  poller.Config
	handoff  time.Duration
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envSeconds(key string, def int) time.Duration {
	n, err := strconv.Atoi(envOr(key, strconv.Itoa(def)))
	if err != nil {
		n = def
	}
	return time.Duration(n) * time.Second
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		slog.Error("missing required env var", "key", key)
		os.Exit(1)
	}
	return v
}

func newApp(ctx context.Context) (*app, error) {
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	maxChains, _ := strconv.Atoi(envOr("MAX_CHAINS", "30"))
	return &app{
		nhl:      nhl.New(),
		store:    store.NewDynamoStore(dynamodb.NewFromConfig(awsCfg), mustEnv("GAMES_TABLE")),
		archive:  store.NewS3Archive(s3.NewFromConfig(awsCfg), mustEnv("RAW_BUCKET")),
		pub:      events.NewEventBridgePublisher(eventbridge.NewFromConfig(awsCfg), mustEnv("EVENT_BUS")),
		lambdaCl: awslambdasvc.NewFromConfig(awsCfg),
		schedCl:  scheduler.NewFromConfig(awsCfg),
		pollCfg: poller.Config{
			LiveInterval:    envSeconds("POLL_INTERVAL_SECONDS", 5),
			PregameInterval: envSeconds("PREGAME_INTERVAL_SECONDS", 30),
			LeaseTTL:        60 * time.Second,
			MaxChains:       maxChains,
		},
		handoff: envSeconds("HANDOFF_BUFFER_SECONDS", 60),
	}, nil
}

func (a *app) pollerDeps() poller.Deps {
	return poller.Deps{
		Feed: a.nhl, Store: a.store, Archive: a.archive, Pub: a.pub,
		Now: time.Now,
		Sleep: func(ctx context.Context, d time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
				return nil
			}
		},
	}
}

func (a *app) handlePoller(ctx context.Context, raw json.RawMessage) error {
	gameID, err := parsePollerPayload(raw)
	if err != nil {
		return err
	}
	owner := "local"
	if lc, ok := lambdacontext.FromContext(ctx); ok {
		owner = lc.AwsRequestID
	}
	outcome, err := poller.Run(ctx, a.pollerDeps(), a.pollCfg, gameID, owner, shouldHandOff(ctx, a.handoff, time.Now))
	if err != nil {
		return err
	}
	slog.Info("poller finished", "gameId", gameID, "outcome", outcome)
	if outcome == poller.OutcomeHandOff {
		inv := sweeper.NewLambdaInvoker(a.lambdaCl, mustEnv("POLLER_FUNCTION_NAME"))
		return inv.InvokePoller(ctx, gameID)
	}
	return nil
}

func (a *app) handleScheduleSync(ctx context.Context) error {
	d := schedsync.Deps{
		Feed: a.nhl, Store: a.store, Archive: a.archive,
		Scheduler: schedsync.NewAWSScheduler(a.schedCl, mustEnv("SCHEDULER_GROUP"), mustEnv("POLLER_FUNCTION_ARN"), mustEnv("SCHEDULER_ROLE_ARN")),
		Now:       time.Now,
	}
	return schedsync.Sync(ctx, d, schedsync.Config{PregameBuffer: 15 * time.Minute}, time.Now().UTC().Format("2006-01-02"))
}

func (a *app) handleSweeper(ctx context.Context) error {
	inv := sweeper.NewLambdaInvoker(a.lambdaCl, mustEnv("POLLER_FUNCTION_NAME"))
	return sweeper.Sweep(ctx, a.store, inv, time.Now())
}

func main() {
	ctx := context.Background()
	a, err := newApp(ctx)
	if err != nil {
		slog.Error("init failed", "err", err)
		os.Exit(1)
	}
	switch mode := mustEnv("MODE"); mode {
	case "poller":
		lambda.Start(a.handlePoller)
	case "schedule-sync":
		lambda.Start(func(ctx context.Context) error { return a.handleScheduleSync(ctx) })
	case "sweeper":
		lambda.Start(func(ctx context.Context) error { return a.handleSweeper(ctx) })
	default:
		fmt.Fprintf(os.Stderr, "unknown MODE %q\n", mode)
		os.Exit(1)
	}
}
```
Then: `go get github.com/aws/aws-lambda-go/lambda github.com/aws/aws-lambda-go/lambdacontext`

- [ ] **Step 4: Run tests and build**

Run: `go test ./... && go build ./...`
Expected: all packages PASS, clean build

- [ ] **Step 5: Commit**

```bash
git add cmd/ go.mod go.sum
git commit -m "feat: ingestor entrypoint with MODE dispatch and Lambda handlers"
```

---

### Task 11: Replay harness

**Files:**
- Create: `cmd/replay/main.go`
- Test: none (it IS a test tool; verified by running it)

**Interfaces:**
- Consumes: `poller.Run`, `poller.Feed`, fakes from `store` and `events`, `nhl` types.
- Produces: `go run ./cmd/replay -game <dir>` where `<dir>` contains numbered play-by-play snapshots (`01.json`, `02.json`, ...); replays them through the full poller path with fakes and prints each published event as one JSON line. Exit code 0 iff the run reaches `OutcomeFinal`.

- [ ] **Step 1: Write the harness**

`cmd/replay/main.go`:
```go
// Replay pushes a directory of recorded play-by-play snapshots through the
// full poller path against in-memory fakes, printing every published event.
// Usage: go run ./cmd/replay -game path/to/snapshots/
// Snapshot files are replayed in lexical order; the last must be a final state.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"hockeytrack/internal/events"
	"hockeytrack/internal/nhl"
	"hockeytrack/internal/poller"
	"hockeytrack/internal/store"
)

type dirFeed struct {
	bodies [][]byte
	i      int
}

func (f *dirFeed) PlayByPlay(_ context.Context, _ int64) (*nhl.PlayByPlay, []byte, error) {
	raw := f.bodies[f.i]
	if f.i < len(f.bodies)-1 {
		f.i++
	}
	var p nhl.PlayByPlay
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, nil, err
	}
	return &p, raw, nil
}

func (f *dirFeed) RawFeed(_ context.Context, _ int64, feed string) ([]byte, error) {
	return []byte(fmt.Sprintf(`{"replayStub":%q}`, feed)), nil
}

func (f *dirFeed) ShiftCharts(_ context.Context, _ int64) ([]byte, error) {
	return []byte(`{"replayStub":"shifts"}`), nil
}

type printingPub struct{ n int }

func (p *printingPub) Publish(_ context.Context, dt string, detail any) error {
	b, err := json.Marshal(map[string]any{"detailType": dt, "detail": detail})
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	p.n++
	return nil
}

func main() {
	dir := flag.String("game", "", "directory of pbp snapshot JSON files")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "usage: replay -game <snapshot dir>")
		os.Exit(2)
	}
	entries, err := filepath.Glob(filepath.Join(*dir, "*.json"))
	if err != nil || len(entries) == 0 {
		fmt.Fprintf(os.Stderr, "no snapshots in %s\n", *dir)
		os.Exit(2)
	}
	sort.Strings(entries)
	feed := &dirFeed{}
	for _, e := range entries {
		b, err := os.ReadFile(e)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		feed.bodies = append(feed.bodies, b)
	}
	var first nhl.PlayByPlay
	json.Unmarshal(feed.bodies[0], &first)

	gs := store.NewFakeGameStore()
	gs.UpsertSchedule(context.Background(), store.GameRecord{
		GameID: first.ID, Season: first.Season, GameDate: first.GameDate,
		HomeAbbrev: first.HomeTeam.Abbrev, AwayAbbrev: first.AwayTeam.Abbrev,
		GameState: "FUT", StartTimeUTC: time.Now(),
	})
	pub := &printingPub{}
	deps := poller.Deps{
		Feed: feed, Store: gs, Archive: store.NewFakeArchive(), Pub: pub,
		Now:   time.Now,
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	outcome, err := poller.Run(context.Background(), deps, poller.DefaultConfig(), first.ID, "replay", func() bool { return false })
	if err != nil {
		fmt.Fprintln(os.Stderr, "replay error:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "replay done: outcome=%d events=%d\n", outcome, pub.n)
	if outcome != poller.OutcomeFinal {
		os.Exit(1)
	}
}
```

- [ ] **Step 2: Verify it works against the fixture**

```bash
mkdir -p /tmp/claude-1000/-home-jay-projects-hockeytrack/0f605f9c-81e7-48f5-b69e-1002bb76f3d1/scratchpad/replaygame
python3 - <<'EOF'
import json
src = json.load(open('internal/nhl/testdata/pbp.json'))
out = '/tmp/claude-1000/-home-jay-projects-hockeytrack/0f605f9c-81e7-48f5-b69e-1002bb76f3d1/scratchpad/replaygame'
plays = src['plays']
for i, (n, state) in enumerate([(30, 'LIVE'), (120, 'LIVE'), (250, 'LIVE'), (len(plays), 'OFF')]):
    snap = dict(src)
    snap['plays'] = plays[:n]
    snap['gameState'] = state
    json.dump(snap, open(f'{out}/{i:02d}.json', 'w'))
EOF
go run ./cmd/replay -game /tmp/claude-1000/-home-jay-projects-hockeytrack/0f605f9c-81e7-48f5-b69e-1002bb76f3d1/scratchpad/replaygame | tail -3
```
Expected: JSON event lines ending with an `nhl.game.final` event; stderr reports `outcome=0` (OutcomeFinal) and events ≈ play count + status changes + final. Exit code 0 (`echo $?`).

- [ ] **Step 3: Commit**

```bash
git add cmd/replay/
git commit -m "feat: replay harness for end-to-end poller validation"
```

---

### Task 12: Dockerfile + Makefile

**Files:**
- Create: `Dockerfile`, `Makefile`

**Interfaces:**
- Consumes: the `cmd/ingestor` binary.
- Produces: image runnable by Lambda (uses `provided.al2023`-compatible static binary with aws-lambda-go's normal entrypoint); `make test`, `make build`, `make push`, `make deploy` targets. Terraform (Tasks 13–14) consumes the pushed image URI via `image_tag` variable.

- [ ] **Step 1: Write Dockerfile**

```dockerfile
FROM golang:1.23 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o /ingestor ./cmd/ingestor

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /ingestor /ingestor
ENTRYPOINT ["/ingestor"]
```

- [ ] **Step 2: Write Makefile**

```makefile
REGION      ?= us-east-1
ACCOUNT_ID  ?= $(shell aws sts get-caller-identity --query Account --output text)
REPO        := hockeytrack
TAG         ?= $(shell git rev-parse --short HEAD)
IMAGE       := $(ACCOUNT_ID).dkr.ecr.$(REGION).amazonaws.com/$(REPO):$(TAG)

.PHONY: test build push deploy

test:
	go test ./...

build: test
	docker build --platform linux/arm64 -t $(IMAGE) .

push: build
	aws ecr get-login-password --region $(REGION) | docker login --username AWS --password-stdin $(ACCOUNT_ID).dkr.ecr.$(REGION).amazonaws.com
	docker push $(IMAGE)

deploy: push
	cd terraform && terraform apply -var="image_tag=$(TAG)" -auto-approve
```

- [ ] **Step 3: Verify the image builds**

Run: `docker build --platform linux/arm64 -t hockeytrack:local .`
Expected: successful build. (If Docker is unavailable in the environment, note it and verify `go build` cross-compiles: `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /dev/null ./cmd/ingestor` — then flag the unbuilt image to the user at the end.)

- [ ] **Step 4: Commit**

```bash
git add Dockerfile Makefile
git commit -m "chore: container image and build targets"
```

---

### Task 13: Terraform — data plane (ECR, S3, DynamoDB, bus, SNS)

**Files:**
- Create: `terraform/providers.tf`, `terraform/variables.tf`, `terraform/data.tf`, `terraform/ecr.tf`, `terraform/s3.tf`, `terraform/dynamodb.tf`, `terraform/eventbridge.tf`, `terraform/sns.tf`, `terraform/outputs.tf`

**Interfaces:**
- Consumes: nothing (first infra task).
- Produces: resources referenced by Task 14 with these exact Terraform names: `aws_ecr_repository.main`, `aws_s3_bucket.raw`, `aws_dynamodb_table.games` (GSI name `byGameDate`), `aws_cloudwatch_event_bus.main` (bus name `hockeytrack`), `aws_sns_topic.alerts`. Variables: `region` (default `us-east-1`), `image_tag` (string, no default), `alert_email` (string, default `""`).

- [ ] **Step 1: Write the files**

`terraform/providers.tf`:
```hcl
terraform {
  required_version = ">= 1.6"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  region = var.region
}
```

`terraform/variables.tf`:
```hcl
variable "region" {
  type    = string
  default = "us-east-1"
}

variable "image_tag" {
  type        = string
  description = "ECR image tag (git SHA) all three Lambdas run"
}

variable "alert_email" {
  type        = string
  default     = ""
  description = "Email for CloudWatch alarm notifications; empty skips the subscription"
}
```

`terraform/data.tf`:
```hcl
data "aws_caller_identity" "current" {}
```

`terraform/ecr.tf`:
```hcl
resource "aws_ecr_repository" "main" {
  name                 = "hockeytrack"
  image_tag_mutability = "IMMUTABLE"
  force_delete         = true
}
```

`terraform/s3.tf`:
```hcl
resource "aws_s3_bucket" "raw" {
  bucket = "hockeytrack-raw-${data.aws_caller_identity.current.account_id}"
}

resource "aws_s3_bucket_public_access_block" "raw" {
  bucket                  = aws_s3_bucket.raw.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}
```

`terraform/dynamodb.tf`:
```hcl
resource "aws_dynamodb_table" "games" {
  name         = "hockeytrack-games"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "gameId"

  attribute {
    name = "gameId"
    type = "N"
  }

  attribute {
    name = "gameDate"
    type = "S"
  }

  global_secondary_index {
    name            = "byGameDate"
    hash_key        = "gameDate"
    projection_type = "ALL"
  }
}
```

`terraform/eventbridge.tf`:
```hcl
resource "aws_cloudwatch_event_bus" "main" {
  name = "hockeytrack"
}
```

`terraform/sns.tf`:
```hcl
resource "aws_sns_topic" "alerts" {
  name = "hockeytrack-alerts"
}

resource "aws_sns_topic_subscription" "email" {
  count     = var.alert_email == "" ? 0 : 1
  topic_arn = aws_sns_topic.alerts.arn
  protocol  = "email"
  endpoint  = var.alert_email
}
```

`terraform/outputs.tf`:
```hcl
output "raw_bucket" {
  value = aws_s3_bucket.raw.bucket
}

output "event_bus" {
  value = aws_cloudwatch_event_bus.main.name
}

output "ecr_repository_url" {
  value = aws_ecr_repository.main.repository_url
}
```

- [ ] **Step 2: Validate**

Run: `cd terraform && terraform init -backend=false && terraform validate`
Expected: `Success! The configuration is valid.`

- [ ] **Step 3: Commit**

```bash
git add terraform/
git commit -m "infra: data plane - ECR, S3, DynamoDB, event bus, SNS"
```

---

### Task 14: Terraform — Lambdas, scheduler, IAM, DLQs, alarms

**Files:**
- Create: `terraform/lambda.tf`, `terraform/scheduler.tf`, `terraform/iam.tf`, `terraform/dlq.tf`, `terraform/alarms.tf`

**Interfaces:**
- Consumes: Task 13 resource names, the env-var contract from Task 10, `EntryName` prefix `hockeytrack-game-` and payload `{"gameId":N}` from Task 8.
- Produces: three `aws_lambda_function`s (`hockeytrack-schedule-sync`, `hockeytrack-poller`, `hockeytrack-sweeper`) on the same image; scheduler group `hockeytrack-games`; daily cron + 5-minute sweeper rule; per-function DLQs and alarms.

- [ ] **Step 1: Write the files**

`terraform/lambda.tf`:
```hcl
locals {
  image_uri = "${aws_ecr_repository.main.repository_url}:${var.image_tag}"
  common_env = {
    GAMES_TABLE = aws_dynamodb_table.games.name
    RAW_BUCKET  = aws_s3_bucket.raw.bucket
    EVENT_BUS   = aws_cloudwatch_event_bus.main.name
  }
}

resource "aws_lambda_function" "schedule_sync" {
  function_name = "hockeytrack-schedule-sync"
  package_type  = "Image"
  image_uri     = local.image_uri
  role          = aws_iam_role.schedule_sync.arn
  architectures = ["arm64"]
  timeout       = 120
  memory_size   = 256

  environment {
    variables = merge(local.common_env, {
      MODE                = "schedule-sync"
      SCHEDULER_GROUP     = aws_scheduler_schedule_group.games.name
      POLLER_FUNCTION_ARN = aws_lambda_function.poller.arn
      SCHEDULER_ROLE_ARN  = aws_iam_role.scheduler_invoke.arn
    })
  }

  dead_letter_config {
    target_arn = aws_sqs_queue.dlq["schedule-sync"].arn
  }
}

resource "aws_lambda_function" "poller" {
  function_name = "hockeytrack-poller"
  package_type  = "Image"
  image_uri     = local.image_uri
  role          = aws_iam_role.poller.arn
  architectures = ["arm64"]
  timeout       = 900
  memory_size   = 256
  # One chain per game; modest headroom for simultaneous games league-wide.
  reserved_concurrent_executions = 20

  environment {
    variables = merge(local.common_env, {
      MODE                 = "poller"
      POLLER_FUNCTION_NAME = "hockeytrack-poller"
    })
  }

  dead_letter_config {
    target_arn = aws_sqs_queue.dlq["poller"].arn
  }
}

resource "aws_lambda_function" "sweeper" {
  function_name = "hockeytrack-sweeper"
  package_type  = "Image"
  image_uri     = local.image_uri
  role          = aws_iam_role.sweeper.arn
  architectures = ["arm64"]
  timeout       = 60
  memory_size   = 256

  environment {
    variables = merge(local.common_env, {
      MODE                 = "sweeper"
      POLLER_FUNCTION_NAME = aws_lambda_function.poller.function_name
    })
  }

  dead_letter_config {
    target_arn = aws_sqs_queue.dlq["sweeper"].arn
  }
}
```

`terraform/scheduler.tf`:
```hcl
resource "aws_scheduler_schedule_group" "games" {
  name = "hockeytrack-games"
}

# Daily schedule-sync at 09:00 UTC (~4am ET).
resource "aws_scheduler_schedule" "daily_sync" {
  name                = "hockeytrack-daily-sync"
  group_name          = aws_scheduler_schedule_group.games.name
  schedule_expression = "cron(0 9 * * ? *)"

  flexible_time_window {
    mode = "OFF"
  }

  target {
    arn      = aws_lambda_function.schedule_sync.arn
    role_arn = aws_iam_role.scheduler_invoke_sync.arn
  }
}

# Sweeper every 5 minutes.
resource "aws_scheduler_schedule" "sweeper" {
  name                = "hockeytrack-sweeper"
  group_name          = aws_scheduler_schedule_group.games.name
  schedule_expression = "rate(5 minutes)"

  flexible_time_window {
    mode = "OFF"
  }

  target {
    arn      = aws_lambda_function.sweeper.arn
    role_arn = aws_iam_role.scheduler_invoke_sweeper.arn
  }
}
```

`terraform/iam.tf`:
```hcl
data "aws_iam_policy_document" "lambda_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

data "aws_iam_policy_document" "scheduler_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["scheduler.amazonaws.com"]
    }
  }
}

locals {
  logs_statement = {
    actions   = ["logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["arn:aws:logs:*:*:*"]
  }
}

# ---- schedule-sync ----
resource "aws_iam_role" "schedule_sync" {
  name               = "hockeytrack-schedule-sync"
  assume_role_policy = data.aws_iam_policy_document.lambda_assume.json
}

data "aws_iam_policy_document" "schedule_sync" {
  statement {
    actions   = local.logs_statement.actions
    resources = local.logs_statement.resources
  }
  statement {
    actions   = ["dynamodb:UpdateItem", "dynamodb:GetItem", "dynamodb:Query"]
    resources = [aws_dynamodb_table.games.arn, "${aws_dynamodb_table.games.arn}/index/*"]
  }
  statement {
    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.raw.arn}/raw/schedule/*"]
  }
  statement {
    actions   = ["scheduler:CreateSchedule", "scheduler:UpdateSchedule", "scheduler:DeleteSchedule", "scheduler:GetSchedule"]
    resources = ["arn:aws:scheduler:${var.region}:${data.aws_caller_identity.current.account_id}:schedule/${aws_scheduler_schedule_group.games.name}/*"]
  }
  statement {
    actions   = ["iam:PassRole"]
    resources = [aws_iam_role.scheduler_invoke.arn]
  }
  statement {
    actions   = ["sqs:SendMessage"]
    resources = [aws_sqs_queue.dlq["schedule-sync"].arn]
  }
}

resource "aws_iam_role_policy" "schedule_sync" {
  role   = aws_iam_role.schedule_sync.id
  policy = data.aws_iam_policy_document.schedule_sync.json
}

# ---- poller ----
resource "aws_iam_role" "poller" {
  name               = "hockeytrack-poller"
  assume_role_policy = data.aws_iam_policy_document.lambda_assume.json
}

data "aws_iam_policy_document" "poller" {
  statement {
    actions   = local.logs_statement.actions
    resources = local.logs_statement.resources
  }
  statement {
    actions   = ["dynamodb:UpdateItem", "dynamodb:GetItem", "dynamodb:Query"]
    resources = [aws_dynamodb_table.games.arn, "${aws_dynamodb_table.games.arn}/index/*"]
  }
  statement {
    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.raw.arn}/raw/*"]
  }
  statement {
    actions   = ["events:PutEvents"]
    resources = [aws_cloudwatch_event_bus.main.arn]
  }
  statement {
    actions   = ["lambda:InvokeFunction"]
    resources = ["arn:aws:lambda:${var.region}:${data.aws_caller_identity.current.account_id}:function:hockeytrack-poller"]
  }
  statement {
    actions   = ["sqs:SendMessage"]
    resources = [aws_sqs_queue.dlq["poller"].arn]
  }
}

resource "aws_iam_role_policy" "poller" {
  role   = aws_iam_role.poller.id
  policy = data.aws_iam_policy_document.poller.json
}

# ---- sweeper ----
resource "aws_iam_role" "sweeper" {
  name               = "hockeytrack-sweeper"
  assume_role_policy = data.aws_iam_policy_document.lambda_assume.json
}

data "aws_iam_policy_document" "sweeper" {
  statement {
    actions   = local.logs_statement.actions
    resources = local.logs_statement.resources
  }
  statement {
    actions   = ["dynamodb:GetItem", "dynamodb:Query"]
    resources = [aws_dynamodb_table.games.arn, "${aws_dynamodb_table.games.arn}/index/*"]
  }
  statement {
    actions   = ["lambda:InvokeFunction"]
    resources = [aws_lambda_function.poller.arn]
  }
  statement {
    actions   = ["sqs:SendMessage"]
    resources = [aws_sqs_queue.dlq["sweeper"].arn]
  }
}

resource "aws_iam_role_policy" "sweeper" {
  role   = aws_iam_role.sweeper.id
  policy = data.aws_iam_policy_document.sweeper.json
}

# ---- roles EventBridge Scheduler assumes ----
resource "aws_iam_role" "scheduler_invoke" {
  name               = "hockeytrack-scheduler-invoke"
  assume_role_policy = data.aws_iam_policy_document.scheduler_assume.json
}

resource "aws_iam_role_policy" "scheduler_invoke" {
  role = aws_iam_role.scheduler_invoke.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = "lambda:InvokeFunction"
      Resource = "arn:aws:lambda:${var.region}:${data.aws_caller_identity.current.account_id}:function:hockeytrack-poller"
    }]
  })
}

resource "aws_iam_role" "scheduler_invoke_sync" {
  name               = "hockeytrack-scheduler-invoke-sync"
  assume_role_policy = data.aws_iam_policy_document.scheduler_assume.json
}

resource "aws_iam_role_policy" "scheduler_invoke_sync" {
  role = aws_iam_role.scheduler_invoke_sync.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = "lambda:InvokeFunction"
      Resource = aws_lambda_function.schedule_sync.arn
    }]
  })
}

resource "aws_iam_role" "scheduler_invoke_sweeper" {
  name               = "hockeytrack-scheduler-invoke-sweeper"
  assume_role_policy = data.aws_iam_policy_document.scheduler_assume.json
}

resource "aws_iam_role_policy" "scheduler_invoke_sweeper" {
  role = aws_iam_role.scheduler_invoke_sweeper.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = "lambda:InvokeFunction"
      Resource = aws_lambda_function.sweeper.arn
    }]
  })
}
```

`terraform/dlq.tf`:
```hcl
resource "aws_sqs_queue" "dlq" {
  for_each                  = toset(["schedule-sync", "poller", "sweeper"])
  name                      = "hockeytrack-${each.key}-dlq"
  message_retention_seconds = 1209600 # 14 days
}
```

`terraform/alarms.tf`:
```hcl
resource "aws_cloudwatch_metric_alarm" "dlq_depth" {
  for_each            = aws_sqs_queue.dlq
  alarm_name          = "hockeytrack-${each.key}-dlq-depth"
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  dimensions          = { QueueName = each.value.name }
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  alarm_actions       = [aws_sns_topic.alerts.arn]
}

resource "aws_cloudwatch_metric_alarm" "poller_errors" {
  alarm_name          = "hockeytrack-poller-errors"
  namespace           = "AWS/Lambda"
  metric_name         = "Errors"
  dimensions          = { FunctionName = aws_lambda_function.poller.function_name }
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 3
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alerts.arn]
}
```

Note: `aws_lambda_function.schedule_sync` references `aws_lambda_function.poller.arn` in its environment — Terraform resolves this ordering automatically (poller is created first). There is no cycle because poller's env uses the literal function name, not a reference to schedule_sync.

- [ ] **Step 2: Validate**

Run: `cd terraform && terraform validate`
Expected: `Success! The configuration is valid.`

- [ ] **Step 3: Commit**

```bash
git add terraform/
git commit -m "infra: lambdas, scheduler, least-privilege IAM, DLQs, alarms"
```

---

### Task 15: Deploy and smoke test

**Files:** none new (operational task)

**Interfaces:**
- Consumes: everything.
- Produces: a running pipeline; verified schedule-sync output in DynamoDB.

**Precondition:** AWS credentials. Run `aws sts get-caller-identity` first — if it fails, STOP and report to the user that credentials are needed (suggest they run `! aws configure` or set up their profile); do not attempt workarounds.

- [ ] **Step 1: First deploy**

```bash
aws sts get-caller-identity
cd terraform && terraform init && cd ..
# ECR repo must exist before the image push, so create data-plane resources first:
cd terraform && terraform apply -target=aws_ecr_repository.main -var="image_tag=bootstrap" -auto-approve && cd ..
make deploy
```
Expected: `terraform apply` completes with all resources created. (`make deploy` runs tests, builds, pushes the git-SHA-tagged image, then applies with that tag.)

- [ ] **Step 2: Smoke test schedule-sync**

```bash
aws lambda invoke --function-name hockeytrack-schedule-sync --payload '{}' /tmp/claude-1000/-home-jay-projects-hockeytrack/0f605f9c-81e7-48f5-b69e-1002bb76f3d1/scratchpad/sync-out.json
cat /tmp/claude-1000/-home-jay-projects-hockeytrack/0f605f9c-81e7-48f5-b69e-1002bb76f3d1/scratchpad/sync-out.json
aws dynamodb scan --table-name hockeytrack-games --max-items 5
aws scheduler list-schedules --group-name hockeytrack-games --max-results 10
aws s3 ls s3://$(cd terraform && terraform output -raw raw_bucket)/raw/schedule/
```
Expected: invoke returns null (no error). In the off-season the schedule may legitimately contain zero games — verify instead that the raw schedule object landed in S3 and the invoke logged no errors (`aws logs tail /aws/lambda/hockeytrack-schedule-sync --since 10m`). If run during preseason/season: game rows in DynamoDB and `hockeytrack-game-*` schedules listed.

- [ ] **Step 3: Verify event plumbing end-to-end (manual poller run on a completed game)**

```bash
# Invoke the poller directly on the known completed game; it should archive
# the final sweep and publish events, reaching OutcomeFinal in one pass.
# First seed the game record via a temporary item:
aws dynamodb put-item --table-name hockeytrack-games --item '{
  "gameId": {"N": "2025020001"}, "season": {"N": "20252026"},
  "gameDate": {"S": "2025-10-07"}, "startTimeUTC": {"S": "2025-10-07T21:00:00Z"},
  "homeAbbrev": {"S": "FLA"}, "awayAbbrev": {"S": "CHI"},
  "venue": {"S": "Amerant Bank Arena"}, "gameState": {"S": "FUT"},
  "scheduleEntryName": {"S": "hockeytrack-game-2025020001"},
  "lastPlaySortOrder": {"N": "0"}, "chainCount": {"N": "0"}, "done": {"BOOL": false}
}'
aws lambda invoke --function-name hockeytrack-poller --payload '{"gameId":2025020001}' /tmp/claude-1000/-home-jay-projects-hockeytrack/0f605f9c-81e7-48f5-b69e-1002bb76f3d1/scratchpad/poll-out.json
cat /tmp/claude-1000/-home-jay-projects-hockeytrack/0f605f9c-81e7-48f5-b69e-1002bb76f3d1/scratchpad/poll-out.json
aws s3 ls s3://$(cd terraform && terraform output -raw raw_bucket)/raw/20252026/2025-10-07/2025020001/ --recursive
aws dynamodb get-item --table-name hockeytrack-games --key '{"gameId":{"N":"2025020001"}}' --projection-expression "done,gameState,lastPlaySortOrder"
```
Expected: poller returns null; S3 shows `pbp/`, `boxscore/`, and four `final/` objects; DynamoDB item shows `done: true`, `gameState: OFF`, nonzero `lastPlaySortOrder`. Note: this publishes ~350 play events to the bus — harmless, no rules exist yet.

- [ ] **Step 4: Commit any fixes and report**

```bash
git add -A && git commit -m "chore: deploy fixes from smoke test" # only if changes were needed
```
Report to the user: deployed resources, smoke-test results, monthly cost expectation (≈$1–2 off-season, ≈$10–15/month in season), and the reminder that goal-notification rules etc. are one EventBridge rule away.

---

## Post-plan notes for the executor

- Preseason starts late September 2026; deploying now means schedule-sync will find games once the NHL publishes the preseason schedule. The Task 15 Step 3 manual poller run is the only way to exercise the live path until then — the replay harness (Task 11) covers it offline.
- If the NHL API shape differs from a fixture assumption at any point, re-capture the fixture and adjust types — never hand-edit fixture JSON.
- Total AWS spend is dominated by poller Lambda-seconds (~$0.05/game). Nothing here needs provisioned capacity.
