package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeCodexFixture(t *testing.T, path, data string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
}

func userCodexContractFixture(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	path := filepath.Join(home, "hooks.json")
	writeCodexFixture(t, path, `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"kata agent-contract-hook --source kata-agent-contract-hook"}]}]}}`)
	return path
}

func TestApplyCodexHooks_DeduplicatesUserContract(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", t.TempDir())
	_, _, err := applyCodexHooks(dir)
	require.NoError(t, err)
	userPath := userCodexContractFixture(t)
	userBefore, err := os.ReadFile(userPath) //nolint:gosec // test-owned config under TempDir
	require.NoError(t, err)

	changed, notes, err := applyCodexHooks(dir)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, []string{"removed workspace contract hook: " + userPath + " already injects it"}, notes)
	assert.Equal(t, map[string]any{"hooks": map[string]any{"SessionStart": []any{
		map[string]any{"matcher": codexSessionStartMatcher, "hooks": []any{expectedCodexHandler()}},
	}}}, readCodexHooks(t, dir))
	userAfter, err := os.ReadFile(userPath) //nolint:gosec // test-owned config under TempDir
	require.NoError(t, err)
	assert.Equal(t, userBefore, userAfter)
}

func TestApplyCodexHooks_KeepsTrackedWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name     string
		data     string
		contract bool
	}{
		{"legacy attention and contract", "{\n  \"hooks\": {\"SessionStart\": [{\"hooks\": [{\"type\": \"command\", \"command\": \"kata attention-hook start\", \"timeout\": 10}]}, {\"hooks\": [{\"command\": \"kata agent-contract-hook --source kata-agent-contract-hook\"}]}]}\n}\n", true},
		{"attention only", `{"hooks":{"SessionStart":[{"hooks":[{"command":"kata attention-hook start"}]}]}}`, false},
		{"foreign only", ` { "hooks": { "SessionStart": [{"hooks":[{"command":"echo example"}]}] } } `, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			runGit(t, dir, "init", "--quiet")
			path := filepath.Join(dir, ".codex", "hooks.json")
			writeCodexFixture(t, path, tc.data)
			runGit(t, dir, "add", "--", ".codex/hooks.json")
			userPath := userCodexContractFixture(t)
			changed, notes, err := applyCodexHooks(dir)
			require.NoError(t, err)
			assert.False(t, changed)
			after, err := os.ReadFile(path) //nolint:gosec // test-owned config under TempDir
			require.NoError(t, err)
			assert.Equal(t, tc.data, string(after), "tracked file must return before any serialization or migration")
			if tc.contract {
				assert.Equal(t, []string{"kept workspace contract hook: .codex/hooks.json is tracked; " + userPath + " also injects it"}, notes)
			} else {
				assert.Empty(t, notes)
			}
		})
	}
}

func TestApplyCodexHooks_TrackedWorkspaceWarnsOnTomlHooks(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		contract   bool
	}{
		{"contract present", ` { "hooks": {"SessionStart": [{"hooks":[{"command":"kata agent-contract-hook --source kata-agent-contract-hook"}]},{"matcher":"resume","hooks":[{"command":"echo example"}]},{"hooks":[{"command":"kata attention-hook start","timeout":10}]}]} } `, true},
		{"no contract", ` { "hooks": {"SessionStart": [{"matcher":"resume","hooks":[{"command":"echo example"}]},{"hooks":[{"command":"kata attention-hook start","timeout":10}]}]} } `, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			runGit(t, dir, "init", "--quiet")
			path := filepath.Join(dir, ".codex", "hooks.json")
			writeCodexFixture(t, path, tc.data)
			runGit(t, dir, "add", "--", ".codex/hooks.json")
			tomlPath := filepath.Join(dir, ".codex", "config.toml")
			writeCodexFixture(t, tomlPath, "[hooks]\n")
			before := readCodexHooks(t, dir)
			userPath := userCodexContractFixture(t)

			changed, notes, err := applyCodexHooks(dir)
			require.NoError(t, err)
			assert.False(t, changed)
			after, err := os.ReadFile(path) //nolint:gosec // test-owned config under TempDir
			require.NoError(t, err)
			assert.Equal(t, tc.data, string(after))
			assert.Equal(t, before, readCodexHooks(t, dir), "all group and handler positions remain untouched")
			want := []string{tomlPath + " already defines a [hooks] table; Codex loads it together with " + path}
			if tc.contract {
				want = append(want, "kept workspace contract hook: .codex/hooks.json is tracked; "+userPath+" also injects it")
			}
			assert.Equal(t, want, notes)
		})
	}
}

func TestApplyCodexHooks_UserContractDetection(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		wantGroups int
	}{
		{"missing", "", 2},
		{"legacy", `{"hooks":{"SessionStart":[{"hooks":[{"command":"kata agent-contract-hook --source kata-agent-contract-hook"}]}]}}`, 1},
		{"visible", `{"hooks":{"SessionStart":[{"hooks":[{"command":"/opt/bin/kata agent-hooks contract codex --source kata-agent-contract-hook"}]}]}}`, 1},
		{"Windows command", `{"hooks":{"SessionStart":[{"hooks":[{"commandWindows":"kata agent-contract-hook --source kata-agent-contract-hook"}]}]}}`, 1},
		{"unmarked", `{"hooks":{"SessionStart":[{"hooks":[{"command":"kata agent-contract-hook"}]}]}}`, 2},
		{"other event", `{"hooks":{"Stop":[{"hooks":[{"command":"kata agent-contract-hook --source kata-agent-contract-hook"}]}]}}`, 2},
		{"empty foreign group", `{"hooks":{"SessionStart":[{"hooks":[]}]}}`, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CODEX_HOME", home)
			path := filepath.Join(home, "hooks.json")
			if tc.data != "" {
				writeCodexFixture(t, path, tc.data)
			}
			dir := t.TempDir()
			_, notes, err := applyCodexHooks(dir)
			require.NoError(t, err)
			assert.Empty(t, notes, "fresh workspace has no duplicate to remove")
			groups := readCodexHooks(t, dir)["hooks"].(map[string]any)["SessionStart"].([]any)
			assert.Len(t, groups, tc.wantGroups)
			if tc.data != "" {
				after, err := os.ReadFile(path) //nolint:gosec // test-owned config under TempDir
				require.NoError(t, err)
				assert.Equal(t, tc.data, string(after))
			}
		})
	}
}

