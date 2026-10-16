package daemon_test

import (
	"encoding/json"
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

func TestClaim_TimedAssignmentHistoricalEnrollmentScope(t *testing.T) {
	for _, refKind := range []string{"alias", "full UID"} {
		for _, bothProjects := range []bool{true, false} {
			name := refKind + "/source only"
			if bothProjects {
				name = refKind + "/both projects"
			}
			t.Run(name, func(t *testing.T) {
				env := testenv.New(t, testenv.WithAuthToken("owner-token"))
				source, target, issue := moveLocalIssueForLeaseTests(t, env)
				var projectID *int64
				if !bothProjects {
					projectID = &source.ID
				}
				enrollment, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{Token: "historical-assignment-enrollment", SpokeInstanceUID: federationTestSpokeUID, ProjectID: projectID, Capabilities: "claim", Actor: "tester"})
				require.NoError(t, err)
				ref := issue.ShortID
				if refKind == "full UID" {
					ref = issue.UID
				}
				response, raw := envDoRaw(t, env, http.MethodPost, issuePathRef(source.ID, ref, "actions/claim"), map[string]any{"actor": "body-actor", "ttl_seconds": 300}, bearer(enrollment.Token))
				if bothProjects {
					require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
					var out struct {
						Issue   db.Issue `json:"issue"`
						Changed bool     `json:"changed"`
					}
					require.NoError(t, json.Unmarshal(raw, &out))
					assert.True(t, out.Changed)
					assert.Equal(t, target.ID, out.Issue.ProjectID)
					require.NotNil(t, out.Issue.Owner)
					assert.Equal(t, "tester", *out.Issue.Owner)
					assert.NotNil(t, out.Issue.AssignmentExpiresOn)
				} else {
					assertAPIError(t, response.StatusCode, raw, http.StatusForbidden, "auth_invalid")
				}
				current, err := env.DB.IssueByID(t.Context(), issue.ID)
				require.NoError(t, err)
				if bothProjects {
					require.NotNil(t, current.Owner)
					assert.Equal(t, "tester", *current.Owner)
					assert.NotNil(t, current.AssignmentExpiresOn)
				} else {
					assert.Nil(t, current.Owner)
					assert.Nil(t, current.AssignmentExpiresOn)
				}
			})
		}
	}
}
