package poller

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"hockeytrack/internal/events"
	"hockeytrack/internal/nhl"
	"hockeytrack/internal/store"
)

// The fixtures here are cut from this project's own archive of game
// 2026010004 (CHI at MIN, 2026-09-19): the last snapshot of the first
// intermission, the first snapshot of the second period, and the one 41
// seconds later. Each was trimmed to the same window of plays around the
// period break, and to four roster spots; nothing in what remains was edited.
//
// What they show. At the start of a period the NHL appends the period-start
// and the opening faceoff with PROVISIONAL sort orders in the 9000s, and
// renumbers them to their real positions within a minute or so. The eventId
// does not change. The poller used to remember the highest sort order it had
// sent and skip anything at or below it, so one poll landing inside that
// minute set the mark to 9004 and every play for the rest of the game --
// every goal, every penalty -- was skipped as already sent. On 2026-09-20,
// four of twelve games were in that state.

var fixedNow = time.Date(2026, 9, 20, 1, 0, 3, 0, time.UTC)

func snapshot(t *testing.T, name string) *nhl.PlayByPlay {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var p nhl.PlayByPlay
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

func ids(plays []nhl.Play) []int64 {
	out := make([]int64, len(plays))
	for i, p := range plays {
		out[i] = p.EventID
	}
	return out
}

func markSent(sent map[int64]bool, plays []nhl.Play) {
	for _, p := range plays {
		sent[p.EventID] = true
	}
}

func TestTheFixturesAreWhatTheyClaimToBe(t *testing.T) {
	prov := snapshot(t, "pbp_provisional.json")
	var high []int64
	for _, p := range prov.Plays {
		if p.SortOrder >= ProvisionalSortOrder {
			high = append(high, p.SortOrder)
		}
	}
	if len(high) != 2 {
		t.Fatalf("provisional snapshot has %d plays in the 9000s, want the period-start and the faceoff", len(high))
	}
	for _, p := range snapshot(t, "pbp_renumbered.json").Plays {
		if p.SortOrder >= ProvisionalSortOrder {
			t.Fatalf("renumbered snapshot still has sort order %d", p.SortOrder)
		}
	}
}

func TestAProvisionalPlayIsHeldBackUntilItHasItsRealNumber(t *testing.T) {
	sent := map[int64]bool{}
	markSent(sent, NewPlays(snapshot(t, "pbp_before_period.json").Plays, sent, false))

	// The poll that used to poison the game.
	got := NewPlays(snapshot(t, "pbp_provisional.json").Plays, sent, false)
	if len(got) != 0 {
		t.Fatalf("sent %v while they carried provisional numbers", ids(got))
	}

	// Forty-one seconds later they have real numbers, and a stoppage has
	// happened since. All three go, in order, numbered as the NHL now has them.
	got = NewPlays(snapshot(t, "pbp_renumbered.json").Plays, sent, false)
	want := []int64{386, 385, 24}
	if len(got) != 3 || got[0].EventID != want[0] || got[1].EventID != want[1] || got[2].EventID != want[2] {
		t.Fatalf("sent %v, want %v", ids(got), want)
	}
	if got[0].SortOrder != 283 || got[1].SortOrder != 284 || got[2].SortOrder != 285 {
		t.Errorf("sort orders %d %d %d, want 283 284 285", got[0].SortOrder, got[1].SortOrder, got[2].SortOrder)
	}
}

func TestNothingIsEverSentTwice(t *testing.T) {
	sent := map[int64]bool{}
	for _, name := range []string{"pbp_before_period.json", "pbp_provisional.json", "pbp_renumbered.json", "pbp_renumbered.json"} {
		plays := NewPlays(snapshot(t, name).Plays, sent, false)
		for _, p := range plays {
			if sent[p.EventID] {
				t.Fatalf("%s: event %d sent twice", name, p.EventID)
			}
		}
		markSent(sent, plays)
	}
	if got := NewPlays(snapshot(t, "pbp_renumbered.json").Plays, sent, false); len(got) != 0 {
		t.Errorf("a snapshot already sent produced %v", ids(got))
	}
}

// The NHL also inserts plays late, below numbers already sent: a penalty
// added on review, a correction. A highest-number mark loses those too.
func TestAPlayInsertedBelowTheHighestNumberSentIsStillSent(t *testing.T) {
	plays := snapshot(t, "pbp_renumbered.json").Plays
	sent := map[int64]bool{}
	var late nhl.Play
	for _, p := range plays {
		if p.EventID == 370 { // a faceoff at sort order 275, well below the 285 that has gone
			late = p
			continue
		}
		sent[p.EventID] = true
	}
	got := NewPlays(plays, sent, false)
	if len(got) != 1 || got[0].EventID != late.EventID {
		t.Fatalf("sent %v, want only the late play %d", ids(got), late.EventID)
	}
}

// If a play never gets a real number, the end of the game is the last chance
// to send it. Nothing is lost for the sake of a tidy sequence.
func TestAtTheFinalWhateverIsLeftIsSent(t *testing.T) {
	sent := map[int64]bool{}
	markSent(sent, NewPlays(snapshot(t, "pbp_before_period.json").Plays, sent, false))
	got := NewPlays(snapshot(t, "pbp_provisional.json").Plays, sent, true)
	if len(got) != 2 {
		t.Fatalf("at the final, sent %v; want the two held-back plays", ids(got))
	}
}

// Games that were being polled when this was deployed have a mark and no
// list of what was sent. Everything at or below the mark was either sent or
// lost; sending it again now would replay a whole game's goals as news.
func TestAGameAlreadyUnderWayIsNotReplayed(t *testing.T) {
	plays := snapshot(t, "pbp_renumbered.json").Plays

	healthy := SeedSent(plays, 276) // the mark after the first period
	got := NewPlays(plays, healthy, false)
	if len(got) != 3 {
		t.Errorf("a healthy game resumes with %v, want the three plays after 276", ids(got))
	}

	poisoned := SeedSent(plays, 9004)
	if got := NewPlays(plays, poisoned, false); len(got) != 0 {
		t.Errorf("a poisoned game replayed %v", ids(got))
	}
	// ...and from here on it works: the next play the NHL adds is sent.
	next := append(append([]nhl.Play{}, plays...), nhl.Play{EventID: 999, SortOrder: 290, TypeDescKey: "penalty"})
	if got := NewPlays(next, poisoned, false); len(got) != 1 || got[0].EventID != 999 {
		t.Errorf("a poisoned game is still stuck: sent %v", ids(got))
	}

	if SeedSent(plays, 0) != nil {
		t.Error("a game that has sent nothing needs no seeding")
	}
}

func TestThePlayEventCarriesTheIdThatDoesNotChange(t *testing.T) {
	pbp := snapshot(t, "pbp_renumbered.json")
	for _, p := range pbp.Plays {
		e := BuildPlayEvent(pbp, p, map[string]int{})
		if e.EventID != p.EventID || e.EventID == 0 {
			t.Fatalf("event for play %d carries eventId %d", p.EventID, e.EventID)
		}
	}
}

// The heartbeat read situationCode from the top of the document, where the
// NHL does not put it. It is under "situation", and only while there is one:
// so no power play ever reached a consumer.
func TestTheSituationIsReadFromWhereTheNHLPutsIt(t *testing.T) {
	pp := snapshot(t, "pbp_power_play.json")
	if got := BuildClockEvent(pp, fixedNow).SituationCode; got != "1451" {
		t.Errorf("during a power play the heartbeat says %q, want 1451", got)
	}
	// Even strength: the NHL sends no situation at all, and that absence is
	// the information. Not the last play's code: a penalty can expire with
	// no play to mark it, and a stale 1451 would leave a power play lit.
	even := snapshot(t, "pbp_renumbered.json")
	if got := BuildClockEvent(even, fixedNow).SituationCode; got != "" {
		t.Errorf("at even strength the heartbeat says %q, want nothing", got)
	}
}

// --- the same thing, through the real loop and the stored state ---

func rawSnapshot(t *testing.T, name, state string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if state != "" {
		m["gameState"] = state
	}
	out, _ := json.Marshal(m)
	return out
}

func seedRealGame(t *testing.T, gs *store.FakeGameStore, rec store.GameRecord) {
	t.Helper()
	rec.GameID, rec.Season, rec.GameDate = 2026010004, 20262027, "2026-09-19"
	rec.HomeAbbrev, rec.AwayAbbrev = "MIN", "CHI"
	if rec.GameState == "" {
		rec.GameState = "LIVE"
	}
	if err := gs.UpsertSchedule(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
}

func playIDs(pub *events.FakePublisher) []int64 {
	var out []int64
	for _, e := range pub.Published {
		if e.DetailType == events.DTPlay {
			out = append(out, e.Detail.(events.PlayEvent).EventID)
		}
	}
	return out
}

func TestAPollThatCatchesTheProvisionalNumbersNoLongerStopsTheGame(t *testing.T) {
	feed := &scriptedFeed{snapshots: [][]byte{
		rawSnapshot(t, "pbp_before_period.json", ""),
		rawSnapshot(t, "pbp_provisional.json", ""),
		rawSnapshot(t, "pbp_renumbered.json", ""),
		rawSnapshot(t, "pbp_renumbered.json", "OFF"),
	}}
	d, gs, _, pub := testDeps(feed)
	seedRealGame(t, gs, store.GameRecord{})
	if out, err := Run(context.Background(), d, DefaultConfig(), 2026010004, "link", func() bool { return false }); err != nil || out != OutcomeFinal {
		t.Fatalf("outcome=%v err=%v", out, err)
	}

	got := playIDs(pub)
	seen := map[int64]int{}
	for _, id := range got {
		seen[id]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("event %d published %d times", id, n)
		}
	}
	for _, want := range []int64{386, 385, 24} { // the period-start, the faceoff, and the play AFTER them
		if seen[want] != 1 {
			t.Errorf("event %d was never published: %v", want, got)
		}
	}
	for _, e := range pub.Published {
		if e.DetailType == events.DTPlay && e.Detail.(events.PlayEvent).Seq >= ProvisionalSortOrder {
			t.Errorf("published event %d with provisional seq %d", e.Detail.(events.PlayEvent).EventID, e.Detail.(events.PlayEvent).Seq)
		}
	}

	rec, _ := gs.Get(context.Background(), 2026010004)
	if rec.LastPlaySortOrder >= ProvisionalSortOrder {
		t.Errorf("the recorded mark is %d; a provisional number must never be recorded", rec.LastPlaySortOrder)
	}
	if len(rec.SentEventIDs) != len(seen) {
		t.Errorf("stored %d sent ids, published %d plays", len(rec.SentEventIDs), len(seen))
	}
}

func TestAGameAlreadyPoisonedStartsWorkingAgainWithoutReplayingItself(t *testing.T) {
	// The state four live games were in when this was written: a mark in the
	// 9000s and no list.
	feed := &scriptedFeed{snapshots: [][]byte{
		rawSnapshot(t, "pbp_provisional.json", ""),
		rawSnapshot(t, "pbp_renumbered.json", ""),
		rawSnapshot(t, "pbp_renumbered.json", "OFF"),
	}}
	d, gs, _, pub := testDeps(feed)
	seedRealGame(t, gs, store.GameRecord{LastPlaySortOrder: 9004})
	if out, err := Run(context.Background(), d, DefaultConfig(), 2026010004, "link", func() bool { return false }); err != nil || out != OutcomeFinal {
		t.Fatalf("outcome=%v err=%v", out, err)
	}
	// Everything in the feed at the moment of the fix is treated as gone.
	// The one play that arrives afterwards is published: the game is unstuck.
	got := playIDs(pub)
	if len(got) != 1 || got[0] != 24 {
		t.Errorf("published %v, want only the play that arrived after the fix (24)", got)
	}
}

func TestTheSentListSurvivesAHandOff(t *testing.T) {
	d, gs, _, pub := testDeps(&scriptedFeed{snapshots: [][]byte{rawSnapshot(t, "pbp_before_period.json", "")}})
	seedRealGame(t, gs, store.GameRecord{})
	calls := 0
	Run(context.Background(), d, DefaultConfig(), 2026010004, "link1", func() bool { calls++; return calls > 1 })
	first := len(playIDs(pub))

	d.Feed = &scriptedFeed{snapshots: [][]byte{rawSnapshot(t, "pbp_renumbered.json", "OFF")}}
	if out, err := Run(context.Background(), d, DefaultConfig(), 2026010004, "link2", func() bool { return false }); err != nil || out != OutcomeFinal {
		t.Fatalf("link2 outcome=%v err=%v", out, err)
	}
	seen := map[int64]bool{}
	for _, id := range playIDs(pub) {
		if seen[id] {
			t.Fatalf("event %d published by both links (first link sent %d)", id, first)
		}
		seen[id] = true
	}
	if !seen[24] || !seen[386] {
		t.Errorf("the second link did not pick up where the first stopped: %v", playIDs(pub))
	}
}
