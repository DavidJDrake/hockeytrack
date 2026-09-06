package synth

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

// TestFinalPBPRemovesTempFileWhenRenameFails is the real regression test for
// the atomic cache write: it forces os.Rename to fail *after* writeCacheAtomic
// has already created its temp file, which is the only path where the cleanup
// matters. A directory sitting at the destination makes the rename fail with
// EISDIR while everything before it succeeds.
//
// Its sibling below blocks os.MkdirAll instead, which short-circuits before
// the write path is entered — useful, but it cannot catch a missing cleanup.
func TestFinalPBPRemovesTempFileWhenRenameFails(t *testing.T) {
	a := store.NewFakeArchive()
	ctx := context.Background()
	body := fixture(t, "1917020001")
	if err := a.Put(ctx, store.FinalKey(19171918, "1917-12-19", 1917020001, "pbp"), body); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	// Occupy the destination with a directory so the rename cannot succeed.
	if err := os.Mkdir(filepath.Join(dir, "1917020001.json"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := FinalPBP(ctx, a, 1917020001, dir)
	if err != nil {
		t.Fatalf("a failed cache write must not fail the run: %v", err)
	}
	if len(got) != len(body) {
		t.Errorf("got %d bytes, want %d", len(got), len(body))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".final-") {
			t.Fatalf("temp file %q left behind after a failed rename", e.Name())
		}
	}
}

// TestFinalPBPLeavesNoTempFileOnFailure covers finding C from fix round 1: a
// failed cache write (including a failed rename) must not leave a
// .final-*.json temp file behind, and must never fail the run itself.
func TestFinalPBPLeavesNoTempFileOnFailure(t *testing.T) {
	a := store.NewFakeArchive()
	ctx := context.Background()
	body := fixture(t, "1917020001")
	if err := a.Put(ctx, store.FinalKey(19171918, "1917-12-19", 1917020001, "pbp"), body); err != nil {
		t.Fatal(err)
	}

	// A file where the cache directory should be makes os.MkdirAll fail,
	// so the write path is never even reached.
	parent := t.TempDir()
	blocked := filepath.Join(parent, "cache")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := FinalPBP(ctx, a, 1917020001, blocked)
	if err != nil {
		t.Fatalf("a cache-write failure must not fail the run: %v", err)
	}
	if len(got) != len(body) {
		t.Errorf("got %d bytes, want %d", len(got), len(body))
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".final-") {
			t.Errorf("temp file %q leaked into %s", e.Name(), parent)
		}
	}

	// In a writable directory, a successful fetch leaves exactly the one
	// named cache file behind, and no leftover temp file beside it.
	dir := t.TempDir()
	if _, err := FinalPBP(ctx, a, 1917020001, dir); err != nil {
		t.Fatal(err)
	}
	entries, err = os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var named, temp int
	for _, e := range entries {
		switch {
		case e.Name() == "1917020001.json":
			named++
		case strings.HasPrefix(e.Name(), ".final-"):
			temp++
		}
	}
	if named != 1 {
		t.Errorf("cache dir has %d files named 1917020001.json, want 1", named)
	}
	if temp != 0 {
		t.Errorf("cache dir has %d leftover .final-* temp files, want 0", temp)
	}
}
