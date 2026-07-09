package main

import (
	"strings"
	"testing"
	"time"
)

// TestDurationFlagBareNumberSuggestsUnit: a unitless number is the common
// agent mistake; it must stay rejected (ambiguous) but the error must
// suggest the seconds spelling rather than echo the stock parse failure.
func TestDurationFlagBareNumberSuggestsUnit(t *testing.T) {
	for _, in := range []string{"1800", "1800.5", " 1800 "} {
		var d time.Duration
		err := durationFlag{&d}.Set(in)
		if err == nil {
			t.Fatalf("Set(%q): want error, got nil", in)
		}
		if !strings.Contains(err.Error(), `"`+strings.TrimSpace(in)+`s"`) {
			t.Fatalf("Set(%q): error %q does not suggest the seconds spelling", in, err)
		}
	}
}

// TestDurationFlagGarbageGetsGuidance: non-numeric junk gets the generic
// guidance message with example spellings, not a bare parse error.
func TestDurationFlagGarbageGetsGuidance(t *testing.T) {
	var d time.Duration
	err := durationFlag{&d}.Set("soon")
	if err == nil {
		t.Fatal("Set(\"soon\"): want error, got nil")
	}
	if !strings.Contains(err.Error(), "30s, 5m, or 1h30m") {
		t.Fatalf("Set(\"soon\"): error %q lacks example spellings", err)
	}
}

// TestDurationFlagValidValues: valid Go durations (and the unit-less zero,
// which ParseDuration accepts) must parse exactly, including surrounding
// whitespace.
func TestDurationFlagValidValues(t *testing.T) {
	cases := map[string]time.Duration{
		"30m":   30 * time.Minute,
		"0":     0,
		"1h30m": 90 * time.Minute,
		" 2s ":  2 * time.Second,
		"1800s": 1800 * time.Second,
		"-1s":   -time.Second, // range checks are the command's job
	}
	for in, want := range cases {
		var d time.Duration
		if err := (durationFlag{&d}).Set(in); err != nil {
			t.Fatalf("Set(%q): unexpected error: %v", in, err)
		}
		if d != want {
			t.Fatalf("Set(%q) = %v, want %v", in, d, want)
		}
	}
}

// TestDurationFlagStringShowsCurrentValue: String() backs the flag's default
// rendering in --help, so it must reflect the pointed-at value.
func TestDurationFlagStringShowsCurrentValue(t *testing.T) {
	d := 2 * time.Second
	if got := (durationFlag{&d}).String(); got != "2s" {
		t.Fatalf("String() = %q, want \"2s\"", got)
	}
	if got := (durationFlag{nil}).String(); got != "0s" {
		t.Fatalf("String() on nil = %q, want \"0s\"", got)
	}
}
