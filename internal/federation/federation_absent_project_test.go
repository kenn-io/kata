package federation

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// A spoke joined to one project keeps pulling when an issue moves in from a
// project the spoke never joined (#1046).
func TestSyncFederationPullsMoveFromUnjoinedProject(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "absent-source", "joined-project")
	spoke := newMoveDeliverySpoke(t, hub, projects[1:])
	issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "inbound", Author: "worker"})
	require.NoError(t, err)
	_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	later, laterEvent, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[1].ID, Title: "later", Author: "worker"})
	require.NoError(t, err)

	require.NoError(t, spoke.sync(t, 0))

	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, binding.PullCursorEventID, laterEvent.ID)
	got, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, spoke.projects[0].ID, got.ProjectID)
	require.Equal(t, "inbound", got.Title)
	_, err = spoke.env.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
}

// A spoke joined to one project keeps pulling when an issue moves out to a
// project the spoke never joined (#1046).
func TestSyncFederationPullsMoveToUnjoinedProject(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "joined-project", "absent-target")
	spoke := newMoveDeliverySpoke(t, hub, projects[:1])
	issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "outbound", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	later, laterEvent, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "later", Author: "worker"})
	require.NoError(t, err)

	require.NoError(t, spoke.sync(t, 0))

	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, binding.PullCursorEventID, laterEvent.ID)
	_, err = spoke.env.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	issues, err := spoke.env.DB.ListIssues(ctx, db.ListIssuesParams{ProjectID: spoke.projects[0].ID})
	require.NoError(t, err)
	for _, listed := range issues {
		require.NotEqual(t, issue.UID, listed.UID, "the moved issue leaves the joined project")
	}
}

// A parked issue is outside the joined projects, so the spoke drops its links
// as it does for any issue there. A child reparented away from it on the hub
// must not end up with two parents.
func TestSyncFederationParksIssueWhoseChildWasReparented(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "joined-project", "absent-target")
	spoke := newMoveDeliverySpoke(t, hub, projects[:1])
	oldParent := createHubIssue(t, hub, projects[0], "old parent")
	newParent := createHubIssue(t, hub, projects[0], "new parent")
	child := createHubIssue(t, hub, projects[0], "child")
	link, _, err := hub.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: oldParent.ID, Type: "parent", Author: "worker"}, parentLinkEvent("issue.linked", child, oldParent))
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	_, err = hub.DB.ReplaceParentAndEvents(ctx, db.ReplaceParentAndEventsParams{ExpectedParentLinkID: link.ID, ExpectedParentIssueID: oldParent.ID, Link: db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: newParent.ID, Type: "parent", Author: "worker"}, UnlinkEvent: parentLinkEvent("issue.unlinked", child, oldParent), LinkEvent: parentLinkEvent("issue.linked", child, newParent)})
	require.NoError(t, err)
	moveHubIssue(t, hub, oldParent, projects[0], projects[1])

	require.NoError(t, spoke.sync(t, 0))

	local, err := spoke.env.DB.IssueByUID(ctx, child.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	links, err := spoke.env.DB.LinksByIssue(ctx, local.ID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	require.Equal(t, newParent.UID, links[0].ToIssueUID)
	parked, err := spoke.env.DB.IssueByUID(ctx, oldParent.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	links, err = spoke.env.DB.LinksByIssue(ctx, parked.ID)
	require.NoError(t, err)
	require.Empty(t, links)
}

// Joining a placeholder's project brings back the links of issues parked in it.
func TestSyncFederationJoiningPlaceholderRestoresParkedLinks(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "joined-project", "absent-target")
	spoke := newMoveDeliverySpoke(t, hub, projects[:1])
	parent := createHubIssue(t, hub, projects[0], "parent")
	child := createHubIssue(t, hub, projects[0], "child")
	_, _, err := hub.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: parent.ID, Type: "parent", Author: "worker"}, parentLinkEvent("issue.linked", child, parent))
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	moveHubIssue(t, hub, child, projects[0], projects[1])
	require.NoError(t, spoke.sync(t, 0))
	local, err := spoke.env.DB.IssueByUID(ctx, child.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	links, err := spoke.env.DB.LinksByIssue(ctx, local.ID)
	require.NoError(t, err)
	require.Empty(t, links, "a parked issue keeps no links")

	placeholder, err := spoke.env.DB.ProjectByUID(ctx, projects[1].UID)
	require.NoError(t, err)
	_, err = spoke.env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: placeholder.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: projects[1].ID, HubProjectUID: projects[1].UID, Enabled: true, PushEnabled: true, Actor: "worker"})
	require.NoError(t, err)
	placeholder, _, _, err = spoke.env.DB.RestoreProject(ctx, placeholder.ID, "worker")
	require.NoError(t, err)
	grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &projects[1].ID, Capabilities: "push,pull", Actor: "worker"})
	require.NoError(t, err)
	spoke.credentials[projects[1].UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[1].ID, Token: grant.Token, Actor: "worker"}
	spoke.projects = append(spoke.projects, placeholder)
	require.NoError(t, spoke.sync(t, 1))

	links, err = spoke.env.DB.LinksByIssue(ctx, local.ID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	require.Equal(t, parent.UID, links[0].ToIssueUID)
}

func createHubIssue(t *testing.T, hub *testenv.Env, project db.Project, title string) db.Issue {
	t.Helper()
	issue, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: title, Author: "worker"})
	require.NoError(t, err)
	return issue
}

func moveHubIssue(t *testing.T, hub *testenv.Env, issue db.Issue, from, to db.Project) {
	t.Helper()
	current, err := hub.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = hub.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: current.ID, FromProjectID: from.ID, ToProjectID: to.ID, IfMatchRev: current.Revision, Actor: "worker", Single: true})
	require.NoError(t, err)
}

func parentLinkEvent(eventType string, child, parent db.Issue) db.LinkEventParams {
	return db.LinkEventParams{EventType: eventType, EventIssueID: child.ID, FromShortID: child.ShortID, FromUID: child.UID, ToShortID: parent.ShortID, ToUID: parent.UID, Actor: "worker"}
}
