package daemon_test

import (
	"encoding/json/jsontext"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// An issue can move while local and then join federation in its new project.
// A timed assignment through the historical route must reach that new hub.
func TestClaim_TimedAssignmentHistoricalRouteUsesCurrentFederation(t *testing.T) {
	hub := testenv.New(t)
	hubProject, hubIssue := createClaimHubIssue(t, hub)
	spoke := testenv.New(t)
	source, err := spoke.DB.CreateProject(t.Context(), "original-project")
	require.NoError(t, err)
	target, err := spoke.DB.CreateProjectWithUID(t.Context(), hubProject.Name, hubProject.UID)
	require.NoError(t, err)
	issue, _, err := spoke.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, UID: hubIssue.UID, Title: "Moved task", Author: "tester"})
	require.NoError(t, err)
	_, err = spoke.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "tester"})
	require.NoError(t, err)
	_, err = spoke.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: target.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: hubProject.ID, HubProjectUID: hubProject.UID, ReplayHorizonEventID: 1, Actor: "tester", Enabled: true, PushEnabled: true})
	require.NoError(t, err)
	// Joining the target pulls its canonical creation baseline before later
	// assignment events can rebuild the replica's read model.
	createdEvents, err := hub.DB.EventsAfter(t.Context(), db.EventsAfterParams{
		ProjectID: hubProject.ID, IssueUID: issue.UID, Types: []string{"issue.created"}, Limit: 1,
	})
	require.NoError(t, err)
	require.Len(t, createdEvents, 1)
	created := createdEvents[0]
	_, err = spoke.DB.InsertRemoteEvent(t.Context(), target.ID, db.RemoteEvent{
		EventUID: created.UID, OriginInstanceUID: created.OriginInstanceUID,
		ProjectUID: created.ProjectUID, ProjectName: created.ProjectName,
		IssueUID: created.IssueUID, Type: created.Type, Actor: created.Actor,
		HLCPhysicalMS: created.HLCPhysicalMS, HLCCounter: created.HLCCounter,
		ContentHash: created.ContentHash, Payload: jsontext.Value(created.Payload), CreatedAt: created.CreatedAt,
	})
	require.NoError(t, err)
	enrollment := createClaimEnrollment(t, hub, hubProject.ID, spoke.DB.InstanceUID(), "claim")
	require.NoError(t, config.WriteFederationCredential(target.UID, config.FederationCredential{HubURL: hub.URL, HubProjectID: hubProject.ID, Token: enrollment.Token, Capabilities: "claim"}))
	response, raw := envDoRaw(t, spoke, http.MethodPost, issuePathRef(source.ID, issue.ShortID, "actions/claim"), map[string]any{"actor": "tester", "ttl_seconds": 300}, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
	current, err := spoke.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	authoritative, err := hub.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.NotNil(t, current.Owner)
	require.NotNil(t, authoritative.Owner)
	assert.Equal(t, "tester", *current.Owner)
	assert.Equal(t, "tester", *authoritative.Owner)
	require.NotNil(t, current.AssignmentExpiresOn)
	assert.Equal(t, current.AssignmentExpiresOn, authoritative.AssignmentExpiresOn)
	assert.Equal(t, target.ID, current.ProjectID)
}
