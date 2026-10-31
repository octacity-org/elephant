package model

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

// SearchQuery filters deterministic text retrieval. Query is split by
// SearchTerms, and an entry matches when every term appears in its title or
// body. Empty Query matches all entries when other filters are set; results sort newest first with ID
// tie-break and are bounded by Limit. Commit-range and file filters are not
// part of the store contract: Git ancestry and graph relations live outside
// SQL, so the application layer applies them before pagination.
type SearchQuery struct {
	Query         string
	Kind          Kind
	Status        string
	Actor         string
	TargetVersion string
	File          string
	UpdatedAfter  *time.Time
	UpdatedBefore *time.Time
	Limit         int
	Offset        int
}

// MaxSearchTerms bounds how many terms one query may contain.
const MaxSearchTerms = 16

// SearchTerms splits a query into terms on whitespace. Text inside double
// quotes is one term, so a quoted phrase must appear as written; a quote
// left open runs to the end of the query. Empty terms are dropped.
func SearchTerms(query string) ([]string, error) {
	var terms []string
	var term strings.Builder
	quoted := false
	flush := func() {
		if text := strings.TrimSpace(term.String()); text != "" {
			terms = append(terms, text)
		}
		term.Reset()
	}
	for _, r := range query {
		switch {
		case r == '"':
			flush()
			quoted = !quoted
		case unicode.IsSpace(r) && !quoted:
			flush()
		default:
			term.WriteRune(r)
		}
	}
	flush()
	if len(terms) > MaxSearchTerms {
		return nil, fmt.Errorf("%w: search query has %d terms; at most %d are allowed", ErrInvalidInput, len(terms), MaxSearchTerms)
	}
	return terms, nil
}
