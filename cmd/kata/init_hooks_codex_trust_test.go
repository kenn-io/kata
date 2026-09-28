package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyCodexHooks_DedupeTrustAndPreservation(t *testing.T) {
	attention := map[string]any{"matcher": codexSessionStartMatcher, "hooks": []any{expectedCodexHandler()}}
	contract := map[string]any{"matcher": codexContractSessionStartMatcher, "hooks": []any{expectedCodexContractHandler()}}
	foreignHandler := map[string]any{"command": "echo example", "type": "command", "timeout": json.Number("13")}
	foreign := map[string]any{"matcher": "resume", "note": "retain group metadata", "hooks": []any{foreignHandler}}
	mixed := func(handlers ...any) map[string]any {
		return map[string]any{"matcher": codexSessionStartMatcher, "note": "keep mixed group", "hooks": handlers}
	}
	for _, tc := range []struct {
		name          string
		before, after []any
		shifted       bool
	}{
		{"contract last", []any{foreign, attention, contract}, []any{foreign, attention}, false},
		{"contract first", []any{contract, attention, foreign}, []any{attention, foreign}, true},
		{"foreign after contract", []any{attention, contract, foreign}, []any{attention, foreign}, true},
		{"foreign before between after", []any{foreign, attention, foreign, contract, foreign}, []any{foreign, attention, foreign, foreign}, true},
		{"repeated foreign before contract", []any{foreign, foreign, attention, contract}, []any{foreign, foreign, attention}, false},
		{"mixed contract last", []any{mixed(foreignHandler, expectedCodexHandler(), expectedCodexContractHandler())}, []any{mixed(foreignHandler, expectedCodexHandler())}, false},
		{"mixed contract first", []any{mixed(expectedCodexContractHandler(), foreignHandler, expectedCodexHandler())}, []any{mixed(foreignHandler, expectedCodexHandler())}, true},
		{"mixed repeated handlers", []any{attention, mixed(foreignHandler, expectedCodexContractHandler(), foreignHandler)}, []any{attention, mixed(foreignHandler, foreignHandler)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			userPath := userCodexContractFixture(t)
			dir := t.TempDir()
			path := filepath.Join(dir, ".codex", "hooks.json")
			before := map[string]any{"retain": "root setting", "hooks": map[string]any{"SessionStart": tc.before, "Stop": []any{foreign}}}
			data, err := json.Marshal(before)
			require.NoError(t, err)
			writeCodexFixture(t, path, string(data))
			changed, notes, err := applyCodexHooks(dir)
			require.NoError(t, err)
			assert.True(t, changed)
			assert.Equal(t, map[string]any{"retain": "root setting", "hooks": map[string]any{"SessionStart": tc.after, "Stop": []any{foreign}}}, readCodexHooks(t, dir))
			wantNotes := []string{"removed workspace contract hook: " + userPath + " already injects it"}
			if tc.shifted {
				wantNotes = append(wantNotes, "Codex will ask to re-trust shifted hooks; open Codex and run /hooks.")
			}
			assert.Equal(t, wantNotes, notes)

			first, err := os.ReadFile(path) //nolint:gosec // test-owned config under TempDir
			require.NoError(t, err)
			changed, notes, err = applyCodexHooks(dir)
			require.NoError(t, err)
			assert.False(t, changed, "second dedupe must be a no-op")
			assert.Empty(t, notes)
			second, err := os.ReadFile(path) //nolint:gosec // test-owned config under TempDir
			require.NoError(t, err)
			assert.Equal(t, first, second, "second init must preserve bytes and trust indexes")
		})
	}
}

func TestApplyCodexHooks_RepairsNoncanonicalAttention(t *testing.T) {
	for _, kind := range []string{"wrong timeout", "missing Windows command", "duplicate", "wrong matcher"} {
		t.Run(kind, func(t *testing.T) {
			userCodexContractFixture(t)
			dir := t.TempDir()
			handler := expectedCodexHandler()
			matcher := codexSessionStartMatcher
			switch kind {
			case "wrong timeout":
				handler["timeout"] = json.Number("2")
			case "missing Windows command":
				delete(handler, "commandWindows")
			case "wrong matcher":
				matcher = "compact"
			}
			groups := []any{map[string]any{"matcher": matcher, "hooks": []any{handler}}}
			if kind == "duplicate" {
				groups = append(groups, map[string]any{"matcher": matcher, "hooks": []any{handler}})
			}
			data, err := json.Marshal(map[string]any{"hooks": map[string]any{"SessionStart": groups}})
			require.NoError(t, err)
			writeCodexFixture(t, filepath.Join(dir, ".codex", "hooks.json"), string(data))
			changed, _, err := applyCodexHooks(dir)
			require.NoError(t, err)
			assert.True(t, changed)
			assert.Equal(t, []any{map[string]any{"matcher": codexSessionStartMatcher, "hooks": []any{expectedCodexHandler()}}}, readCodexHooks(t, dir)["hooks"].(map[string]any)["SessionStart"])
		})
	}
}

func TestApplyCodexHooks_PreservesWorkspaceAttentionPositions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tracked bool
	}{
		{"no user contract", false},
		{"tracked with user contract", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CODEX_HOME", t.TempDir())
			groups := []any{
				map[string]any{"matcher": codexSessionStartMatcher, "hooks": []any{
					expectedCodexHandler(), map[string]any{"type": "command", "command": "echo example"},
				}},
				map[string]any{"matcher": codexContractSessionStartMatcher, "hooks": []any{expectedCodexContractHandler()}},
			}
			data, err := json.Marshal(map[string]any{"hooks": map[string]any{"SessionStart": groups}})
			require.NoError(t, err)
			writeCodexFixture(t, filepath.Join(dir, ".codex", "hooks.json"), string(data))
			if tc.tracked {
				runGit(t, dir, "init", "--quiet")
				runGit(t, dir, "add", "--", ".codex/hooks.json")
				userCodexContractFixture(t)
			}

			changed, _, err := applyCodexHooks(dir)
			require.NoError(t, err)
			assert.False(t, changed)
			assert.Equal(t, groups, readCodexHooks(t, dir)["hooks"].(map[string]any)["SessionStart"],
				"unchanged attention and foreign hooks must keep their group and handler positions")
		})
	}
}
