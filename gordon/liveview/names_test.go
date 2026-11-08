package main

import (
	"strings"
	"testing"
)

// gordonName is deterministic and well-formed for every persona id.
func TestGordonNameDeterministic(t *testing.T) {
	ids := []string{"bull", "bear", "cautious-bull", "neutral", "aggressive-bear"}
	for _, id := range ids {
		a := gordonName(id)
		b := gordonName(id)
		if a != b {
			t.Errorf("%q: not deterministic (%q vs %q)", id, a, b)
		}
		if !strings.HasSuffix(a, " Gordon") {
			t.Errorf("%q: name %q lacks the Gordon surname", id, a)
		}
		if strings.HasPrefix(a, " ") {
			t.Errorf("%q: name %q has an empty first name", id, a)
		}
	}
}

func TestGordonNameEmptyID(t *testing.T) {
	if got := gordonName(""); got != "Gordon" {
		t.Errorf(`gordonName("") = %q, want "Gordon"`, got)
	}
}
