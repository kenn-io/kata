package daemon_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

// Reserved project-control keys are invalid generic metadata writes. Report a
// client error with the typed-API instruction and leave the metadata unchanged.
func TestCronProjectPauseReservedMetadataHTTP(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "example-project")
	for _, key := range []string{"cron.pause", "cron.request.01ARZ3NDEKTSV4RRFFQ69G5FAV"} {
		status, raw := cronHTTP(t, env, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/metadata", project.ID), map[string]any{"actor": "operator", "patch": map[string]any{key: true}})
		require.Equalf(t, 400, status, "%s", raw)
		require.Contains(t, string(raw), "typed cron control API")
	}
	current, err := env.DB.ProjectByID(t.Context(), project.ID)
	require.NoError(t, err)
	require.Equal(t, project.Metadata, current.Metadata)
}
