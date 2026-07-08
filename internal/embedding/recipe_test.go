package embedding

import (
	"strings"
	"testing"
)

func TestEmbedTextNoTruncation(t *testing.T) {
	long := strings.Repeat("x", 20000)
	got := EmbedText("t", long)
	if want := "t\n\n" + long; got != want {
		t.Fatalf("EmbedText truncated: got %d runes, want %d", len([]rune(got)), len([]rune(want)))
	}
}
