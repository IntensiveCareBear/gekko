package main

import (
	"testing"
	"time"
)

func TestBearCooldown(t *testing.T) {
	old := bearCooldownDur
	defer func() { bearCooldownDur = old }()

	bearCooldownDur = 50 * time.Millisecond
	if inBearCooldown("KXA") {
		t.Fatal("unmarked ticker must not be in cooldown")
	}
	markBearCooldown("KXA")
	if !inBearCooldown("KXA") || inBearCooldown("KXB") {
		t.Fatal("cooldown must apply to the marked ticker only")
	}
	time.Sleep(60 * time.Millisecond)
	if inBearCooldown("KXA") {
		t.Fatal("cooldown must expire")
	}

	bearCooldownDur = 0
	markBearCooldown("KXC")
	if inBearCooldown("KXC") {
		t.Fatal("BEARISH_COOLDOWN=0 must disable the cooldown")
	}
}
