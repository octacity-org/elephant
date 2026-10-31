package model

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestSearchTerms(t *testing.T) {
	for query, want := range map[string][]string{
		"":                              nil,
		"   ":                           nil,
		"shutdown":                      {"shutdown"},
		"  node_key_for_id\tHANDLES \n": {"node_key_for_id", "HANDLES"},
		`"edge type" HANDLES`:           {"edge type", "HANDLES"},
		`a"b c"d`:                       {"a", "b c", "d"},
		`"" "  " x`:                     {"x"},
		`"unterminated phrase`:          {"unterminated phrase"},
		`100% "a_b"`:                    {"100%", "a_b"},
	} {
		got, err := SearchTerms(query)
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("SearchTerms(%q) = %q, %v; want %q", query, got, err, want)
		}
	}
	if _, err := SearchTerms(strings.Repeat("w ", MaxSearchTerms)); err != nil {
		t.Fatalf("%d terms rejected: %v", MaxSearchTerms, err)
	}
	if _, err := SearchTerms(strings.Repeat("w ", MaxSearchTerms+1)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("%d terms accepted: %v", MaxSearchTerms+1, err)
	}
}
