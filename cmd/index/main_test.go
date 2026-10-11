package main

import (
	"testing"
	"time"
)

func TestParseSeasons(t *testing.T) {
	now := time.Date(2025, 10, 8, 0, 0, 0, 0, time.UTC)
	if s, err := parseSeasons("", now); err != nil || len(s) != 1 || s[0] != 20252026 {
		t.Errorf("default = %v %v", s, err)
	}
	if s, err := parseSeasons("20242025", now); err != nil || len(s) != 1 || s[0] != 20242025 {
		t.Errorf("one = %v %v", s, err)
	}
	s, err := parseSeasons("20222023-20242025", now)
	if err != nil || len(s) != 3 || s[0] != 20242025 || s[2] != 20222023 {
		t.Errorf("range = %v %v", s, err)
	}
	for _, bad := range []string{"2025", "20252025", "20242025-20222023", "x"} {
		if _, err := parseSeasons(bad, now); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}
