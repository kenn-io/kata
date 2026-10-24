package definition

import (
	"strings"
	"testing"
)

func TestPortableKeyNativeBoundary(t *testing.T) {
	for _, n := range []int{255, 256, 257} {
		if got := portableKey(strings.Repeat("a", n)); got != (n <= 256) {
			t.Fatalf("%d-byte native key accepted=%v", n, got)
		}
	}
}
