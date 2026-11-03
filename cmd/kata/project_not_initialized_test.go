package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

// TestProjectNotInitializedErrorData verifies the project_not_initialized
// CLI envelope preserves the daemon's error.data and not-found exit code.
func TestProjectNotInitializedErrorData(t *testing.T) {
	env := testenv.New(t)
	stdout, stderr, err := executeRootCapture(t, contextWithBaseURL(t.Context(), env.URL), "--workspace", t.TempDir(), "--project", "nope", "list", "--format", "json")
	require.Error(t, err)
	assert.Empty(t, stdout)

	got := parseErrorEnvelope(t, []byte(stderr))
	assert.Equal(t, "project_not_initialized", got.Error.Code)
	assert.Equal(t, ExitNotFound, got.Error.ExitCode)
	var data map[string]any
	require.NoError(t, json.Unmarshal(got.Error.Data, &data), stderr)
	assert.Equal(t, map[string]any{"project": "nope"}, data)
}
