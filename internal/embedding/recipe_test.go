package embedding

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEmbedTextJoinsAndTruncatesOnRuneBoundary(t *testing.T) {
	got := EmbedText("Title", "Body")
	if got != "Title\n\nBody" {
		t.Fatalf("join: got %q", got)
	}
	// Use a 3-byte rune so a byte-based truncation (s[:maxEmbedChars]) would
	// slice mid-rune and corrupt UTF-8. The recipe must cut on a rune boundary.
	// "三" is 3 bytes; maxEmbedChars is not a multiple of 3, so a byte cut lands
	// inside a rune.
	long := strings.Repeat("三", maxEmbedChars+100)
	out := EmbedText(long, "")
	if !utf8.ValidString(out) {
		t.Fatalf("truncation split a multi-byte rune: output is not valid UTF-8")
	}
	if n := utf8.RuneCountInString(out); n != maxEmbedChars {
		t.Fatalf("truncation kept %d runes, want exactly %d", n, maxEmbedChars)
	}
}

func TestFingerprintIsStableAndComponentSensitive(t *testing.T) {
	base := Fingerprint("nomic-embed-text", 768, "")
	if len(base) != 64 {
		t.Fatalf("fingerprint length = %d, want 64", len(base))
	}
	if base == Fingerprint("other-model", 768, "") {
		t.Fatal("model change did not alter fingerprint")
	}
	if base == Fingerprint("nomic-embed-text", 1024, "") {
		t.Fatal("dims change did not alter fingerprint")
	}
	if base == Fingerprint("nomic-embed-text", 768, "salt") {
		t.Fatal("salt change did not alter fingerprint")
	}
	if base != Fingerprint("nomic-embed-text", 768, "") {
		t.Fatal("fingerprint not deterministic")
	}
}

// TestFingerprintResistsComponentBoundaryCollisions pins the length-prefixed
// encoding. A naive NUL-separated stream ("v%d\x00%s\x00%d\x00%s" over model,
// dims, salt) lets the boundaries between components slide: distinct tuples can
// serialize to the same byte stream and thus the same hash. The length prefixes
// make every component's extent explicit, so no two distinct tuples collide.
func TestFingerprintResistsComponentBoundaryCollisions(t *testing.T) {
	// Under a NUL-joined encoding both of these produce the identical stream
	// "...x\x001\x00p\x002\x00q": the first reads model="x", dims=1,
	// salt="p\x002\x00q"; the second reads model="x\x001\x00p", dims=2,
	// salt="q". Length prefixing forces a distinct serialization for each.
	a := Fingerprint("x", 1, "p\x002\x00q")
	b := Fingerprint("x\x001\x00p", 2, "q")
	if a == b {
		t.Fatal("NUL-boundary-sliding tuples collided: encoding is not length-prefixed")
	}

	// Components containing the delimiter bytes used by the length-prefixed
	// format ("|", ":") and digits must not let a "shifted" decomposition of
	// the salt collide with a different (model, salt) split.
	c := Fingerprint("m|7", 768, "x")
	d := Fingerprint("m", 768, "7|x")
	if c == d {
		t.Fatal("delimiter-bearing components collided across a shifted split")
	}
	// A digit run that abuts the dims field must not merge with it: model="m"
	// followed by salt="1:24" must differ from model="m1", dims still distinct
	// only by where the boundary falls.
	e := Fingerprint("m", 124, "")
	f := Fingerprint("m1", 24, "")
	if e == f {
		t.Fatal("digit-adjacent model/dims boundary collided")
	}
}