func TestApplyCodexHooks_UserConfigError(t *testing.T) {
	for _, data := range []string{`{"hooks":`, `{"hooks":{"SessionStart":7}}`} {
		t.Run(data, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, ".codex", "hooks.json")
			before := ` { "hooks": {} } `
			writeCodexFixture(t, path, before)
			home := t.TempDir()
			t.Setenv("CODEX_HOME", home)
			writeCodexFixture(t, filepath.Join(home, "hooks.json"), data)
			_, _, err := applyCodexHooks(dir)
			require.Error(t, err)
			after, err := os.ReadFile(path) //nolint:gosec // test-owned config under TempDir
			require.NoError(t, err)
			assert.Equal(t, before, string(after))
		})
	}
}

func TestApplyCodexHooks_InstallsWorkspaceWithoutUserHome(t *testing.T) {
	dir := t.TempDir()
	for _, key := range []string{"CODEX_HOME", "HOME", "USERPROFILE", "home"} {
		// Setenv registers restoration before removing the platform's home key.
		t.Setenv(key, "")
		require.NoError(t, os.Unsetenv(key))
	}

	changed, notes, err := applyCodexHooks(dir)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Empty(t, notes)
	assert.Equal(t, map[string]any{"hooks": map[string]any{
		"SessionStart": expectedCodexSessionStartGroups(),
	}}, readCodexHooks(t, dir))
}

func TestApplyCodexHooks_SameConfigIsNotDuplicate(t *testing.T) {
	for _, kind := range []string{"direct", "symlink", "hard link"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CODEX_HOME", t.TempDir())
			_, _, err := applyCodexHooks(dir)
			require.NoError(t, err)
			workspacePath := filepath.Join(dir, ".codex", "hooks.json")
			userHome := filepath.Join(dir, ".codex")
			if kind != "direct" {
				userHome = t.TempDir()
				link := os.Symlink
				if kind == "hard link" {
					link = os.Link
				}
				require.NoError(t, link(workspacePath, filepath.Join(userHome, "hooks.json")))
			}
			t.Setenv("CODEX_HOME", userHome)
			before, err := os.ReadFile(workspacePath) //nolint:gosec // test-owned config under TempDir
			require.NoError(t, err)
			_, notes, err := applyCodexHooks(dir)
			require.NoError(t, err)
			assert.Empty(t, notes)
			after, err := os.ReadFile(workspacePath) //nolint:gosec // test-owned config under TempDir
			require.NoError(t, err)
			assert.Equal(t, before, after)
			assert.Len(t, readCodexHooks(t, dir)["hooks"].(map[string]any)["SessionStart"], 2)
		})
	}
}

func TestApplyCodexHooks_SelectedHomeOnly(t *testing.T) {
	other := userCodexContractFixture(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	dir := t.TempDir()
	_, _, err := applyCodexHooks(dir)
	require.NoError(t, err)
	assert.Len(t, readCodexHooks(t, dir)["hooks"].(map[string]any)["SessionStart"], 2)
	var user map[string]any
	data, err := os.ReadFile(other) //nolint:gosec // test-owned config under TempDir
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &user))
	assert.Len(t, user["hooks"].(map[string]any)["SessionStart"], 1)
}

func TestApplyCodexHooks_KeepsTrackedNestedAndLinkedWorkspace(t *testing.T) {
	for _, kind := range []string{"nested", "linked"} {
		t.Run(kind, func(t *testing.T) {
			repo := t.TempDir()
			runGit(t, repo, "init", "--quiet")
			dir := repo
			if kind == "nested" {
				dir = filepath.Join(repo, "example-workspace")
			}
			data := ` { "hooks": {"SessionStart": [{"hooks": [{"command":"kata agent-contract-hook --source kata-agent-contract-hook"}]}]} } `
			writeCodexFixture(t, filepath.Join(dir, ".codex", "hooks.json"), data)
			runGit(t, repo, "add", "--all")
			if kind == "linked" {
				runGit(t, repo, "-c", "user.name=Example Actor", "-c", "user.email=actor@example.invalid", "commit", "--quiet", "-m", "fixture")
				dir = filepath.Join(t.TempDir(), "linked-workspace")
				runGit(t, repo, "worktree", "add", "--quiet", "-b", "example-branch", dir)
			}
			userPath := userCodexContractFixture(t)
			changed, notes, err := applyCodexHooks(dir)
			require.NoError(t, err)
			assert.False(t, changed)
			after, err := os.ReadFile(filepath.Join(dir, ".codex", "hooks.json")) //nolint:gosec // test-owned config under TempDir
			require.NoError(t, err)
			assert.Equal(t, data, string(after))
			assert.Equal(t, []string{"kept workspace contract hook: .codex/hooks.json is tracked; " + userPath + " also injects it"}, notes)
		})
	}
}
