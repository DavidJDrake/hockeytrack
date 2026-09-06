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
	"strconv"
	"strings"
	"time"

	"hockeytrack/internal/nhl"
)

// pregameSeconds is what a feed shows before the opening faceoff.
const pregameSeconds = 1200

// critThreshold is the point in a final regulation period at which the NHL
// feed switches a one-goal game from LIVE to CRIT.
const critThreshold = 300

// Options controls how Snapshots paces and labels the reconstructed poll
// sequence.
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

	// Reject inputs that cannot be attributed to a team, or options that
	// cannot produce a valid document, before any snapshot is built.
	if final.HomeTeam.ID == 0 {
		return nil, fmt.Errorf("synth: home team id is %d; cannot attribute plays", final.HomeTeam.ID)
	}
	if final.AwayTeam.ID == 0 {
		return nil, fmt.Errorf("synth: away team id is %d; cannot attribute plays", final.AwayTeam.ID)
	}
	if final.HomeTeam.ID == final.AwayTeam.ID {
		return nil, fmt.Errorf("synth: home and away team ids are both %d; cannot attribute plays", final.HomeTeam.ID)
	}
	if opts.GameID < 0 {
		return nil, fmt.Errorf("synth: GameID is %d; must not be negative", opts.GameID)
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

	// A document with no plays (fact: pre-modern tiers can be this sparse)
	// still needs a terminal FINAL snapshot, or a poller driven by it would
	// never observe an end state.
	if len(cuts) == 0 {
		doc["gameState"] = "FINAL"
		doc["clock"] = clockDoc("00:00", 0, false, false)
		awayTeam["score"], homeTeam["score"] = final.AwayTeam.Score, final.HomeTeam.Score
		awayTeam["sog"], homeTeam["sog"] = final.AwayTeam.SOG, final.HomeTeam.SOG
		if err := emit(); err != nil {
			return nil, err
		}
		return out, nil
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
	var out []int
	// lastCut starts far in the past so the very first play always closes
	// a snapshot, the same way i == len(plays)-1 always closes the last.
	lastCut := -1 << 30
	lastPeriod := plays[0].PeriodDescriptor.Number
	for i, p := range plays {
		elapsed := parseClock(p.TimeInPeriod)
		if elapsed < 0 {
			// An unreadable clock must not force or suppress a cut: treat
			// the play as having made no progress since the last cut.
			elapsed = lastCut
		}
		periodChanged := p.PeriodDescriptor.Number != lastPeriod
		// Finding C: periodChanged only fires on the first play of the next
		// period, never on the period-end play itself (which still carries
		// the old period number), so a boundary marker is checked
		// explicitly and forces a cut regardless of interval. The
		// pre-modern tier has no boundary plays at all, so periodChanged
		// is kept as the only signal available there.
		boundary := p.TypeDescKey == "period-start" ||
			p.TypeDescKey == "period-end" ||
			p.TypeDescKey == "game-end"
		due := time.Duration(elapsed-lastCut)*time.Second >= interval
		switch {
		case i == len(plays)-1, periodChanged, boundary, due:
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
		remaining >= 0 && remaining <= critThreshold && margin <= 1 {
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
	if remaining < 0 {
		// A malformed clock string still travels through unchanged; only
		// the derived seconds count falls back, to zero.
		remaining = 0
	}
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

// parseClock reads the feed's MM:SS clock strings, returning -1 for anything
// it cannot read. Callers must distinguish that from a real 00:00, because a
// zero clock means "period over" and would otherwise make every malformed
// play look like the dying seconds of a one-goal game.
func parseClock(s string) int {
	m, sec, ok := strings.Cut(s, ":")
	if !ok {
		return -1
	}
	mins, err := strconv.Atoi(m)
	if err != nil || mins < 0 {
		return -1
	}
	secs, err := strconv.Atoi(sec)
	if err != nil || secs < 0 || secs > 59 {
		return -1
	}
	return mins*60 + secs
}
