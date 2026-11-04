package main

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronCLIValidateCollisionsQuiet(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	body := cronCLIJob(t)
	body.Definition.Trigger = cron.Trigger{Kind: "cron", Cron: "0 9 * * *"}
	peerDefinition := body.Definition.Native()
	peerDefinition.Enabled = true
	peer, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Daily peer", Definition: peerDefinition, Actor: "operator"})
	require.NoError(t, err)
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	out, _, err := executeCronCLIWithoutJSON(t, env.URL, nil, "job", "validate", "--file", cronCLIFile(t, body), "--quiet", "--count", "1", "--from", "2026-11-03T00:00:00Z")
	require.NoError(t, err, out)
	var result db.CronValidation
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	require.True(t, result.Valid)
	require.Len(t, result.Schedule.Next, 1)
	require.Len(t, result.Schedule.Collisions.Items, 1)
	require.Equal(t, 7, result.Schedule.Collisions.Items[0].Occurrences)
	require.Equal(t, []cron.CollisionPeer{{UID: peer.UID, Name: peer.Name}}, result.Schedule.Collisions.Items[0].Peers)
	require.Equal(t, "paused", result.Warnings[0].Code)
	after, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
}
