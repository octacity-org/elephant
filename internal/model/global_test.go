package model

import (
	"strings"
	"testing"
)

func TestHostName(t *testing.T) {
	for raw, want := range map[string]string{
		"Atas-MacBook-Pro.local": "atas-macbook-pro.local",
		" Mac Studio ":           "mac-studio",
		"büro_1":                 "b--ro_1",
		strings.Repeat("x", 80):  strings.Repeat("x", maxTagLen-len(HostTag)),
	} {
		got := HostName(raw)
		if got != want {
			t.Fatalf("HostName(%q) = %q, want %q", raw, got, want)
		}
		if _, err := NormalizeTags([]string{HostTag + got}); err != nil {
			t.Fatalf("host tag for %q: %v", raw, err)
		}
	}
}
