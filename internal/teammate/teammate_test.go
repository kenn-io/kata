package teammate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidate(t *testing.T) {
	for _, value := range []string{"", "reviewer-7", "A.b_3", strings.Repeat("a", 64)} {
		require.NoError(t, Validate(value))
	}
	for _, value := range []string{" reviewer-7", "reviewer/7", "@reviewer", "a\nb", "é", strings.Repeat("a", 65)} {
		require.Error(t, Validate(value))
	}
}

func TestResolveUsesExplicitOverrideBeforeFallback(t *testing.T) {
	override := "reviewer-7"
	got, err := Resolve(&override, "teammate-1")
	require.NoError(t, err)
	assert.Equal(t, "reviewer-7", got)

	empty := ""
	got, err = Resolve(&empty, "teammate-1")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestResolveValidatesSelectedValue(t *testing.T) {
	_, err := Resolve(nil, "@reviewer")
	require.Error(t, err)

	override := "reviewer/7"
	_, err = Resolve(&override, "teammate-1")
	require.Error(t, err)
}

func TestStampClonesMetadataAndAddsTeammate(t *testing.T) {
	input := map[string]any{"priority": "high"}
	got, err := Stamp(input, "reviewer-7")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"priority": "high", "teammate": "reviewer-7"}, got)
	assert.Equal(t, map[string]any{"priority": "high"}, input)
	got["priority"] = "low"
	assert.Equal(t, "high", input["priority"])
}

func TestStampPreservesValidExplicitTeammate(t *testing.T) {
	for _, handle := range []string{"", "reviewer-7"} {
		got, err := Stamp(map[string]any{"teammate": "reviewer-7"}, handle)
		require.NoError(t, err)
		assert.Equal(t, "reviewer-7", got["teammate"])
	}
}

func TestStampRejectsInvalidOrConflictingTeammate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]any
		handle   string
	}{
		{name: "invalid handle", handle: "@reviewer"},
		{name: "non-string metadata", metadata: map[string]any{"teammate": 7}},
		{name: "invalid metadata", metadata: map[string]any{"teammate": "reviewer/7"}},
		{name: "conflict", metadata: map[string]any{"teammate": "reviewer-7"}, handle: "implementer-3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Stamp(tc.metadata, tc.handle)
			require.Error(t, err)
		})
	}
}

func TestStampEmptyTeammateReturnsDetachedCopy(t *testing.T) {
	input := map[string]any{"priority": "high"}
	got, err := Stamp(input, "")
	require.NoError(t, err)
	assert.Equal(t, input, got)
	got["priority"] = "low"
	assert.Equal(t, "high", input["priority"])
}
