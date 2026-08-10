package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSafeUILaunchURLRejectsUnsafeOrigins(t *testing.T) {
	for _, origin := range []string{
		"ftp://kata.example",
		"https://user@kata.example",
		"https://kata.example/prefix",
		"https://kata.example?tenant=alpha",
		"https://kata.example#fragment",
	} {
		t.Run(origin, func(t *testing.T) {
			target, ok := safeUILaunchURL(&WebSessionManager{origin: origin}, "01J00000000000000000000001")
			require.False(t, ok)
			require.Empty(t, target)
		})
	}
}
