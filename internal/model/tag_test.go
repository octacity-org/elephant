package model

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestNormalizeTags(t *testing.T) {
	got, err := NormalizeTags([]string{" Build ", "zova", "build", "host:Mac-Studio", "a/b_c.d"})
	if err != nil || !slices.Equal(got, []string{"a/b_c.d", "build", "host:mac-studio", "zova"}) {
		t.Fatalf("got %q, %v", got, err)
	}
	if got, err := NormalizeTags(nil); err != nil || len(got) != 0 {
		t.Fatalf("nil: %q %v", got, err)
	}
	for _, bad := range []string{"", "  ", "two words", "emoji🐘", "semi;colon", strings.Repeat("a", 65), "-leading", ":leading"} {
		if _, err := NormalizeTags([]string{bad}); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%q accepted: %v", bad, err)
		}
	}
	if _, err := NormalizeTags([]string{strings.Repeat("a", 64)}); err != nil {
		t.Fatalf("64-byte tag rejected: %v", err)
	}
	many := make([]string, MaxTags+1)
	for i := range many {
		many[i] = fmt.Sprint("t", i)
	}
	if _, err := NormalizeTags(many[:MaxTags]); err != nil {
		t.Fatalf("%d tags rejected: %v", MaxTags, err)
	}
	if _, err := NormalizeTags(many); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("%d tags accepted: %v", MaxTags+1, err)
	}
	if node := TagNode("build"); node != "tag:build" {
		t.Fatal(node)
	}
	if name, ok := TagFromNode("tag:host:a"); !ok || name != "host:a" {
		t.Fatal(name, ok)
	}
	if _, ok := TagFromNode("file:p:x"); ok {
		t.Fatal("file node read as tag")
	}
}
