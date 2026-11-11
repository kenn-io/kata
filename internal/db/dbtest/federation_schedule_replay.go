package dbtest

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/uid"
)

// RunFederationScheduleReplay checks concurrent edits retain one whole schedule.
func RunFederationScheduleReplay(t *testing.T, open func(*testing.T) db.Storage) {
	t.Helper()
	store := open(t)
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, project.ID, "operator")
	require.NoError(t, err)
	spoke, err := uid.New()
	require.NoError(t, err)
	issue, err := uid.New()
	require.NoError(t, err)
	base := `{"value":"2026-01-01","timezone":"UTC","tzdata_version":"` + metadata.ScheduleTZDataVersion + `"}`
	first := `{"value":"2026-01-02","timezone":"Europe/Paris","tzdata_version":"` + metadata.ScheduleTZDataVersion + `"}`
	second := `{"value":"2026-01-01","timezone":"Asia/Tokyo","tzdata_version":"` + metadata.ScheduleTZDataVersion + `"}`
	inputs := []struct{ kind, payload string }{
		{"issue.created", `{"uid":"` + issue + `","title":"original","author":"worker","status":"open","metadata":{"scheduled_on":` + base + `},"created_at":"2026-05-23T12:00:00.000Z"}`},
		{"issue.metadata_updated", `{"diff":{"scheduled_on":{"from":` + base + `,"to":` + first + `}}}`},
		{"issue.metadata_updated", `{"diff":{"scheduled_on":{"from":` + base + `,"to":` + second + `}}}`},
	}
	events := make([]db.FederationIngestEvent, 0, len(inputs))
	for i, input := range inputs {
		event := newRemoteEvent(t, project, &issue, input.kind, "worker", spoke, int64(300+i), jsontext.Value(input.payload))
		events = append(events, db.FederationIngestEvent{SourceEventID: int64(i + 1), Event: event})
	}
	_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: project.ID, SpokeInstanceUID: spoke, BoundActor: "worker", EventFeatures: db.SupportedEventFeatures, Events: events})
	require.NoError(t, err)
	stored, err := store.IssueByUID(ctx, issue, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.JSONEq(t, `{"scheduled_on":`+second+`}`, string(stored.Metadata))
}
