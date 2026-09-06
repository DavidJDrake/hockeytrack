# Synthetic Live Games Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn any of the 72,921 archived NHL games into a game the pipeline believes is happening right now, so the poller, the event contract, the website and the forthcoming scoreboard can be exercised out of season.

**Architecture:** A new `internal/synth` package reconstructs the sequence of play-by-play documents a live game would have produced, from the single final document the archive keeps. It plugs into the existing `poller.Feed` interface, so the production poll loop runs unmodified over synthesized input. Two front ends consume it: `cmd/replay` (offline, fakes, deterministic) and `cmd/livefire` (real EventBridge bus, paced, under a synthetic game id and a distinct event source).

**Tech Stack:** Go 1.27.0, stdlib only for the synthesizer; AWS SDK v2 (already vendored) for the S3 source and the EventBridge publisher.

**Spec:** Jira epic HOC-45 (https://davidjdrake.atlassian.net/browse/HOC-45), tasks HOC-46 through HOC-50. Where this plan and the Jira text disagree, **this plan wins** — see "Corrections to the ticket text" below. The "Verified data facts" section is the spec of record for the reconstruction rules.

---

## Global Constraints

- Go 1.27.0, module `hockeytrack`. **No new third-party dependencies**: stdlib plus the AWS SDK v2 modules already in `go.mod`.
- `make test` (govulncheck + `go test ./...`) must pass at the end of every task.
- **Every test must pass with no AWS credentials in the environment.** Fixtures are checked in; nothing in `go test ./...` may call AWS.
- **Do not modify `internal/poller`.** The synthesizer plugs into the existing `poller.Feed` interface. If something cannot be expressed through that seam, stop and report it rather than widening the interface.
- The event contract in `internal/events` is frozen: do not change or remove any existing field or type. Adding a new exported constant or constructor is allowed.
- Never run `terraform apply`, `make deploy`, `make site`, `make push`, any AWS **mutating** API call, or `git push`. Read-only AWS calls (`s3api list-objects-v2`, `s3 cp` **from** S3) are permitted.
- Never commit, move or delete `terraform/terraform.tfvars`.
- Synthetic game ids are `realGameID + 9_000_000_000` (11 digits). A real NHL game id is always 10 digits, so the two can never collide. Never publish a synthetic event under a real game id except through the explicit `-as-poller` flag in Task 4.
- House style: stdlib `testing` only, no assertion libraries; `t.Errorf` with `%+v`. Doc comments on exported identifiers say *why*, not *what*. Match the surrounding code.

---

## Verified data facts (spec of record)

These were measured against 20 real archived games spanning 1917-18, 2009-10, 2010-11 and 2024-25, including overtime and shootout games. **Do not re-derive them; do not "improve" on them.**

**F1. Every play carries its own clock and period.** `timeInPeriod`, `timeRemaining` and `periodDescriptor` are present on 100% of plays in all tiers, including 1917-18. The clock therefore never needs to be computed from period lengths.

**F2. Running score comes from goal plays.** Only `goal` plays carry `details.awayScore` / `details.homeScore`, and they are the running score after that goal. True in every tier including 1917-18.

**F3. Shots on goal must be counted, not read.** Per-play `details.homeSOG`/`awaySOG` appear only on `shot-on-goal` plays and exclude goals, so they are not the running total. The rule that reproduces the final document's `sog` exactly on all 20 games is:

> `SOG(team) = number of plays with typeDescKey in {"shot-on-goal", "goal"} owned by that team whose periodDescriptor.periodType != "SO"`
>
> …except that a game containing **no** `shot-on-goal` play at all (the pre-2006 tier) reports `SOG = 0` for both teams.

Shootout attempts and the shootout-winning goal are excluded, which is why the `periodType != "SO"` clause is load-bearing.

**F4. `situationCode` is missing on some plays in older tiers** (8 of 310 plays in a 2010-11 game, 6 of 342 in 2009-10, all 19 in 1917-18). Carry the last non-empty value forward; leave it empty until the first play that has one.

**F5. Not every game ends with a `game-end` play.** The 1917-18 tier has no period or game markers at all, and at least one 2009-10 game's feed simply stops. **The last snapshot is therefore always forced to `gameState: "FINAL"`**, regardless of the last play's type. Without this the poller never terminates.

**F6. The shootout winner's goal is in no play's running score.** In game 2024021299 the last goal play reports 4-4 while the final document reports 4-5. **The last snapshot therefore takes its score and shots from the final document's own top-level values**, not from the fold.

**F7. During a shootout the clock is stopped.** `SO` plays carry `timeInPeriod` and `timeRemaining` of `00:00`. `running` must be false whenever `periodType == "SO"`.

**F8. JSON round-tripping must use `UseNumber()`.** Decoding into `map[string]any` with the default decoder turns game id 2025020001 into a float64 and re-marshals it as `2.025020001e+09`, corrupting every id. Every decode into a generic map in this package uses `json.Decoder` with `UseNumber()`.

### Corrections to the ticket text

HOC-46's description says to recompute the score by counting goals and to rebuild the clock from period lengths. Both are wrong against the data: F2 and F1 supersede them. HOC-49's description proposes a flag to enable alert delivery; in fact every alert rule in `terraform/notifications.tf` pins `source = ["hockeytrack.poller"]`, so publishing under `hockeytrack.synthetic` is **silent by construction** and needs no flag. The flag in Task 4 is the inverse: `-as-poller` opts *in* to the real source.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/nhl/types.go` (modify) | Add `TimeRemaining` and `SituationCode` to `Play`. |
| `internal/synth/synth.go` (create) | The reconstruction: `Snapshots`, the fold, the clock and state rules. Pure; no I/O. |
| `internal/synth/synth_test.go` (create) | Unit tests for the fold against checked-in finals. |
| `internal/synth/testdata/` (create) | Checked-in final feeds used by every test in the package. |
| `internal/synth/feed.go` (create) | `Feed`, a `poller.Feed` over a snapshot sequence, with a pacing hook. |
| `internal/synth/source.go` (create) | Resolve a game id to its archive key, fetch it, cache it on disk. |
| `internal/synth/e2e_test.go` (create) | Drives the real `poller.Run` over a synthesized game against fakes. |
| `internal/synth/golden_test.go` (create) | Golden event-stream regression over the curated game set. |
| `internal/synth/testdata/golden/` (create) | Recorded event streams, regenerated with `-update`. |
| `internal/events/eventbridge.go` (modify) | Add a constructor taking an explicit source. |
| `internal/events/events.go` (modify) | Add `SourceSynthetic`. |
| `cmd/replay/main.go` (modify) | Add `-game`/`-cache`/`-interval`; keep `-dir`. |
| `cmd/livefire/main.go` (create) | Paced publish of a synthetic game to the real bus. |
| `Makefile` (modify) | `replay`, `golden`, `golden-update`, `livefire` targets. |
| `README.md` (modify) | "Synthesizing a live game" section. |

---

### Task 1: The synthesizer (HOC-46)

**Files:**
- Modify: `internal/nhl/types.go` (the `Play` struct)
- Create: `internal/synth/synth.go`
- Test: `internal/synth/synth_test.go`
- Fixtures: `internal/synth/testdata/2025020001.json`, `internal/synth/testdata/1917020001.json`

**Interfaces:**
- Consumes: `nhl.PlayByPlay`, `nhl.Play` from `internal/nhl`.
- Produces, relied on by Tasks 2-4:
  - `type Options struct { Interval time.Duration; GameID int64 }`
  - `type Snapshot struct { PBP *nhl.PlayByPlay; Raw []byte }`
  - `func Snapshots(finalRaw []byte, opts Options) ([]Snapshot, error)`

- [ ] **Step 1: Copy the fixtures**

```bash
cd /home/jay/projects/hockeytrack
mkdir -p internal/synth/testdata
cp internal/poller/testdata/pbp.json internal/synth/testdata/2025020001.json
cp internal/nhl/testdata/pbp_1917020001.json internal/synth/testdata/1917020001.json
```

- [ ] **Step 2: Add the two missing play fields**

In `internal/nhl/types.go`, add to `type Play struct` immediately after `TimeInPeriod`:

```go
	TimeRemaining    string           `json:"timeRemaining"`
	SituationCode    string           `json:"situationCode"`
```

Adding fields is backward compatible: `Play.UnmarshalJSON` copies through an alias type and is unaffected.

- [ ] **Step 3: Run the existing tests to confirm nothing broke**

Run: `go test ./internal/...`
Expected: PASS.

- [ ] **Step 4: Write the failing tests**

Create `internal/synth/synth_test.go`. The fixture is CHI @ FLA, game 2025020001, 361 plays, final 2-3, shots 19-37.

```go
package synth

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSnapshotsPregameComesFirst(t *testing.T) {
	s, err := Snapshots(fixture(t, "2025020001"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(s) != 362 {
		t.Fatalf("snapshots = %d, want 362 (pregame + one per play)", len(s))
	}
	p := s[0].PBP
	if p.GameState != "PRE" || len(p.Plays) != 0 {
		t.Errorf("pregame = %s with %d plays", p.GameState, len(p.Plays))
	}
	if p.HomeTeam.Score != 0 || p.AwayTeam.Score != 0 || p.HomeTeam.SOG != 0 || p.AwayTeam.SOG != 0 {
		t.Errorf("pregame score/shots = %+v %+v", p.HomeTeam, p.AwayTeam)
	}
	if p.Clock.SecondsRemaining != 1200 || p.Clock.TimeRemaining != "20:00" || p.Clock.Running {
		t.Errorf("pregame clock = %+v", p.Clock)
	}
	if len(p.RosterSpots) != 40 {
		t.Errorf("pregame rosterSpots = %d, want 40", len(p.RosterSpots))
	}
	if p.ID != 2025020001 {
		t.Errorf("pregame id = %d", p.ID)
	}
}

func TestSnapshotsLastIsFinalAndComplete(t *testing.T) {
	s, err := Snapshots(fixture(t, "2025020001"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	p := s[len(s)-1].PBP
	if p.GameState != "FINAL" {
		t.Errorf("last state = %q, want FINAL", p.GameState)
	}
	if len(p.Plays) != 361 {
		t.Errorf("last plays = %d, want 361", len(p.Plays))
	}
	if p.AwayTeam.Score != 2 || p.HomeTeam.Score != 3 {
		t.Errorf("last score = %d-%d, want 2-3", p.AwayTeam.Score, p.HomeTeam.Score)
	}
	if p.AwayTeam.SOG != 19 || p.HomeTeam.SOG != 37 {
		t.Errorf("last shots = %d-%d, want 19-37", p.AwayTeam.SOG, p.HomeTeam.SOG)
	}
}

// Values hand-verified against the fixture: see the plan's "Verified data
// facts". Index i is the snapshot whose last play is plays[i-1].
func TestSnapshotsScoreAndShotsAtCutPoints(t *testing.T) {
	s, err := Snapshots(fixture(t, "2025020001"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		playIdx                int // index into the plays slice
		sortOrder              int64
		away, home, asog, hsog int
		period                 int
		remaining              string
	}{
		{66, 166, 1, 0, 3, 8, 1, "09:57"},
		{136, 340, 2, 2, 5, 17, 2, "18:51"},
		{293, 729, 2, 3, 16, 30, 3, "10:20"},
		{351, 852, 2, 3, 19, 37, 3, "01:53"},
	}
	for _, c := range cases {
		p := s[c.playIdx+1].PBP
		last := p.Plays[len(p.Plays)-1]
		if last.SortOrder != c.sortOrder {
			t.Fatalf("snapshot %d ends at sortOrder %d, want %d", c.playIdx+1, last.SortOrder, c.sortOrder)
		}
		if p.AwayTeam.Score != c.away || p.HomeTeam.Score != c.home {
			t.Errorf("sort %d score = %d-%d, want %d-%d", c.sortOrder, p.AwayTeam.Score, p.HomeTeam.Score, c.away, c.home)
		}
		if p.AwayTeam.SOG != c.asog || p.HomeTeam.SOG != c.hsog {
			t.Errorf("sort %d shots = %d-%d, want %d-%d", c.sortOrder, p.AwayTeam.SOG, p.HomeTeam.SOG, c.asog, c.hsog)
		}
		if p.PeriodDescriptor.Number != c.period || p.Clock.TimeRemaining != c.remaining {
			t.Errorf("sort %d clock = P%d %s, want P%d %s", c.sortOrder, p.PeriodDescriptor.Number, p.Clock.TimeRemaining, c.period, c.remaining)
		}
		if p.SituationCode != "1551" {
			t.Errorf("sort %d situationCode = %q", c.sortOrder, p.SituationCode)
		}
	}
}

// Last five minutes of the third with a one-goal margin is CRIT; earlier is
// LIVE. Snapshot 352 is P3 with 01:53 left at 2-3.
func TestSnapshotsCritInTheLastFiveMinutes(t *testing.T) {
	s, err := Snapshots(fixture(t, "2025020001"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := s[352].PBP.GameState; got != "CRIT" {
		t.Errorf("late one-goal state = %q, want CRIT", got)
	}
	if got := s[137].PBP.GameState; got != "LIVE" {
		t.Errorf("mid-game state = %q, want LIVE", got)
	}
}

// period-end at play index 129 (P1) and 232 (P2) is followed by another
// period-start, so it is an intermission; the P3 period-end at 359 is not.
func TestSnapshotsIntermissionOnlyBetweenPeriods(t *testing.T) {
	s, err := Snapshots(fixture(t, "2025020001"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{129, 232} {
		c := s[i+1].PBP.Clock
		if !c.InIntermission || c.Running || c.SecondsRemaining != 0 {
			t.Errorf("snapshot after play %d = %+v, want stopped intermission", i, c)
		}
	}
	if c := s[360].PBP.Clock; c.InIntermission {
		t.Errorf("final period-end = %+v, want no intermission", c)
	}
}

func TestSnapshotsClockNeverRewindsWithinAPeriod(t *testing.T) {
	s, err := Snapshots(fixture(t, "2025020001"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	prevPeriod, prev := 0, 0
	for i, sn := range s[1:] {
		p := sn.PBP
		if p.PeriodDescriptor.Number != prevPeriod {
			prevPeriod, prev = p.PeriodDescriptor.Number, p.Clock.SecondsRemaining
			continue
		}
		if p.Clock.SecondsRemaining > prev {
			t.Fatalf("snapshot %d clock went up: %d > %d", i+1, p.Clock.SecondsRemaining, prev)
		}
		prev = p.Clock.SecondsRemaining
	}
}

// A feed with no period markers and no shot plays still terminates, still
// reports zero shots, and still carries an empty situation code.
func TestSnapshotsPreModernTierStillEndsFinal(t *testing.T) {
	s, err := Snapshots(fixture(t, "1917020001"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	last := s[len(s)-1].PBP
	if last.GameState != "FINAL" {
		t.Errorf("state = %q, want FINAL", last.GameState)
	}
	if last.AwayTeam.SOG != 0 || last.HomeTeam.SOG != 0 {
		t.Errorf("shots = %d-%d, want 0-0 (feed records no shots)", last.AwayTeam.SOG, last.HomeTeam.SOG)
	}
	if last.SituationCode != "" {
		t.Errorf("situationCode = %q, want empty", last.SituationCode)
	}
	if last.AwayTeam.Score != 7 || last.HomeTeam.Score != 4 {
		t.Errorf("score = %d-%d, want 7-4", last.AwayTeam.Score, last.HomeTeam.Score)
	}
}

func TestSnapshotsIntervalModeIsCoarserButComplete(t *testing.T) {
	s, err := Snapshots(fixture(t, "2025020001"), Options{Interval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(s) >= 362 {
		t.Errorf("interval snapshots = %d, want fewer than the 362 per-play ones", len(s))
	}
	last := s[len(s)-1].PBP
	if last.GameState != "FINAL" || len(last.Plays) != 361 {
		t.Errorf("interval last = %s with %d plays", last.GameState, len(last.Plays))
	}
}

// The whole document travels, not just the fields the poller reads, and the
// numeric ids survive the round trip (see fact F8).
func TestSnapshotsPreserveUnknownFieldsAndIDs(t *testing.T) {
	s, err := Snapshots(fixture(t, "2025020001"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(s[len(s)-1].Raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"venue", "summary", "tvBroadcasts", "gameType", "startTimeUTC"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("field %q dropped from the snapshot", k)
		}
	}
	if string(s[0].Raw[:1]) != "{" {
		t.Errorf("raw is not an object")
	}
	if got := doc["id"]; got != json.Number("2025020001") && got != float64(2025020001) {
		t.Errorf("id round-tripped as %#v", got)
	}
}

// Play.Raw must be the original play bytes, because the play event carries
// them through to consumers verbatim.
func TestSnapshotsKeepOriginalPlayBytes(t *testing.T) {
	s, err := Snapshots(fixture(t, "2025020001"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	var orig struct {
		Plays []json.RawMessage `json:"plays"`
	}
	if err := json.Unmarshal(fixture(t, "2025020001"), &orig); err != nil {
		t.Fatal(err)
	}
	got := s[len(s)-1].PBP.Plays[0].Raw
	var a, b map[string]any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(orig.Plays[0], &b); err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Errorf("play 0 has %d fields, original had %d", len(a), len(b))
	}
}

func TestSnapshotsRewritesGameID(t *testing.T) {
	s, err := Snapshots(fixture(t, "2025020001"), Options{GameID: 11025020001})
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{0, len(s) - 1} {
		if s[i].PBP.ID != 11025020001 {
			t.Errorf("snapshot %d id = %d", i, s[i].PBP.ID)
		}
	}
}
```

- [ ] **Step 5: Run the tests to verify they fail**

Run: `go test ./internal/synth/`
Expected: FAIL — `undefined: Snapshots`.

- [ ] **Step 6: Write the synthesizer**

Create `internal/synth/synth.go`:

```go
// Package synth reconstructs the sequence of play-by-play documents a live
// game would have produced, from the single final document the archive
// keeps for every finished game. It is how the pipeline is exercised out of
// season: the reconstruction plugs into poller.Feed, so the production poll
// loop runs unmodified over a game that finished years ago.
//
// A snapshot is a reconstruction, not a recording. The cadence is chosen
// here rather than dictated by how often the poller happened to look, and
// the field order of the re-marshalled document differs from the original.
// Everything a consumer reads — plays, clock, score, shots, situation,
// roster — is faithful.
package synth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"hockeytrack/internal/nhl"
)

// pregameClock is what a feed shows before the opening faceoff.
const pregameSeconds = 1200

// critThreshold is the point in a final regulation period at which the NHL
// feed switches a one-goal game from LIVE to CRIT.
const critThreshold = 300

type Options struct {
	// Interval groups plays into one snapshot per Interval of elapsed game
	// time. Zero emits one snapshot per play: denser than any real poll
	// cadence, and the strictest test of the diff logic.
	Interval time.Duration
	// GameID replaces the document's own id. Live-fire runs set this to a
	// synthetic id so events can never be mistaken for a real game.
	GameID int64
}

// Snapshot is one synthesized poll result: the decoded document and the
// exact bytes a feed would have returned for it.
type Snapshot struct {
	PBP *nhl.PlayByPlay
	Raw []byte
}

// playState is the game state immediately after one play.
type playState struct {
	awayScore, homeScore int
	awayShots, homeShots int
	situation            string
}

// Snapshots reconstructs the poll sequence for one archived final feed. The
// first snapshot is always pre-game and the last always reports FINAL, so a
// poller driven by them starts and terminates exactly as it would live.
func Snapshots(finalRaw []byte, opts Options) ([]Snapshot, error) {
	// Fact F8: the default decoder would turn the game id into a float and
	// re-marshal it in exponent form.
	doc := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(finalRaw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode final: %w", err)
	}

	var final nhl.PlayByPlay
	if err := json.Unmarshal(finalRaw, &final); err != nil {
		return nil, fmt.Errorf("decode final as play-by-play: %w", err)
	}

	// Plays travel as raw bytes so the play events carry the original
	// object verbatim; period descriptors are lifted the same way.
	var envelope struct {
		Plays []json.RawMessage `json:"plays"`
	}
	if err := json.Unmarshal(finalRaw, &envelope); err != nil {
		return nil, fmt.Errorf("decode plays: %w", err)
	}
	if len(envelope.Plays) != len(final.Plays) {
		return nil, fmt.Errorf("play count mismatch: %d raw vs %d decoded", len(envelope.Plays), len(final.Plays))
	}
	periodDescs := make([]json.RawMessage, len(envelope.Plays))
	for i, raw := range envelope.Plays {
		var pd struct {
			PeriodDescriptor json.RawMessage `json:"periodDescriptor"`
		}
		if err := json.Unmarshal(raw, &pd); err != nil {
			return nil, fmt.Errorf("decode play %d period: %w", i, err)
		}
		periodDescs[i] = pd.PeriodDescriptor
	}

	states := fold(final.Plays, final.HomeTeam.ID, final.AwayTeam.ID)
	cuts := cutPoints(final.Plays, opts.Interval)

	homeTeam, _ := doc["homeTeam"].(map[string]any)
	awayTeam, _ := doc["awayTeam"].(map[string]any)
	if homeTeam == nil || awayTeam == nil {
		return nil, fmt.Errorf("document has no homeTeam/awayTeam object")
	}
	if opts.GameID != 0 {
		doc["id"] = json.Number(fmt.Sprint(opts.GameID))
	}

	out := make([]Snapshot, 0, len(cuts)+1)

	emit := func() error {
		raw, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		var pbp nhl.PlayByPlay
		if err := json.Unmarshal(raw, &pbp); err != nil {
			return err
		}
		out = append(out, Snapshot{PBP: &pbp, Raw: raw})
		return nil
	}

	// Pre-game: rosters are published, nothing else has happened yet.
	doc["plays"] = []json.RawMessage{}
	doc["gameState"] = "PRE"
	doc["periodDescriptor"] = map[string]any{"number": 1, "periodType": "REG"}
	doc["clock"] = clockDoc("20:00", pregameSeconds, false, false)
	delete(doc, "situationCode")
	homeTeam["score"], homeTeam["sog"] = 0, 0
	awayTeam["score"], awayTeam["sog"] = 0, 0
	if err := emit(); err != nil {
		return nil, err
	}

	for n, i := range cuts {
		p := final.Plays[i]
		st := states[i]
		last := n == len(cuts)-1

		doc["plays"] = envelope.Plays[:i+1]
		doc["periodDescriptor"] = periodDescs[i]
		doc["gameState"] = gameState(p, st, last)
		doc["clock"] = clockDocFor(final.Plays, i)
		if st.situation != "" {
			doc["situationCode"] = st.situation
		} else {
			delete(doc, "situationCode")
		}
		// Fact F6: the shootout winner's goal is in no play's running
		// score, so the last snapshot defers to the document itself.
		if last {
			awayTeam["score"], homeTeam["score"] = final.AwayTeam.Score, final.HomeTeam.Score
			awayTeam["sog"], homeTeam["sog"] = final.AwayTeam.SOG, final.HomeTeam.SOG
		} else {
			awayTeam["score"], homeTeam["score"] = st.awayScore, st.homeScore
			awayTeam["sog"], homeTeam["sog"] = st.awayShots, st.homeShots
		}
		if err := emit(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// fold walks the plays once, recording the state after each, so a cut at
// any index is a lookup rather than a rescan.
func fold(plays []nhl.Play, homeID, awayID int64) []playState {
	hasShots := false
	for _, p := range plays {
		if p.TypeDescKey == "shot-on-goal" {
			hasShots = true
			break
		}
	}
	out := make([]playState, len(plays))
	var cur playState
	for i, p := range plays {
		d := p.ParsedDetails()
		if d.AwayScore != nil && d.HomeScore != nil {
			cur.awayScore, cur.homeScore = *d.AwayScore, *d.HomeScore
		}
		// Fact F3: shots are counted, and shootout attempts do not count.
		if hasShots && p.PeriodDescriptor.PeriodType != "SO" &&
			(p.TypeDescKey == "shot-on-goal" || p.TypeDescKey == "goal") {
			switch d.EventOwnerTeamID {
			case homeID:
				cur.homeShots++
			case awayID:
				cur.awayShots++
			}
		}
		// Fact F4: older tiers omit it on some plays; carry it forward.
		if p.SituationCode != "" {
			cur.situation = p.SituationCode
		}
		out[i] = cur
	}
	return out
}

// cutPoints chooses which plays end a snapshot. Zero interval means every
// play. Otherwise a snapshot ends at each period boundary and whenever the
// game clock has advanced by interval, and always at the last play.
func cutPoints(plays []nhl.Play, interval time.Duration) []int {
	if len(plays) == 0 {
		return nil
	}
	if interval <= 0 {
		out := make([]int, len(plays))
		for i := range plays {
			out[i] = i
		}
		return out
	}
	step := int(interval.Seconds())
	var out []int
	lastCut := -1 << 30
	lastPeriod := plays[0].PeriodDescriptor.Number
	for i, p := range plays {
		elapsed := parseClock(p.TimeInPeriod)
		periodChanged := p.PeriodDescriptor.Number != lastPeriod
		switch {
		case i == len(plays)-1, periodChanged, elapsed-lastCut >= step:
			out = append(out, i)
			lastCut = elapsed
			if periodChanged {
				lastPeriod = p.PeriodDescriptor.Number
			}
		}
	}
	return out
}

// gameState maps a play onto the state the feed would report. Fact F5: the
// last snapshot is FINAL whatever the last play is, because older feeds
// carry no game-end marker and the poller would otherwise never stop.
func gameState(p nhl.Play, st playState, last bool) string {
	if last || p.TypeDescKey == "game-end" {
		return "FINAL"
	}
	margin := st.homeScore - st.awayScore
	if margin < 0 {
		margin = -margin
	}
	remaining := parseClock(p.TimeRemaining)
	if p.PeriodDescriptor.PeriodType == "REG" && p.PeriodDescriptor.Number >= 3 &&
		remaining <= critThreshold && margin <= 1 {
		return "CRIT"
	}
	return "LIVE"
}

// clockDocFor renders the clock as of plays[i]. The clock stops at a period
// or game end and throughout a shootout (fact F7); an intermission is a
// period-end with another period still to come.
func clockDocFor(plays []nhl.Play, i int) map[string]any {
	p := plays[i]
	remaining := parseClock(p.TimeRemaining)
	stopped := p.TypeDescKey == "period-end" || p.TypeDescKey == "game-end" ||
		p.PeriodDescriptor.PeriodType == "SO"
	intermission := false
	if p.TypeDescKey == "period-end" {
		for _, later := range plays[i+1:] {
			if later.TypeDescKey == "period-start" {
				intermission = true
				break
			}
		}
	}
	return clockDoc(p.TimeRemaining, remaining, !stopped, intermission)
}

func clockDoc(remaining string, seconds int, running, intermission bool) map[string]any {
	return map[string]any{
		"timeRemaining":    remaining,
		"secondsRemaining": seconds,
		"running":          running,
		"inIntermission":   intermission,
	}
}

// parseClock reads the feed's MM:SS clock strings. Anything malformed
// counts as zero: a bad clock must not stop a replay.
func parseClock(s string) int {
	var m, sec int
	if _, err := fmt.Sscanf(s, "%d:%d", &m, &sec); err != nil {
		return 0
	}
	return m*60 + sec
}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./internal/synth/ -v`
Expected: PASS, all eleven tests.

- [ ] **Step 8: Run the full suite**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/nhl/types.go internal/synth/
git commit -m "synth: reconstruct a live game's snapshots from an archived final"
```

---

### Task 2: Feed, archive source, and the replay command (HOC-47)

**Files:**
- Create: `internal/synth/feed.go`, `internal/synth/source.go`
- Modify: `cmd/replay/main.go`
- Test: `internal/synth/feed_test.go`, `internal/synth/source_test.go`, `internal/synth/e2e_test.go`

**Interfaces:**
- Consumes: `Snapshots`, `Snapshot`, `Options` from Task 1; `poller.Feed`, `poller.Run`, `poller.Deps`, `poller.DefaultConfig`, `store.FakeGameStore`, `store.FakeArchive`, `store.GameRecord`, `events.FakePublisher`.
- Produces, relied on by Task 4:
  - `func NewFeed(snaps []Snapshot) *Feed`
  - `type Feed struct { Before func(ctx context.Context, s Snapshot, i int) error; ... }`
  - `func (f *Feed) PlayByPlay(ctx context.Context, gameID int64) (*nhl.PlayByPlay, []byte, error)`
  - `func (f *Feed) RawFeed(ctx context.Context, gameID int64, feed string) ([]byte, error)`
  - `func (f *Feed) ShiftCharts(ctx context.Context, gameID int64) ([]byte, error)`
  - `type Source interface { List(ctx, prefix string) ([]string, error); Get(ctx, key string) ([]byte, error) }`
  - `func SeasonOf(gameID int64) int64`
  - `func FinalPBP(ctx context.Context, src Source, gameID int64, cacheDir string) ([]byte, error)`

- [ ] **Step 1: Write the failing feed and source tests**

Create `internal/synth/feed_test.go`:

```go
package synth

import (
	"context"
	"testing"
)

func TestFeedServesSnapshotsThenRepeatsTheLast(t *testing.T) {
	s, err := Snapshots(fixture(t, "1917020001"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	f := NewFeed(s)
	ctx := context.Background()
	seen := 0
	for i := 0; i < len(s)+3; i++ {
		p, raw, err := f.PlayByPlay(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) == 0 || p == nil {
			t.Fatalf("call %d returned nothing", i)
		}
		seen++
	}
	if seen != len(s)+3 {
		t.Errorf("served %d, want %d", seen, len(s)+3)
	}
	// Past the end it must keep reporting the finished game, or the poller
	// would never observe a final state.
	p, _, _ := f.PlayByPlay(ctx, 0)
	if p.GameState != "FINAL" {
		t.Errorf("exhausted feed state = %q, want FINAL", p.GameState)
	}
}

func TestFeedCallsBeforeHookOncePerSnapshot(t *testing.T) {
	s, err := Snapshots(fixture(t, "1917020001"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	f := NewFeed(s)
	calls := 0
	f.Before = func(context.Context, Snapshot, int) error { calls++; return nil }
	for i := 0; i < 5; i++ {
		if _, _, err := f.PlayByPlay(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 5 {
		t.Errorf("Before called %d times, want 5", calls)
	}
}

func TestFeedStubsTheOtherEndpoints(t *testing.T) {
	f := NewFeed([]Snapshot{{Raw: []byte(`{}`)}})
	b, err := f.RawFeed(context.Background(), 1, "boxscore")
	if err != nil || len(b) == 0 {
		t.Errorf("RawFeed = %q, %v", b, err)
	}
	if b, err := f.ShiftCharts(context.Background(), 1); err != nil || len(b) == 0 {
		t.Errorf("ShiftCharts = %q, %v", b, err)
	}
}
```

Create `internal/synth/source_test.go`:

```go
package synth

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"hockeytrack/internal/store"
)

func TestSeasonOf(t *testing.T) {
	cases := map[int64]int64{
		2025020001: 20252026,
		1917020001: 19171918,
		2024021299: 20242025,
	}
	for id, want := range cases {
		if got := SeasonOf(id); got != want {
			t.Errorf("SeasonOf(%d) = %d, want %d", id, got, want)
		}
	}
}

func TestFinalPBPResolvesThroughTheArchive(t *testing.T) {
	a := store.NewFakeArchive()
	ctx := context.Background()
	key := store.FinalKey(20252026, "2025-10-07", 2025020001, "pbp")
	body := fixture(t, "2025020001")
	if err := a.Put(ctx, key, body); err != nil {
		t.Fatal(err)
	}
	// Decoys under the same season prefix must not be picked up.
	_ = a.Put(ctx, store.FinalKey(20252026, "2025-10-07", 2025020002, "pbp"), []byte(`{"id":2}`))
	_ = a.Put(ctx, store.SnapshotKey(20252026, "2025-10-07", 2025020001, "pbp", nowForTest()), []byte(`{"id":3}`))

	got, err := FinalPBP(ctx, a, 2025020001, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(body) {
		t.Errorf("got %d bytes, want %d", len(got), len(body))
	}
}

func TestFinalPBPMissingGameIsAClearError(t *testing.T) {
	_, err := FinalPBP(context.Background(), store.NewFakeArchive(), 2025020001, "")
	if err == nil {
		t.Fatal("want an error for an absent game")
	}
}

func TestFinalPBPCachesToDisk(t *testing.T) {
	a := store.NewFakeArchive()
	ctx := context.Background()
	body := fixture(t, "1917020001")
	if err := a.Put(ctx, store.FinalKey(19171918, "1917-12-19", 1917020001, "pbp"), body); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := FinalPBP(ctx, a, 1917020001, dir); err != nil {
		t.Fatal(err)
	}
	cached := filepath.Join(dir, "1917020001.json")
	if _, err := os.Stat(cached); err != nil {
		t.Fatalf("cache file not written: %v", err)
	}
	// A second call must be served from disk: an empty archive proves it.
	got, err := FinalPBP(ctx, store.NewFakeArchive(), 1917020001, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(body) {
		t.Errorf("cached read = %d bytes, want %d", len(got), len(body))
	}
}
```

Add to `internal/synth/source_test.go` a tiny helper:

```go
func nowForTest() time.Time { return time.Date(2025, 10, 7, 23, 0, 0, 0, time.UTC) }
```

(with `"time"` imported).

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/synth/`
Expected: FAIL — `undefined: NewFeed`, `undefined: SeasonOf`.

- [ ] **Step 3: Write the feed**

Create `internal/synth/feed.go`:

```go
package synth

import (
	"context"
	"fmt"

	"hockeytrack/internal/nhl"
)

// Feed serves synthesized snapshots in order through the poller's Feed
// interface. Once the sequence is exhausted it repeats the last snapshot,
// because the poller polls until it observes a final state and the last
// snapshot is always FINAL.
type Feed struct {
	snaps []Snapshot
	i     int

	// Before runs immediately before each snapshot is served, with the
	// index of the snapshot about to be returned. Live-fire uses it to
	// pace the run against the wall clock; offline replay leaves it nil.
	Before func(ctx context.Context, s Snapshot, i int) error
}

func NewFeed(snaps []Snapshot) *Feed { return &Feed{snaps: snaps} }

// Len reports how many distinct snapshots the feed holds.
func (f *Feed) Len() int { return len(f.snaps) }

func (f *Feed) PlayByPlay(ctx context.Context, _ int64) (*nhl.PlayByPlay, []byte, error) {
	if len(f.snaps) == 0 {
		return nil, nil, fmt.Errorf("synth: feed has no snapshots")
	}
	i := f.i
	if i >= len(f.snaps) {
		i = len(f.snaps) - 1
	}
	s := f.snaps[i]
	if f.Before != nil {
		if err := f.Before(ctx, s, i); err != nil {
			return nil, nil, err
		}
	}
	f.i++
	return s.PBP, s.Raw, nil
}

// RawFeed and ShiftCharts are stubs: the archive holds these feeds too, but
// nothing in the event contract is derived from them, and serving a
// placeholder keeps a replay free of network calls.
func (f *Feed) RawFeed(_ context.Context, _ int64, feed string) ([]byte, error) {
	return []byte(fmt.Sprintf(`{"synthStub":%q}`, feed)), nil
}

func (f *Feed) ShiftCharts(_ context.Context, _ int64) ([]byte, error) {
	return []byte(`{"synthStub":"shifts"}`), nil
}
```

Note: `PlayByPlay` returns the snapshot's own `*nhl.PlayByPlay` rather than a copy. The poller only reads it, and sharing keeps a 362-snapshot replay from allocating a second copy of every document.

- [ ] **Step 4: Write the archive source**

Create `internal/synth/source.go`:

```go
package synth

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Source is the read side of the raw archive. store.S3Archive and
// store.FakeArchive both satisfy it.
type Source interface {
	List(ctx context.Context, prefix string) ([]string, error)
	Get(ctx context.Context, key string) ([]byte, error)
}

// SeasonOf derives the season id from a game id: the first four digits are
// the season's opening year, and a season id is that year followed by the
// next. 2024021299 belongs to season 20242025.
func SeasonOf(gameID int64) int64 {
	year := gameID / 1000000
	return year*10000 + year + 1
}

// FinalPBP fetches a game's archived final play-by-play. The archive key
// carries the game date, which the game id does not, so the game is located
// by listing its season prefix. When cacheDir is non-empty the body is read
// from and written to <cacheDir>/<gameID>.json, which makes repeat runs and
// offline work possible.
func FinalPBP(ctx context.Context, src Source, gameID int64, cacheDir string) ([]byte, error) {
	if cacheDir != "" {
		if b, err := os.ReadFile(cachePath(cacheDir, gameID)); err == nil {
			return b, nil
		}
	}
	season := SeasonOf(gameID)
	keys, err := src.List(ctx, fmt.Sprintf("raw/%d/", season))
	if err != nil {
		return nil, fmt.Errorf("list season %d: %w", season, err)
	}
	want := fmt.Sprintf("/%d/final/pbp.json", gameID)
	key := ""
	for _, k := range keys {
		if strings.HasSuffix(k, want) {
			key = k
			break
		}
	}
	if key == "" {
		return nil, fmt.Errorf("game %d has no final/pbp.json under raw/%d/ (%d keys listed)", gameID, season, len(keys))
	}
	body, err := src.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	if cacheDir != "" {
		if err := os.MkdirAll(cacheDir, 0o755); err == nil {
			// A cache write failure is not worth failing the run over.
			_ = os.WriteFile(cachePath(cacheDir, gameID), body, 0o644)
		}
	}
	return body, nil
}

func cachePath(dir string, gameID int64) string {
	return filepath.Join(dir, fmt.Sprintf("%d.json", gameID))
}

// DefaultCacheDir is where replays keep downloaded finals.
func DefaultCacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "hockeytrack", "finals")
}
```

- [ ] **Step 5: Run the feed and source tests**

Run: `go test ./internal/synth/ -run 'Feed|Season|FinalPBP' -v`
Expected: PASS.

- [ ] **Step 6: Write the end-to-end poller test**

Create `internal/synth/e2e_test.go`. It lives in the external test package so it can import `poller` without any risk of a cycle.

```go
package synth_test

import (
	"context"
	"os"
	"testing"
	"time"

	"hockeytrack/internal/events"
	"hockeytrack/internal/nhl"
	"hockeytrack/internal/poller"
	"hockeytrack/internal/store"
	"hockeytrack/internal/synth"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// runGame drives the real poller over a synthesized game and returns
// everything it published.
func runGame(t *testing.T, name string, opts synth.Options) []events.PublishedEvent {
	t.Helper()
	raw := readFixture(t, name)
	snaps, err := synth.Snapshots(raw, opts)
	if err != nil {
		t.Fatal(err)
	}
	var first nhl.PlayByPlay
	if err := jsonUnmarshal(raw, &first); err != nil {
		t.Fatal(err)
	}
	gs := store.NewFakeGameStore()
	if err := gs.UpsertSchedule(context.Background(), store.GameRecord{
		GameID: snaps[0].PBP.ID, Season: first.Season, GameDate: first.GameDate,
		HomeAbbrev: first.HomeTeam.Abbrev, AwayAbbrev: first.AwayTeam.Abbrev,
		GameState: "FUT", StartTimeUTC: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	pub := &events.FakePublisher{}
	outcome, err := poller.Run(context.Background(), poller.Deps{
		Feed: synth.NewFeed(snaps), Store: gs, Archive: store.NewFakeArchive(), Pub: pub,
		Now:   time.Now,
		Sleep: func(context.Context, time.Duration) error { return nil },
	}, poller.DefaultConfig(), snaps[0].PBP.ID, "test", func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if outcome != poller.OutcomeFinal {
		t.Fatalf("outcome = %v, want OutcomeFinal", outcome)
	}
	return pub.Published
}

func countByType(evs []events.PublishedEvent) map[string]int {
	m := map[string]int{}
	for _, e := range evs {
		m[e.DetailType]++
	}
	return m
}

func TestPollerReachesFinalOverASynthesizedGame(t *testing.T) {
	got := countByType(runGame(t, "2025020001", synth.Options{Interval: 30 * time.Second}))
	for _, dt := range []string{events.DTStatus, events.DTRoster, events.DTClock, events.DTPlay, events.DTFinal} {
		if got[dt] == 0 {
			t.Errorf("no %s events published; got %v", dt, got)
		}
	}
	if got[events.DTPlay] != 361 {
		t.Errorf("play events = %d, want 361 (one per play, no duplicates)", got[events.DTPlay])
	}
	if got[events.DTFinal] != 1 {
		t.Errorf("final events = %d, want 1", got[events.DTFinal])
	}
	// The roster never changes in a real game, so it is published once.
	if got[events.DTRoster] != 1 {
		t.Errorf("roster events = %d, want 1", got[events.DTRoster])
	}
}

func TestPollerHandlesTheOldestTier(t *testing.T) {
	got := countByType(runGame(t, "1917020001", synth.Options{}))
	if got[events.DTPlay] != 19 {
		t.Errorf("play events = %d, want 19", got[events.DTPlay])
	}
	if got[events.DTFinal] != 1 {
		t.Errorf("final events = %d, want 1", got[events.DTFinal])
	}
}
```

Add at the bottom of the same file:

```go
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
```

with `"encoding/json"` imported.

- [ ] **Step 7: Run it**

Run: `go test ./internal/synth/ -run TestPoller -v`
Expected: PASS. If the play count is not exactly 361, the diff logic is seeing duplicate or missing plays — fix the synthesizer, not the poller.

- [ ] **Step 8: Rewrite the replay command**

Replace `cmd/replay/main.go` with:

```go
// Replay drives the poller over one game and prints every event it
// publishes, against in-memory fakes. The game comes either from the S3
// archive (-game, any of the finished games the archive holds) or from a
// directory of recorded live snapshots (-dir).
//
// Usage:
//
//	replay -game 2024021299
//	replay -game 2024021299 -interval 30s
//	replay -dir path/to/snapshots/
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

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"hockeytrack/internal/nhl"
	"hockeytrack/internal/poller"
	"hockeytrack/internal/store"
	"hockeytrack/internal/synth"
)

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
	game := flag.Int64("game", 0, "archived game id to synthesize")
	dir := flag.String("dir", "", "directory of recorded pbp snapshot JSON files")
	bucket := flag.String("bucket", os.Getenv("HOCKEYTRACK_RAW_BUCKET"), "raw archive bucket (default $HOCKEYTRACK_RAW_BUCKET)")
	cache := flag.String("cache", synth.DefaultCacheDir(), "directory for downloaded finals; empty disables caching")
	interval := flag.Duration("interval", 0, "group plays into one snapshot per interval of game time; 0 means one per play")
	flag.Parse()

	if (*game == 0) == (*dir == "") {
		fmt.Fprintln(os.Stderr, "usage: replay -game <id> | -dir <snapshot dir>")
		os.Exit(2)
	}

	ctx := context.Background()
	var feed poller.Feed
	var first nhl.PlayByPlay

	if *game != 0 {
		raw, err := loadFinal(ctx, *bucket, *cache, *game)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		snaps, err := synth.Snapshots(raw, synth.Options{Interval: *interval})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := json.Unmarshal(raw, &first); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "replaying %d (%s @ %s, %s) as %d snapshots\n",
			first.ID, first.AwayTeam.Abbrev, first.HomeTeam.Abbrev, first.GameDate, len(snaps))
		feed = synth.NewFeed(snaps)
	} else {
		f, p, err := loadDir(*dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		feed, first = f, p
	}

	gs := store.NewFakeGameStore()
	if err := gs.UpsertSchedule(ctx, store.GameRecord{
		GameID: first.ID, Season: first.Season, GameDate: first.GameDate,
		HomeAbbrev: first.HomeTeam.Abbrev, AwayAbbrev: first.AwayTeam.Abbrev,
		GameState: "FUT", StartTimeUTC: time.Now(),
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	pub := &printingPub{}
	outcome, err := poller.Run(ctx, poller.Deps{
		Feed: feed, Store: gs, Archive: store.NewFakeArchive(), Pub: pub,
		Now:   time.Now,
		Sleep: func(context.Context, time.Duration) error { return nil },
	}, poller.DefaultConfig(), first.ID, "replay", func() bool { return false })
	if err != nil {
		fmt.Fprintln(os.Stderr, "replay error:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "replay done: outcome=%d events=%d\n", outcome, pub.n)
	if outcome != poller.OutcomeFinal {
		os.Exit(1)
	}
}

// loadFinal reads the game's final feed, preferring the on-disk cache so a
// repeat replay needs no AWS credentials at all.
func loadFinal(ctx context.Context, bucket, cache string, game int64) ([]byte, error) {
	if cache != "" {
		if b, err := os.ReadFile(filepath.Join(cache, fmt.Sprintf("%d.json", game))); err == nil {
			return b, nil
		}
	}
	if bucket == "" {
		return nil, fmt.Errorf("game %d is not cached and -bucket is empty; pass -bucket or set HOCKEYTRACK_RAW_BUCKET", game)
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	return synth.FinalPBP(ctx, store.NewS3Archive(s3.NewFromConfig(cfg), bucket), game, cache)
}

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

func loadDir(dir string) (*dirFeed, nhl.PlayByPlay, error) {
	var first nhl.PlayByPlay
	entries, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(entries) == 0 {
		return nil, first, fmt.Errorf("no snapshots in %s", dir)
	}
	sort.Strings(entries)
	f := &dirFeed{}
	for _, e := range entries {
		b, err := os.ReadFile(e)
		if err != nil {
			return nil, first, err
		}
		f.bodies = append(f.bodies, b)
	}
	if err := json.Unmarshal(f.bodies[0], &first); err != nil {
		return nil, first, err
	}
	return f, first, nil
}
```

`cmd/replay` does not import `internal/events`: it prints whatever detail type the poller hands it, so it needs no knowledge of the contract.

- [ ] **Step 9: Build and smoke-test the command offline**

```bash
go build ./cmd/replay
mkdir -p "$HOME/.cache/hockeytrack/finals"
cp internal/synth/testdata/2025020001.json "$HOME/.cache/hockeytrack/finals/"
./replay -game 2025020001 -interval 60s | head -3
./replay -game 2025020001 -interval 60s | wc -l
rm -f replay
```

Expected: JSON event lines on stdout, a `replay done: outcome=0` summary on stderr, and exit status 0.

- [ ] **Step 10: Run the full suite**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 11: Commit**

```bash
git add internal/synth/ cmd/replay/main.go
git commit -m "synth: serve synthesized games to the poller and replay them by game id"
```

---

### Task 3: Golden event-stream regression suite (HOC-48)

**Files:**
- Create: `internal/synth/golden_test.go`
- Create: `internal/synth/testdata/2024021294.json`, `2024021297.json`, `2024021298.json`, `2024021299.json`, `2024021300.json`
- Create: `internal/synth/testdata/golden/*.json` (generated)

**Interfaces:**
- Consumes: `synth.Snapshots`, `synth.NewFeed`, `poller.Run`, `events.FakePublisher` — all as used in `e2e_test.go`.
- Produces: nothing other tasks consume.

**The curated set.** These six games were selected from the archive and their characteristics verified; the notes are why each one is in the set.

| Game | Matchup | Final | Why it is here |
|---|---|---|---|
| `2025020001` | CHI @ FLA | 2-3 REG | Plain regulation game; already the repo's baseline fixture. |
| `2024021298` | ANA @ MIN | 2-3 OT | Overtime: the `OT` period type and a game-winning goal outside regulation. |
| `2024021299` | VGK @ CGY | 4-5 SO | Shootout: `SO` period type, a stopped clock, and the winning goal that appears in no play's running score. |
| `2024021294` | FLA @ TBL | 1-5 REG | 14 penalties including a match penalty and two majors. |
| `2024021297` | UTA @ STL | 1-6 REG | Two misconducts, the one penalty class the others miss. |
| `2024021300` | LAK @ SEA | 6-5 REG | A penalty shot, an empty-net goal, and eleven goals total. |
| `1917020001` | MTL @ SEN | 7-4 | Pre-modern tier: goals and penalties only, no period markers, no shots. |

- [ ] **Step 1: Download the five new fixtures**

These are read-only S3 gets. The bucket name comes from Terraform output; do not run any other Terraform command.

```bash
cd /home/jay/projects/hockeytrack
export AWS_REGION=us-east-1
B=hockeytrack-raw-989232581535
# All five were played on 2025-04-15.
for id in 2024021294 2024021297 2024021298 2024021299 2024021300; do
  aws s3 cp "s3://$B/raw/20242025/2025-04-15/$id/final/pbp.json" "internal/synth/testdata/$id.json" --quiet
done
ls -la internal/synth/testdata/
```

Expected: five files, each roughly 120-160 KB. If a copy fails, the date is wrong — find the key with:
`aws s3api list-objects-v2 --bucket $B --prefix raw/20242025/ --query "Contents[?contains(Key, '<id>/final/pbp.json')].Key" --output text`

- [ ] **Step 2: Write the golden test**

Create `internal/synth/golden_test.go`:

```go
package synth_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"hockeytrack/internal/events"
	"hockeytrack/internal/synth"
)

var update = flag.Bool("update", false, "rewrite the golden event streams")

// curated is the set of archived games whose event streams are locked down.
// Each entry names why the game is in the set; removing one removes that
// coverage.
var curated = []struct {
	game string
	why  string
}{
	{"2025020001", "plain regulation game"},
	{"2024021298", "overtime winner"},
	{"2024021299", "shootout, stopped clock, winner outside the play stream"},
	{"2024021294", "match penalty and two majors"},
	{"2024021297", "misconducts"},
	{"2024021300", "penalty shot, empty-net goal, eleven goals"},
	{"1917020001", "pre-modern tier: no period markers, no shots"},
}

// normalised is one published event with the volatile fields removed, so a
// golden file is stable across runs.
type normalised struct {
	DetailType string          `json:"detailType"`
	Detail     json.RawMessage `json:"detail"`
}

// normalise drops the two fields that legitimately differ run to run: the
// clock event's observation timestamp, and the play event's raw payload
// (which is the archived play verbatim and is already covered by the
// synthesizer's own tests).
func normalise(evs []events.PublishedEvent) []normalised {
	out := make([]normalised, 0, len(evs))
	for _, e := range evs {
		b, err := json.Marshal(e.Detail)
		if err != nil {
			panic(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			panic(err)
		}
		delete(m, "observedAt")
		delete(m, "raw")
		nb, err := json.Marshal(m)
		if err != nil {
			panic(err)
		}
		out = append(out, normalised{DetailType: e.DetailType, Detail: nb})
	}
	return out
}

func TestGoldenEventStreams(t *testing.T) {
	for _, c := range curated {
		t.Run(c.game, func(t *testing.T) {
			evs := runGame(t, c.game, synth.Options{Interval: 30 * time.Second})
			got, err := json.MarshalIndent(normalise(evs), "", " ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			path := filepath.Join("testdata", "golden", c.game+".json")
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				t.Logf("wrote %s (%d events, %s)", path, len(evs), c.why)
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v\nrun: go test ./internal/synth/ -run TestGolden -update", err)
			}
			if string(got) != string(want) {
				t.Errorf("event stream for %s changed (%s).\nRegenerate with -update once you are sure the change is intended.\n%s",
					c.game, c.why, firstDiff(string(want), string(got)))
			}
		})
	}
}

// firstDiff reports the first differing line, which is far more useful than
// dumping two multi-megabyte streams.
func firstDiff(want, got string) string {
	w, g := splitLines(want), splitLines(got)
	for i := 0; i < len(w) && i < len(g); i++ {
		if w[i] != g[i] {
			return "line " + itoa(i+1) + ":\n  want: " + w[i] + "\n  got:  " + g[i]
		}
	}
	return "length differs: want " + itoa(len(w)) + " lines, got " + itoa(len(g))
}
```

And the two helpers it uses, at the bottom of the same file (import `strings` and `strconv`). Do not pull in a diff library — the constraint on dependencies is absolute.

```go
func splitLines(s string) []string { return strings.Split(s, "\n") }

func itoa(n int) string { return strconv.Itoa(n) }
```

- [ ] **Step 3: Generate the golden files**

Run: `go test ./internal/synth/ -run TestGolden -update -v`
Expected: PASS, with seven `wrote testdata/golden/<game>.json` log lines.

- [ ] **Step 4: Sanity-check what was recorded**

```bash
for f in internal/synth/testdata/golden/*.json; do
  echo "$f $(python3 -c "import json,sys;d=json.load(open('$f'));print(len(d),'events')")"
done
du -sh internal/synth/testdata/
```

Expected: every file has hundreds of events; the shootout game has the most. If any file has zero events the run did not reach the game.

- [ ] **Step 5: Verify the suite now guards the contract**

Run: `go test ./internal/synth/ -run TestGolden`
Expected: PASS.

Then prove it fails when the contract moves: temporarily change `events.DTClock` to `"nhl.game.clock2"` in `internal/events/events.go`, re-run, confirm FAIL, and revert.

- [ ] **Step 6: Run the full suite and check the repo size**

Run: `go test ./... && du -sh internal/synth/testdata/`
Expected: PASS, testdata under about 4 MB.

- [ ] **Step 7: Commit**

```bash
git add internal/synth/
git commit -m "synth: golden event streams for a curated set of archived games"
```

---

### Task 4: Live-fire against the real bus (HOC-49)

**Files:**
- Modify: `internal/events/events.go`, `internal/events/eventbridge.go`
- Create: `cmd/livefire/main.go`
- Test: `internal/events/eventbridge_test.go` (extend if it exists), `internal/synth/pace_test.go`
- Create: `internal/synth/pace.go`

**Interfaces:**
- Consumes: `synth.Feed.Before`, `synth.Snapshots`, `synth.FinalPBP` from Tasks 1-2.
- Produces:
  - `const events.SourceSynthetic = "hockeytrack.synthetic"`
  - `func events.NewEventBridgePublisherWithSource(client *eventbridge.Client, busName, source string) *EventBridgePublisher`
  - `func synth.Pacer(speed float64, maxWait time.Duration, sleep func(context.Context, time.Duration) error) func(context.Context, Snapshot, int) error`

**Safety design, and why it is safe by construction.** Every alert rule in `terraform/notifications.tf` pins `source = ["hockeytrack.poller"]`. Publishing under `hockeytrack.synthetic` therefore matches no existing rule and can reach no subscriber. Nothing in Terraform changes. Beyond that:

- The game id is `real + 9_000_000_000`, eleven digits, which no real NHL game can be.
- The store and the archive are the in-memory fakes, so no DynamoDB row and no S3 object is written. There is nothing to tear down.
- `-as-poller` publishes under the real source, which *does* reach the alert rules. It prints a warning and waits ten seconds first, so a mistake can be interrupted.

- [ ] **Step 1: Add the synthetic source and the constructor**

In `internal/events/events.go`, add to the const block after `Source`:

```go
	// SourceSynthetic marks events produced by a replayed game. Every
	// notification rule pins the real source, so a synthetic run reaches
	// no subscriber unless one opts in.
	SourceSynthetic = "hockeytrack.synthetic"
```

In `internal/events/eventbridge.go`, add a `source` field to `EventBridgePublisher`, use `p.source` in `Publish` instead of the package constant, and provide both constructors:

```go
type EventBridgePublisher struct {
	client  *eventbridge.Client
	busName string
	source  string
}

func NewEventBridgePublisher(client *eventbridge.Client, busName string) *EventBridgePublisher {
	return NewEventBridgePublisherWithSource(client, busName, Source)
}

// NewEventBridgePublisherWithSource publishes under an explicit source.
// Live-fire replays use SourceSynthetic so notification rules, which all
// pin the real source, cannot match them.
func NewEventBridgePublisherWithSource(client *eventbridge.Client, busName, source string) *EventBridgePublisher {
	return &EventBridgePublisher{client: client, busName: busName, source: source}
}
```

- [ ] **Step 2: Write the pacer test**

Create `internal/synth/pace_test.go`:

```go
package synth

import (
	"context"
	"testing"
	"time"
)

func TestPacerSleepsProportionalToGameTime(t *testing.T) {
	s, err := Snapshots(fixture(t, "2025020001"), Options{Interval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	var slept []time.Duration
	p := Pacer(60, time.Minute, func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	})
	for i := range s {
		if err := p(context.Background(), s[i], i); err != nil {
			t.Fatal(err)
		}
	}
	if len(slept) != len(s) {
		t.Fatalf("sleeps = %d, want %d", len(slept), len(s))
	}
	if slept[0] != 0 {
		t.Errorf("first sleep = %v, want 0", slept[0])
	}
	var total time.Duration
	for _, d := range slept {
		if d < 0 {
			t.Fatalf("negative sleep %v", d)
		}
		total += d
	}
	if total == 0 {
		t.Error("paced run never slept")
	}
	// A full game at 60x should take on the order of minutes, not hours.
	if total > 30*time.Minute {
		t.Errorf("total pacing = %v, implausible at 60x", total)
	}
}

func TestPacerCapsLongGaps(t *testing.T) {
	s, err := Snapshots(fixture(t, "2025020001"), Options{Interval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	var maxSleep time.Duration
	p := Pacer(1, 2*time.Second, func(_ context.Context, d time.Duration) error {
		if d > maxSleep {
			maxSleep = d
		}
		return nil
	})
	for i := range s {
		if err := p(context.Background(), s[i], i); err != nil {
			t.Fatal(err)
		}
	}
	if maxSleep > 2*time.Second {
		t.Errorf("max sleep = %v, want the 2s cap", maxSleep)
	}
}
```

- [ ] **Step 3: Run it to verify it fails**

Run: `go test ./internal/synth/ -run TestPacer`
Expected: FAIL — `undefined: Pacer`.

- [ ] **Step 4: Write the pacer**

Create `internal/synth/pace.go`:

```go
package synth

import (
	"context"
	"time"

	"hockeytrack/internal/nhl"
)

// Pacer returns a Feed.Before hook that spaces snapshots out the way the
// game itself was spaced, divided by speed. An intermission or a period
// break can be a long gap in game time, so each wait is capped: the point
// is to reproduce the rhythm consumers see, not to sit idle.
//
// speed is a multiplier over game time (60 means a minute of hockey per
// second). sleep is injected so tests observe the waits instead of taking
// them.
func Pacer(speed float64, maxWait time.Duration, sleep func(context.Context, time.Duration) error) func(context.Context, Snapshot, int) error {
	if speed <= 0 {
		speed = 1
	}
	prev := -1
	var prevElapsed int
	return func(ctx context.Context, s Snapshot, i int) error {
		elapsed := gameElapsed(s.PBP)
		if prev < 0 {
			prev, prevElapsed = i, elapsed
			return sleep(ctx, 0)
		}
		delta := elapsed - prevElapsed
		prev, prevElapsed = i, elapsed
		if delta <= 0 {
			return sleep(ctx, 0)
		}
		wait := time.Duration(float64(delta) * float64(time.Second) / speed)
		if maxWait > 0 && wait > maxWait {
			wait = maxWait
		}
		return sleep(ctx, wait)
	}
}

// gameElapsed is total elapsed game time in seconds, counting each finished
// period as a full twenty minutes. It only has to increase monotonically,
// which is all the pacer needs; overtime lengths do not have to be exact.
func gameElapsed(p *nhl.PlayByPlay) int {
	period := p.PeriodDescriptor.Number
	if period < 1 {
		period = 1
	}
	inPeriod := 1200 - p.Clock.SecondsRemaining
	if inPeriod < 0 {
		inPeriod = 0
	}
	return (period-1)*1200 + inPeriod
}
```

- [ ] **Step 5: Run the pacer tests**

Run: `go test ./internal/synth/ -run TestPacer -v`
Expected: PASS.

- [ ] **Step 6: Write the live-fire command**

Create `cmd/livefire/main.go`:

```go
// Livefire replays an archived game onto the real EventBridge bus, so the
// website, the notification path and the scoreboard can be exercised out of
// season.
//
// It is safe by construction. Events carry the source hockeytrack.synthetic,
// which no notification rule matches — every rule in terraform/notifications.tf
// pins hockeytrack.poller. The game id is the real id plus 9,000,000,000, so
// it is eleven digits and cannot collide with a real game. The game store and
// the archive are in-memory fakes, so no DynamoDB row and no S3 object is
// written and there is nothing to clean up afterwards.
//
// -as-poller publishes under the real source instead, which does reach the
// notification rules. That is the one flag that can send a text message.
//
// Usage:
//
//	livefire -game 2024021299 -bus hockeytrack -speed 60
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"hockeytrack/internal/events"
	"hockeytrack/internal/nhl"
	"hockeytrack/internal/poller"
	"hockeytrack/internal/store"
	"hockeytrack/internal/synth"
)

// syntheticOffset lifts a ten-digit NHL game id into eleven digits, where no
// real game can ever be.
const syntheticOffset = 9_000_000_000

func main() {
	game := flag.Int64("game", 0, "archived game id to replay (required)")
	bus := flag.String("bus", "hockeytrack", "EventBridge bus name")
	bucket := flag.String("bucket", os.Getenv("HOCKEYTRACK_RAW_BUCKET"), "raw archive bucket (default $HOCKEYTRACK_RAW_BUCKET)")
	cache := flag.String("cache", synth.DefaultCacheDir(), "directory for downloaded finals")
	speed := flag.Float64("speed", 60, "game-time multiplier; 1 is real time")
	interval := flag.Duration("interval", 30*time.Second, "game time between snapshots")
	capWait := flag.Duration("cap", 5*time.Second, "longest wall-clock wait between snapshots")
	asPoller := flag.Bool("as-poller", false, "publish under the real poller source; THIS CAN SEND NOTIFICATIONS")
	dry := flag.Bool("dry-run", false, "print events instead of publishing them")
	flag.Parse()

	if *game == 0 {
		fmt.Fprintln(os.Stderr, "usage: livefire -game <id> [-speed 60] [-dry-run]")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	source := events.SourceSynthetic
	if *asPoller {
		source = events.Source
		fmt.Fprintf(os.Stderr,
			"WARNING: publishing as %q. Notification rules WILL match these events\nand subscribers may receive email or SMS. Ctrl-C within 10 seconds to abort.\n",
			source)
		select {
		case <-time.After(10 * time.Second):
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "aborted")
			return
		}
	}

	raw, err := loadFinal(ctx, *bucket, *cache, *game)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	syntheticID := *game + syntheticOffset
	snaps, err := synth.Snapshots(raw, synth.Options{Interval: *interval, GameID: syntheticID})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var first nhl.PlayByPlay
	if err := json.Unmarshal(raw, &first); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	pub, err := publisher(ctx, *bus, source, *dry)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	feed := synth.NewFeed(snaps)
	feed.Before = synth.Pacer(*speed, *capWait, sleep)

	fmt.Fprintf(os.Stderr, "livefire: %s @ %s (%s), real id %d, publishing as %d under %q, %d snapshots at %.0fx\n",
		first.AwayTeam.Abbrev, first.HomeTeam.Abbrev, first.GameDate, *game, syntheticID, source, len(snaps), *speed)

	gs := store.NewFakeGameStore()
	if err := gs.UpsertSchedule(ctx, store.GameRecord{
		GameID: syntheticID, Season: first.Season, GameDate: first.GameDate,
		HomeAbbrev: first.HomeTeam.Abbrev, AwayAbbrev: first.AwayTeam.Abbrev,
		GameState: "FUT", StartTimeUTC: time.Now(),
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	outcome, err := poller.Run(ctx, poller.Deps{
		Feed: feed, Store: gs, Archive: store.NewFakeArchive(), Pub: pub,
		Now: time.Now, Sleep: sleep,
	}, poller.DefaultConfig(), syntheticID, "livefire", func() bool { return false })
	if err != nil {
		fmt.Fprintln(os.Stderr, "livefire error:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "livefire done: outcome=%d\n", outcome)
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type printingPub struct{}

func (printingPub) Publish(_ context.Context, dt string, detail any) error {
	b, err := json.Marshal(map[string]any{"detailType": dt, "detail": detail})
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func publisher(ctx context.Context, bus, source string, dry bool) (events.Publisher, error) {
	if dry {
		return printingPub{}, nil
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	return events.NewEventBridgePublisherWithSource(eventbridge.NewFromConfig(cfg), bus, source), nil
}

func loadFinal(ctx context.Context, bucket, cache string, game int64) ([]byte, error) {
	if cache != "" {
		if b, err := os.ReadFile(filepath.Join(cache, fmt.Sprintf("%d.json", game))); err == nil {
			return b, nil
		}
	}
	if bucket == "" {
		return nil, fmt.Errorf("game %d is not cached and -bucket is empty; pass -bucket or set HOCKEYTRACK_RAW_BUCKET", game)
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	return synth.FinalPBP(ctx, store.NewS3Archive(s3.NewFromConfig(cfg), bucket), game, cache)
}
```

- [ ] **Step 7: Verify the dry run end to end**

```bash
go build ./cmd/livefire
cp internal/synth/testdata/2024021299.json "$HOME/.cache/hockeytrack/finals/"
./livefire -game 2024021299 -speed 100000 -dry-run | head -5
./livefire -game 2024021299 -speed 100000 -dry-run | grep -c 11024021299
rm -f livefire
```

Expected: event JSON on stdout, every event carrying `"gameId":11024021299`, and a non-zero count from the grep.

- [ ] **Step 8: Confirm no real publish happens without credentials being used**

Run: `go test ./...`
Expected: PASS. **Do not run `livefire` without `-dry-run` in this task** — the real-bus run is a deliberate operator action recorded in Task 5's documentation, and the controller performs it, not the implementer.

- [ ] **Step 9: Commit**

```bash
git add internal/events/ internal/synth/pace.go internal/synth/pace_test.go cmd/livefire/
git commit -m "livefire: pace a synthesized game onto the real bus under a synthetic source"
```

---

### Task 5: Make targets, CI and documentation (HOC-50)

**Files:**
- Modify: `Makefile`
- Modify: `README.md`
- Modify: `.github/workflows/*.yml` if one exists; otherwise skip the CI step and say so in the report.

**Interfaces:**
- Consumes: the `replay` and `livefire` commands and the `TestGolden` test from Tasks 2-4.
- Produces: nothing other tasks consume.

- [ ] **Step 1: Check whether CI exists**

```bash
ls -la .github/workflows/ 2>/dev/null || echo "no GitHub Actions workflows"
```

If there is no workflow, do not create one: `make test` already runs the offline suite and inventing CI is outside this task. Record the finding in your report.

- [ ] **Step 2: Add the Make targets**

In `Makefile`, add `replay golden golden-update livefire` to the `.PHONY` line, and append:

```makefile
# Replay one archived game through the poller offline, printing every event
# it publishes. GAME is an NHL game id; INTERVAL groups plays into snapshots
# of that much game time (0 for one snapshot per play).
GAME     ?=
INTERVAL ?= 30s
SPEED    ?= 60

replay:
	@test -n "$(GAME)" || { echo "usage: make replay GAME=2024021299 [INTERVAL=30s]"; exit 2; }
	HOCKEYTRACK_RAW_BUCKET=$$(cd terraform && terraform output -raw raw_bucket) \
		go run ./cmd/replay -game $(GAME) -interval $(INTERVAL)

# The golden event-stream regression suite. Runs offline from checked-in
# fixtures; needs no AWS credentials.
golden:
	go test ./internal/synth/ -run TestGolden

# Rewrite the golden files. Separate from `golden` so a regeneration is
# always deliberate: review the diff before committing it.
golden-update:
	go test ./internal/synth/ -run TestGolden -update -v

# Replay a game onto the REAL event bus under the synthetic source, which no
# notification rule matches. Add -as-poller by hand if you specifically want
# to test the notification path; that can send email and SMS.
livefire:
	@test -n "$(GAME)" || { echo "usage: make livefire GAME=2024021299 [SPEED=60]"; exit 2; }
	HOCKEYTRACK_RAW_BUCKET=$$(cd terraform && terraform output -raw raw_bucket) \
		go run ./cmd/livefire -game $(GAME) -speed $(SPEED) -interval $(INTERVAL)
```

- [ ] **Step 3: Verify the offline targets work**

```bash
make golden
make replay GAME=2025020001 INTERVAL=60s 2>&1 | tail -2
```

Expected: `golden` passes; `replay` ends with a `replay done: outcome=0` line. `replay` reads the cached copy, so it needs no credentials once the fixture is cached.

- [ ] **Step 4: Document it in the README**

Add a section after the backfill section. Match the surrounding prose style: full sentences, no bullet soup.

```markdown
## Synthesizing a live game

The NHL season is six months long and the pipeline is silent for the other
six. `cmd/replay` and `cmd/livefire` close that gap by reconstructing a live
game from a finished one: given any of the games in the archive, they rebuild
the sequence of play-by-play documents the poller *would* have seen and run
the real poll loop over them.

Offline, against in-memory fakes, printing every event to stdout:

```
make replay GAME=2024021299
```

A snapshot is a reconstruction, not a recording. The plays, clock, score,
shots, situation codes and rosters are exactly what the game produced, but
the cadence is chosen rather than observed, and the JSON field order differs
from the original. Pass `INTERVAL=0` for one snapshot per play, which is
denser than any real poll and the strictest test of the diff logic.

`make golden` runs the regression suite: seven curated games — regulation,
overtime, shootout, a match penalty, misconducts, a penalty shot and empty
net, and a 1917 game with no period markers at all — whose complete event
streams are recorded in `internal/synth/testdata/golden/`. It runs from
checked-in fixtures and needs no AWS credentials. When you change the event
contract on purpose, `make golden-update` rewrites the recordings; review
that diff carefully, because it is the contract other people build against.

To drive real consumers, `make livefire GAME=2024021299 SPEED=60` publishes
to the real EventBridge bus. This is safe by default and deliberately so.
Events carry the source `hockeytrack.synthetic`, and every notification rule
pins `hockeytrack.poller`, so nothing you do here can send anyone a text
message. The game id is the real id plus 9,000,000,000, which is eleven
digits and therefore cannot collide with a real NHL game. The game store and
the archive are in-memory fakes, so no DynamoDB row and no S3 object is
written and there is nothing to clean up.

The one exception is `-as-poller`, which publishes under the real source so
that the notification path itself can be tested. It reaches live subscribers.
The tool prints a warning and waits ten seconds before starting so a mistake
can be interrupted.
```

- [ ] **Step 5: Cross-reference from the scoreboard section**

Find the sentence in `README.md` that mentions the scoreboard as a consumer of the event stream and add one sentence: that the scoreboard device is developed against `make livefire`, because the bus is otherwise silent out of season.

- [ ] **Step 6: Run everything**

Run: `make test`
Expected: govulncheck clean, all tests pass.

- [ ] **Step 7: Commit**

```bash
git add Makefile README.md
git commit -m "replay: make targets and docs for synthesizing a live game"
```

---

## Notes for the controller

- Task 3 downloads five fixtures from S3. Those are read-only `GetObject` calls and are permitted; nothing else in this plan touches AWS.
- Task 4 explicitly forbids a real publish. The first real-bus run is an operator action for the controller to take after the branch is reviewed, and its result belongs in the ledger, not in a subagent's hands.
- The plan adds fields to `nhl.Play` in Task 1. Every later task depends on that; do not reorder.
- If a reviewer objects to `Feed.PlayByPlay` returning a shared `*nhl.PlayByPlay` rather than a copy: that is deliberate and documented. The poller only reads it, and copying every document would double a replay's memory.
