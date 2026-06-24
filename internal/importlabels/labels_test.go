package importlabels

import (
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"lowercase", "Needs Review", "needs-review"},
		{"whitespace to dash", " needs\treview \nnow ", "needs-review-now"},
		{"invalid characters replaced", "Bad Label! @#$ ok", "bad-label-ok"},
		{"empty fallback", "!!!", "imported"},
		{"allowed punctuation preserved", "source:beads.release_1", "source:beads.release_1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Normalize(tt.in); got != tt.want {
				t.Fatalf("Normalize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNormalizeMaxHashTruncates(t *testing.T) {
	got := Normalize(strings.Repeat("x", 100))
	want := strings.Repeat("x", 55) + "-09ecb6eb"
	if got != want {
		t.Fatalf("Normalize(long) = %q, want %q", got, want)
	}
	if len(got) != 64 {
		t.Fatalf("Normalize(long) length = %d, want 64", len(got))
	}
}

func TestAppendNormalizedDeduplicates(t *testing.T) {
	seen := map[string]struct{}{}
	got := AppendNormalized([]string{"existing"}, seen,
		"Needs Review",
		"needs-review",
		"needs   review!!!",
		"Other",
	)

	want := []string{"existing", "needs-review", "other"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("AppendNormalized() = %#v, want %#v", got, want)
	}
}
