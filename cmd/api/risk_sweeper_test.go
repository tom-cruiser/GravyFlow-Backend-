package main

import (
	"reflect"
	"testing"
)

func TestRiskSweeperRequiresSustainedLoad(t *testing.T) {
	r := newRiskSweeper(nil)

	rounds := []struct {
		high []string
		want []string
	}{
		{[]string{"a", "b"}, []string{}},
		{[]string{"a", "b"}, []string{}},
		{[]string{"a"}, []string{"a"}},      // a: 3 in a row; b dropped out and resets
		{[]string{"a", "b"}, []string{"a"}}, // a keeps alerting (deduped in SQL); b back to 1
		{[]string{}, []string{}},            // everything calms down
		{[]string{"a"}, []string{}},         // a starts over from 1
	}
	for i, round := range rounds {
		if got := r.observe(round.high); !reflect.DeepEqual(got, round.want) {
			t.Fatalf("round %d: got %v, want %v", i, got, round.want)
		}
	}
}
