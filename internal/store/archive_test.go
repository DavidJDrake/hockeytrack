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
