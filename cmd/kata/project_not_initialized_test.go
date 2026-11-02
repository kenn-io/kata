package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/testfix"
)

// TestProjectNotInitializedErrorData verifies the project_not_initialized
// envelope carries the looked-up alias or project name in error.data, and
// omits data when the message names nothing.
func TestProjectNotInitializedErrorData(t *testing.T) {
	setupKataEnv(t)
	t.Setenv("KATA_SERVER", "")
	env := testenv.New(t)

	gitWorkspace := func(t *testing.T) string {
		dir := t.TempDir()
		runGit(t, dir, "init", "--quiet")
		runGit(t, dir, "remote", "add", "origin", "https://github.com/example/spoke-project.git")
		return dir
	}

	for _, tc := range []struct {
		name     string
		setup    func(t *testing.T) string
		extra    []string
		wantData map[string]any // nil means the data key must be absent
	}{
		{
			name:     "unregistered git alias",
			setup:    gitWorkspace,
			wantData: map[string]any{"alias": "github.com/example/spoke-project"},
		},
		{
			name:     "explicit unregistered project",
			setup:    func(t *testing.T) string { return t.TempDir() },
			extra:    []string{"--project", "nope"},
			wantData: map[string]any{"project": "nope"},
		},
		{
			name: "kata.toml binds unregistered project",
			setup: func(t *testing.T) string {
				dir := gitWorkspace(t)
				testfix.WriteKataToml(t, dir, "ghost-project")
				return dir
			},
			wantData: map[string]any{"project": "ghost-project"},
		},
		{
			name:  "no kata.toml and no git ancestor",
			setup: func(t *testing.T) string { return t.TempDir() },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.setup(t)
			args := append([]string{"--workspace", dir}, tc.extra...)
			args = append(args, "list", "--format", "json")
			stdout, stderr, err := executeRootCapture(t, contextWithBaseURL(t.Context(), env.URL), args...)
			require.Error(t, err)
			assert.Empty(t, stdout)

			var got struct {
				Error map[string]json.RawMessage `json:"error"`
			}
			require.NoError(t, json.Unmarshal([]byte(stderr), &got), stderr)

			var code string
			require.NoError(t, json.Unmarshal(got.Error["code"], &code))
			assert.Equal(t, "project_not_initialized", code)
			var exitCode int
			require.NoError(t, json.Unmarshal(got.Error["exit_code"], &exitCode))
			assert.Equal(t, ExitNotFound, exitCode)

			raw, hasData := got.Error["data"]
			if tc.wantData == nil {
				assert.False(t, hasData, "data must be absent: %s", stderr)
				return
			}
			require.True(t, hasData, stderr)
			var data map[string]any
			require.NoError(t, json.Unmarshal(raw, &data))
			assert.Equal(t, tc.wantData, data)
		})
	}
}
