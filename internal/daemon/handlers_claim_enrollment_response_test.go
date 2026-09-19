package daemon_test

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	katauid "go.kenn.io/kata/internal/uid"
)

// Distinctive private content seeded on the hub copy of a claimed issue. A
// claim-only enrollment response must never carry any of these strings.
const (
	enrollmentSecretTitle     = "CLASSIFIED-title-Ω-7f3k-q9"
	enrollmentSecretBody      = "CLASSIFIED-body-λ-2m8x-z5"
	enrollmentSecretMetaKey   = "vendor"
	enrollmentSecretMetaVal   = "CLASSIFIED-metadata-μ-6n2w-r8"
	enrollmentSecretAuthor    = "CLASSIFIED-author-θ-4p7s-t1"
	enrollmentSecretEditTitle = "CLASSIFIED-edited-title-ξ-8v4k-n2"
	enrollmentSecretEditBody  = "CLASSIFIED-edited-body-δ-3j9h-m6"
)

func enrollmentSecrets() []string {
	return []string{
		enrollmentSecretTitle, enrollmentSecretBody, enrollmentSecretMetaVal,
		enrollmentSecretAuthor, enrollmentSecretEditTitle, enrollmentSecretEditBody,
	}
}

func createPrivateEnrollmentIssue(
	t *testing.T,
	env *testenv.Env,
	projectID int64,
) db.Issue {
	t.Helper()
	issue, _, err := env.DB.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: projectID,
		Title:     enrollmentSecretTitle,
		Body:      enrollmentSecretBody,
		Author:    enrollmentSecretAuthor,
		Metadata: map[string]jsontext.Value{
			enrollmentSecretMetaKey: jsontext.Value(`"` + enrollmentSecretMetaVal + `"`),
		},
	})
	require.NoError(t, err)
	return issue
}

// enrollmentAssignmentSafeEventTypes lists the event types a claim-only
// enrollment response may carry: assignment lifecycle only, never content.
var enrollmentAssignmentSafeEventTypes = map[string]bool{
	"issue.assigned":           true,
	"issue.unassigned":         true,
	"issue.assignment_renewed": true,
	"issue.assignment_expired": true,
}

func createPrivateContentHubProject(t *testing.T, env *testenv.Env, name string) db.Project {
	t.Helper()
	project, err := env.DB.CreateProject(context.Background(), name)
	require.NoError(t, err)
	_, err = env.DB.EnableProjectFederation(context.Background(), project.ID, "tester")
	require.NoError(t, err)
	return project
}

func TestClaim_EnrollmentBearerResponseOmitsPrivateIssueContent(t *testing.T) {
	hub := testenv.New(t)
	hubProject := createPrivateContentHubProject(t, hub, "github.com/test/private-hub")
	issue := createPrivateEnrollmentIssue(t, hub, hubProject.ID)
	spokeUID, err := katauid.New()
	require.NoError(t, err)
	enrollment := createClaimEnrollment(t, hub, hubProject.ID, spokeUID, "claim")

	headers := map[string]string{"Authorization": "Bearer " + enrollment.Token}
	resp, raw := envDoRaw(t, hub, http.MethodPost,
		issuePathRef(hubProject.ID, issue.ShortID, "actions/claim"),
		map[string]any{"ttl_seconds": 300}, headers)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)

	// A claim-only enrollment bearer is authorized to assign, not to read:
	// no private title/body/metadata/author may cross the boundary.
	for _, secret := range enrollmentSecrets() {
		assert.NotContainsf(t, string(raw), secret,
			"claim-only enrollment response leaked private content")
	}

	var out api.ClaimResponseBody
	require.NoError(t, json.Unmarshal(raw, &out))
	require.NotNil(t, out.Issue.Owner)
	assert.Equal(t, "tester", *out.Issue.Owner)
	require.NotNil(t, out.Issue.AssignmentExpiresOn)
	assert.Empty(t, out.Issue.Title)
	assert.Empty(t, out.Issue.Body)
	assert.Empty(t, out.Issue.Author)
	assert.Empty(t, out.Issue.Metadata)
	assert.Empty(t, out.ReplayEvents)
	require.Len(t, out.Events, 1)
	assert.Equal(t, "issue.assigned", out.Events[0].Type)
}

