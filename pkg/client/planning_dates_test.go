package client_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestPlanningDatesTypedClientPreservesRequiredNulls(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	api, err := client.NewWithHTTPClient(env.URL, env.HTTP)
	require.NoError(t, err)
	for _, present := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "present"}[present], func(t *testing.T) {
			metadata := map[string]jsontext.Value{}
			if present {
				metadata["scheduled_on"] = []byte(`"2026-10-06T09:00:00Z"`)
			}
			issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Review", Author: "worker", Metadata: metadata})
			require.NoError(t, err)
			response, err := api.IssuePlanningDatesWithResponse(t.Context(), &generated.IssuePlanningDatesRequestOptions{PathParams: &generated.IssuePlanningDatesPath{ProjectID: project.ID, Ref: issue.UID}})
			require.NoError(t, err)
			require.NotNil(t, response.JSON200)
			var raw map[string]any
			require.NoError(t, json.Unmarshal(response.Body, &raw))
			require.Contains(t, raw, "scheduled_on")
			require.Contains(t, raw, "deadline_on")
			require.Nil(t, raw["deadline_on"])
			encoded, err := json.Marshal(response.JSON200)
			require.NoError(t, err)
			var roundTrip map[string]any
			require.NoError(t, json.Unmarshal(encoded, &roundTrip))
			require.Equal(t, raw, roundTrip, "typed decoding must preserve required nulls and resolved date values")
			require.NoError(t, response.JSON200.Validate())
		})
	}
}
