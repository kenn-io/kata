package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtraJSONRendererFallsBackWhenRawArrayChildrenAreUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
		old  []any
	}{
		{"absent", nil, []any{"keep"}},
		{"empty", []byte(`[]`), []any{"keep"}},
		{"non-array", []byte(`null`), []any{"keep"}},
		{"shorter", []byte(`["other"]`), []any{"other", "keep"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rendered []byte
			var err error
			require.NotPanics(t, func() {
				rendered, err = renderExtraJSONValue(tc.raw, tc.old, []any{"keep", "new"})
			})
			require.NoError(t, err)
			require.JSONEq(t, `["keep","new"]`, string(rendered))
		})
	}
}

func TestExtraTOMLAbsentNeutralPlanKeepsNilPreimages(t *testing.T) {
	opts := extraOptions(t, "kimi-code")
	opts.ConfigPath = filepath.Join(opts.Home, "absent.toml")
	opts.Contract, opts.Attention = false, false
	blocks, err := parseExtraTOMLHooks(nil)
	require.NoError(t, err)
	require.Nil(t, blocks, "no hook blocks retains the parser's nil result")
	plan, err := planExtraAgentHooks(opts, true)
	require.NoError(t, err)
	require.Len(t, plan.Changes, 1)
	require.Nil(t, plan.Changes[0].Original)
	require.False(t, plan.Changes[0].OriginalExists)
	require.Nil(t, plan.Changes[0].Content)
	changed, err := publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	require.False(t, changed)
}