func TestClaim_EnrollmentRenewalReplayCarriesOnlyAssignmentEvents(t *testing.T) {
	hub := testenv.New(t)
	hubProject := createPrivateContentHubProject(t, hub, "github.com/test/private-renewal")
	issue := createPrivateEnrollmentIssue(t, hub, hubProject.ID)
	spokeUID, err := katauid.New()
	require.NoError(t, err)
	enrollment := createClaimEnrollment(t, hub, hubProject.ID, spokeUID, "claim")

	// An existing timed assignment turns the enrollment claim into a renewal,
	// which is the path that attaches assignment replay events.
	_, err = hub.DB.ClaimOwner(context.Background(), db.ClaimOwnerParams{
		IssueID: issue.ID, Actor: "tester", TTL: time.Hour,
	})
	require.NoError(t, err)
	// Private content edits land inside the renewal replay window so the raw
	// issue.created/issue.updated replay events would carry them if they ever
	// crossed the enrollment boundary.
	editedTitle := enrollmentSecretEditTitle
	editedBody := enrollmentSecretEditBody
	_, _, changed, err := hub.DB.EditIssue(context.Background(), db.EditIssueParams{
		IssueID: issue.ID, Title: &editedTitle, Body: &editedBody, Actor: "editor",
	})
	require.NoError(t, err)
	require.True(t, changed)

	headers := map[string]string{"Authorization": "Bearer " + enrollment.Token}
	resp, raw := envDoRaw(t, hub, http.MethodPost,
		issuePathRef(hubProject.ID, issue.ShortID, "actions/claim"),
		map[string]any{"ttl_seconds": 300}, headers)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)

	for _, secret := range enrollmentSecrets() {
		assert.NotContainsf(t, string(raw), secret,
			"claim-only enrollment renewal replay leaked private content")
	}

	var out api.ClaimResponseBody
	require.NoError(t, json.Unmarshal(raw, &out))
	require.NotNil(t, out.Issue.Owner)
	assert.Equal(t, "tester", *out.Issue.Owner)
	require.NotNil(t, out.Issue.AssignmentExpiresOn)
	assert.Empty(t, out.Issue.Title)
	assert.Empty(t, out.Issue.Body)
	assert.Empty(t, out.Issue.Author)
	assert.Empty(t, out.Issue.Metadata)
	require.NotEmpty(t, out.ReplayEvents, "renewal must still repair assignment state")
	for _, event := range out.ReplayEvents {
		assert.Truef(t, enrollmentAssignmentSafeEventTypes[event.Type],
			"renewal replay carried non-assignment event %q", event.Type)
	}
	require.NotNil(t, out.Event)
	assert.Equal(t, "issue.assignment_renewed", out.Event.Type)
}

func TestClaim_TimedAssignmentForwardingSpokeRepairsWithoutIssueContent(t *testing.T) {
	hub, spoke, hubProject, spokeProject, issue, token := createClaimForwardingPair(t, "claim")
	require.NoError(t, config.WriteFederationCredential(spokeProject.UID, config.FederationCredential{
		HubURL: hub.URL, HubProjectID: hubProject.ID, Token: token, Capabilities: "claim",
	}))

	// The hub copy carries private content that never reached the spoke: the
	// spoke must repair assignment state without the claim response carrying
	// any of it across. The content edit lands after the hub assignment so it
	// sits inside the renewal replay window a claim-only credential receives.
	_, err := hub.DB.ClaimOwner(context.Background(), db.ClaimOwnerParams{
		IssueID: issue.ID, Actor: "tester", TTL: time.Hour,
	})
	require.NoError(t, err)
	editedTitle := enrollmentSecretEditTitle
	editedBody := enrollmentSecretEditBody
	_, _, changed, err := hub.DB.EditIssue(context.Background(), db.EditIssueParams{
		IssueID: issue.ID, Title: &editedTitle, Body: &editedBody, Actor: "editor",
	})
	require.NoError(t, err)
	require.True(t, changed)
	_, err = hub.DB.PatchIssueMetadata(context.Background(), db.PatchIssueMetadataIn{
		IssueID: issue.ID,
		Actor:   "editor",
		Patch: map[string]jsontext.Value{
			enrollmentSecretMetaKey: jsontext.Value(`"` + enrollmentSecretMetaVal + `"`),
		},
	})
	require.NoError(t, err)

	// A refreshed baseline contains the assignment but must not hide the
	// earlier assignment event needed by a spoke that has not pulled it.
	_, refreshed, err := hub.DB.RefreshProjectFederationBaseline(t.Context(), hubProject.ID, "tester")
	require.NoError(t, err)
	require.True(t, refreshed)

	resp, raw := envDoRaw(t, spoke, http.MethodPost,
		issuePathRef(spokeProject.ID, issue.ShortID, "actions/claim"),
		map[string]any{"actor": "tester", "ttl_seconds": 300}, nil)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)

	for _, secret := range enrollmentSecrets() {
		assert.NotContainsf(t, string(raw), secret,
			"forwarded claim response leaked hub-private content to the spoke")
	}

	var out api.ClaimResponseBody
	require.NoError(t, json.Unmarshal(raw, &out))
	require.NotNil(t, out.Issue.Owner)
	assert.Equal(t, "tester", *out.Issue.Owner)
	require.NotNil(t, out.Issue.AssignmentExpiresOn)
	assert.Nil(t, out.ReplayEvents, "spoke responses never forward replay events")
	require.Len(t, out.Events, 1)
	assert.Truef(t, enrollmentAssignmentSafeEventTypes[out.Events[0].Type],
		"spoke response carried non-assignment event %q", out.Events[0].Type)

	// The spoke applied the assignment events and repaired its own replica.
	stored, err := spoke.DB.IssueByUID(context.Background(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.NotNil(t, stored.Owner)
	assert.Equal(t, "tester", *stored.Owner)
	authoritative, err := hub.DB.IssueByUID(context.Background(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, authoritative.AssignmentExpiresOn, stored.AssignmentExpiresOn)
	assert.Equal(t, out.Issue.AssignmentExpiresOn, stored.AssignmentExpiresOn)
}
