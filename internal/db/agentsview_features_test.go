package db_test

import (
	"database/sql"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/agentsviewrelay"
	"go.kenn.io/kata/internal/db"
)

func TestAgentsViewAssociationRequiresPeerFeature(t *testing.T) {
	project, root := "01ARZ3NDEKTSV4RRFFQ69G5FAW", "01ARZ3NDEKTSV4RRFFQ69G5FAX"
	a := agentsviewrelay.Association{Version: 1, IssueUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", ProjectUID: project, AuthorityUID: root, ServiceID: strings.Repeat("a", 64), RootKeyID: strings.Repeat("c", 64), Agent: "pi", NativeSessionID: "selected", SessionID: "pi:selected", SnapshotSHA256: strings.Repeat("b", 64), SnapshotBytes: 12, LocatorBase: "https://archive.example", Route: []agentsviewrelay.RouteObservation{{InstanceUID: root, ProjectUID: project}}}
	b, err := json.Marshal(struct {
		Association agentsviewrelay.Association `json:"agentsview_association"`
	}{a})
	require.NoError(t, err)
	feature, err := db.PayloadRequiredFeatures("issue.closed", string(b))
	require.NoError(t, err)
	require.Equal(t, agentsviewrelay.EventFeature, feature)
	require.ErrorIs(t, db.RequireEventFeatures("cron_v1,issue_move_v1", feature), db.ErrUnsupportedEventFeatures)
	require.NoError(t, db.RequireEventFeatures(db.SupportedEventFeatures, feature))
	legacy, err := db.PayloadRequiredFeatures("issue.closed", `{"reason":"done"}`)
	require.NoError(t, err)
	require.Empty(t, legacy)
}

func TestProjectFeaturesRecognizeEscapedAssociationKey(t *testing.T) {
	project, root := "01ARZ3NDEKTSV4RRFFQ69G5FAW", "01ARZ3NDEKTSV4RRFFQ69G5FAX"
	association := agentsviewrelay.Association{Version: 1, IssueUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", ProjectUID: project, AuthorityUID: root, ServiceID: strings.Repeat("a", 64), RootKeyID: strings.Repeat("c", 64), Agent: "pi", NativeSessionID: "selected", SessionID: "pi:selected", SnapshotSHA256: strings.Repeat("b", 64), SnapshotBytes: 12, LocatorBase: "https://archive.example", Route: []agentsviewrelay.RouteObservation{{InstanceUID: root, ProjectUID: project}}}
	raw, err := json.Marshal(association)
	require.NoError(t, err)
	payload := `{"agentsview_\u0061ssociation":` + string(raw) + `}`
	database := sql.OpenDB(featureProbeConnector{state: &featureProbeState{noCron: true, closePayloads: []string{payload}}})
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	features, err := db.ProjectRequiredEventFeatures(t.Context(), database, project, false)
	require.NoError(t, err)
	require.Equal(t, agentsviewrelay.EventFeature, features)
}
