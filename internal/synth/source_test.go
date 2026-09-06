package synth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"hockeytrack/internal/store"
)

func nowForTest() time.Time { return time.Date(2025, 10, 7, 23, 0, 0, 0, time.UTC) }

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
