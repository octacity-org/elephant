package model

import (
	"fmt"
	"slices"
	"strings"
)

// Tags are short labels on entries, stored as tag:<name> graph nodes that
// entries reach through a tagged edge. Tag nodes carry no project, so the
// same name groups entries across projects; readers filter by ownership.
const (
	MaxTags   = 16
	maxTagLen = 64
	TagEdge   = "tagged"
)

// TagNode returns the graph node of a normalized tag.
func TagNode(name string) string { return "tag:" + name }

// TagFromNode returns the tag name of a tag node.
func TagFromNode(node string) (string, bool) { return strings.CutPrefix(node, "tag:") }

// NormalizeTags lowercases and trims tags, then validates, deduplicates and
// sorts them. A tag is 1–64 bytes of a-z, 0-9, and - _ . : /, starting with
// a letter or digit.
func NormalizeTags(raw []string) ([]string, error) {
	out := make([]string, 0, len(raw))
	for _, tag := range raw {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if !validTag(tag) {
			return nil, fmt.Errorf("%w: tag %q must be 1–%d characters of a-z, 0-9, and - _ . : /, starting with a letter or digit", ErrInvalidInput, tag, maxTagLen)
		}
		out = append(out, tag)
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) > MaxTags {
		return nil, fmt.Errorf("%w: at most %d tags per entry", ErrInvalidInput, MaxTags)
	}
	return out, nil
}

func validTag(tag string) bool {
	if tag == "" || len(tag) > maxTagLen {
		return false
	}
	for i, r := range tag {
		alnum := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if !alnum && (i == 0 || !strings.ContainsRune("-_.:/", r)) {
			return false
		}
	}
	return true
}
