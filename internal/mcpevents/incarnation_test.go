package mcpevents

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGeneratedIncarnations(t *testing.T) {
	seen := map[string]bool{}
	for range 16 {
		incarnation, err := newIncarnation()
		require.NoError(t, err)
		assert.Regexp(t, `^[0-9a-f]{32}$`, incarnation, "128 random bits as lowercase hex")
		assert.False(t, seen[incarnation])
		seen[incarnation] = true

	}
}
