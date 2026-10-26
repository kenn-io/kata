package harness

import (
	"strings"
	"testing"

	"go.kenn.io/kata/internal/teammate"
	"hegel.dev/go/hegel"
)

// Spec property 3: every canonical harness and arbitrary model/provider pair
// produces a portable, bounded, deterministic handle retaining its harness.
func TestPropertyHandleValid(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		d := Detection{
			Harness:  hegel.Draw(ht, hegel.SampledFrom(Names())),
			Model:    hegel.Draw(ht, hegel.Text()),
			Provider: hegel.Draw(ht, hegel.Text()),
		}
		got := Handle(d, ModeModel)
		if err := teammate.Validate(got); err != nil {
			ht.Fatalf("Handle(%+v) = %q: %v", d, got, err)
		}
		if len(got) > 64 {
			ht.Fatalf("Handle(%+v) = %q: exceeds 64 bytes", d, got)
		}
		if prefix, _, _ := strings.Cut(got, "."); prefix != d.Harness {
			ht.Fatalf("Handle(%+v) = %q: prefix = %q", d, got, prefix)
		}
		if again := Handle(d, ModeModel); again != got {
			ht.Fatalf("Handle(%+v) changed from %q to %q", d, got, again)
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

// Spec property 4: harness mode is exactly the model-mode harness prefix.
func TestPropertyHarnessModeIsPrefix(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		d := Detection{
			Harness:  hegel.Draw(ht, hegel.SampledFrom(Names())),
			Model:    hegel.Draw(ht, hegel.Text()),
			Provider: hegel.Draw(ht, hegel.Text()),
		}
		prefix, _, _ := strings.Cut(Handle(d, ModeModel), ".")
		if got := Handle(d, ModeHarness); got != prefix || got != d.Harness {
			ht.Fatalf("Handle(%+v, harness) = %q; model prefix = %q, harness = %q", d, got, prefix, d.Harness)
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

// Parsing accepts exactly the three documented mode spellings.
func TestPropertyParseMode(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		input := hegel.Draw(ht, hegel.Text())
		got, err := ParseMode(input)
		switch input {
		case "model", "harness", "off":
			if err != nil || string(got) != input {
				ht.Fatalf("ParseMode(%q) = %q, %v", input, got, err)
			}
		default:
			if err == nil {
				ht.Fatalf("ParseMode(%q) = %q, nil; want validation error", input, got)
			}
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}
