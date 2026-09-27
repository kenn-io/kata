package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexHookIndexesShifted_PreservesNumericIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, first, second string
	}{
		{"large adjacent integers", "9007199254740992", "9007199254740993"},
		{"numeric literal scale", "10", "10.0"},
		{"numeric literal exponent", "1000", "1e3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hooks.json")
			writeCodexFixture(t, path, `{"hooks":{"SessionStart":[{"hooks":[{"command":"echo example","value":`+tc.first+`},{"command":"echo example","value":`+tc.second+`}]}]}}`)
			before, err := readCodexHookConfig(path)
			require.NoError(t, err)
			writeCodexFixture(t, path, `{"hooks":{"SessionStart":[{"hooks":[{"command":"echo example","value":`+tc.second+`}]}]}}`)
			after, err := readCodexHookConfig(path)
			require.NoError(t, err)

			shifted, err := codexHookIndexesShifted(before, after)
			require.NoError(t, err)
			assert.True(t, shifted, "distinct numeric handlers must not collapse: the survivor moved from index 1 to 0")
		})
	}
}

func TestReadCodexHookConfig_RejectsTrailingPayload(t *testing.T) {
	for _, data := range []string{`{} {}`, `{} true`, `{} trailing`} {
		t.Run(data, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hooks.json")
			writeCodexFixture(t, path, data)
			parsed, err := readCodexHookConfig(path)
			require.Error(t, err)
			assert.Nil(t, parsed)
		})
	}
}
