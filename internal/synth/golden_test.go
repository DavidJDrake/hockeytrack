package synth_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	{"2024021300", "penalty shot, eleven goals in one game"},
	{"2024021292", "shutout with a genuine empty-net goal"},
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

func splitLines(s string) []string { return strings.Split(s, "\n") }

func itoa(n int) string { return strconv.Itoa(n) }
