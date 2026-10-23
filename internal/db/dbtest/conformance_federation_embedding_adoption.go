package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// A spoke keeps the hub's root-owned producer metadata after leaving. Rejoining
// must not turn that inherited value into a spoke-authored metadata update.
func checkSpokeRejoinPreservesHubEmbeddingProducer(t *testing.T, spoke db.Storage, backend Backend) error {
	t.Helper()
	ctx := t.Context()
	hub := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, hub.Close()) })

	hubProject, err := hub.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = hub.EnableProjectFederation(ctx, hubProject.ID, "operator")
	require.NoError(t, err)
	producer := projectProducerTestConfig(hub.InstanceUID())
	producerRaw, err := json.Marshal(producer)
	require.NoError(t, err)
	_, err = hub.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{
		ProjectID: hubProject.ID, Actor: "operator",
		Patch: map[string]jsontext.Value{db.ProjectEmbeddingMetadataKey: jsontext.Value(producerRaw)},
	})
	require.NoError(t, err)

	localProject, err := spoke.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	_, err = spoke.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{
		ProjectID: localProject.ID, Actor: "operator",
		Patch: map[string]jsontext.Value{
			db.ProjectEmbeddingMetadataKey: jsontext.Value(producerRaw),
			"spoke_note":                   jsontext.Value(`"retained"`),
		},
	})
	require.NoError(t, err)
	adoption := db.AdoptProjectIntoFederationParams{
		ProjectID: localProject.ID, HubURL: "https://daemon.example",
		HubProjectID: hubProject.ID, HubProjectUID: hubProject.UID,
		ReplayHorizonEventID: 1, Actor: "operator",
	}
	_, err = spoke.AdoptProjectIntoFederation(ctx, adoption)
	require.NoError(t, err)
	_, err = spoke.LeaveFederationReplica(ctx, localProject.ID)
	require.NoError(t, err)
	detached, err := spoke.ProjectByID(ctx, localProject.ID)
	require.NoError(t, err)
	detachedProducer, err := db.ProjectEmbeddingProducerFromMetadata(detached.Metadata)
	require.NoError(t, err)
	require.Equal(t, &producer, detachedProducer, "leaving retains the hub's producer selection")

	enrollment, err := hub.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		SpokeInstanceUID: spoke.InstanceUID(), ProjectID: &hubProject.ID,
		Actor: "operator", Capabilities: "pull,push",
	})
	require.NoError(t, err)
	rejoined, err := spoke.AdoptProjectIntoFederation(ctx, adoption)
	require.NoError(t, err)
	localEvents, err := spoke.PendingFederationPushEvents(ctx, localProject.ID, spoke.InstanceUID(), rejoined.Binding.PushCursorEventID, 100)
	require.NoError(t, err)
	require.Len(t, localEvents, 1, "re-adoption creates one project metadata baseline event")
	require.Equal(t, "project.metadata_updated", localEvents[0].Type)

	batch := []db.FederationIngestEvent{{
		SourceEventID: localEvents[0].ID,
		Event:         portableNativeEvent(localEvents[0]),
	}}
	_, err = hub.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID: hubProject.ID, FederationEnrollmentID: enrollment.Enrollment.ID,
		SpokeInstanceUID: spoke.InstanceUID(), BoundActor: "operator", Events: batch,
	})
	require.NoError(t, err, "the hub must accept the rejoined spoke's metadata baseline")
	updatedHubProject, err := hub.ProjectByID(ctx, hubProject.ID)
	require.NoError(t, err)
	upstreamProducer, err := db.ProjectEmbeddingProducerFromMetadata(updatedHubProject.Metadata)
	require.NoError(t, err)
	require.Equal(t, &producer, upstreamProducer, "the spoke baseline must leave the hub-owned producer unchanged")
	var updatedMetadata map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(updatedHubProject.Metadata), &updatedMetadata))
	require.Equal(t, jsontext.Value(`"retained"`), updatedMetadata["spoke_note"], "ordinary spoke metadata must remain in the baseline")
	return nil
}
