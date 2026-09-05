package main

import (
	"reflect"
	"testing"
)

func TestSelectSeasons(t *testing.T) {
	all := []int64{19171918, 19181919, 20032004, 20052006, 20242025, 20252026, 20262027}
	cases := []struct {
		spec string
		want []int64
		err  bool
	}{
		{"all", []int64{20262027, 20252026, 20242025, 20052006, 20032004, 19181919, 19171918}, false},
		{"20252026", []int64{20252026}, false},
		{"20032004-20242025", []int64{20242025, 20052006, 20032004}, false},
		{"20042005", nil, true},          // lockout year: not a season
		{"20242025-20032004", nil, true}, // reversed range
		{"19001901-19101911", nil, true}, // nothing in range
		{"nope", nil, true},
	}
	for _, c := range cases {
		got, err := selectSeasons(all, c.spec)
		if c.err {
			if err == nil {
				t.Errorf("selectSeasons(%q) expected error, got %v", c.spec, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("selectSeasons(%q): %v", c.spec, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("selectSeasons(%q) = %v, want %v", c.spec, got, c.want)
		}
	}
}
