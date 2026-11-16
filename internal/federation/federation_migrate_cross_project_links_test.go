package federation

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/internal/testenv"
)

// Two linked projects move from a source hub to a target hub one at a time:
// each is merged from its own export into a Kata home and then adopted into
// the target hub. Once both have arrived, the target hub must hold the links
// between them.
func TestMigrateProjectsOneAtATimeKeepsCrossProjectLinks(t *testing.T) {
	ctx := t.Context()
	source := testenv.New(t)
	var sourceProjects []db.Project
	for _, name := range []string{"first-project", "second-project"} {
		project, err := source.DB.CreateProject(ctx, name)
		require.NoError(t, err)
		sourceProjects = append(sourceProjects, project)
	}
	create := func(index int, title string) db.Issue {
		issue, _, err := source.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: sourceProjects[index].ID, Title: title, Author: "author"})
		require.NoError(t, err)
		return issue
	}
	blocker, epic, peer := create(0, "blocker"), create(0, "epic"), create(0, "peer")
	blocked, child, related := create(1, "blocked"), create(1, "child"), create(1, "related")
	var want [][3]string
	for _, link := range []struct {
		from, to db.Issue
		typ      string
	}{{blocker, blocked, "blocks"}, {child, epic, "parent"}, {peer, related, "related"}} {
		created, err := source.DB.CreateLink(ctx, db.CreateLinkParams{FromIssueID: link.from.ID, ToIssueID: link.to.ID, Type: link.typ, Author: "link-author"})
		require.NoError(t, err)
		want = append(want, [3]string{created.FromIssueUID, created.ToIssueUID, link.typ})
	}

	target, targetProjects := newMoveDeliveryHub(t, "first-project", "second-project")
	home := testenv.New(t)
	credentials := newMoveCredentialStore()
	for index, sourceProject := range sourceProjects {
		var export bytes.Buffer
		require.NoError(t, jsonl.Export(ctx, source.DB, &export, jsonl.ExportOptions{IncludeDeleted: true, ProjectID: sourceProject.ID}))
		require.NoError(t, jsonl.ImportWithOptions(ctx, &export, home.DB, jsonl.ImportOptions{MergeProject: true}))
		local, err := home.DB.ProjectByName(ctx, sourceProject.Name)
		require.NoError(t, err)

		hubProject := targetProjects[index]
		grant, err := target.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
			SpokeInstanceUID: home.DB.InstanceUID(), ProjectID: &hubProject.ID, Capabilities: "push,pull",
			Actor: "worker", AllowAdoptionSnapshotAuthors: true,
		})
		require.NoError(t, err)
		adopted, err := home.DB.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{
			ProjectID: local.ID, HubURL: target.URL, HubProjectID: hubProject.ID, HubProjectUID: hubProject.UID, Actor: "worker",
		})
		require.NoError(t, err)
		credentials[hubProject.UID] = config.FederationCredential{HubURL: target.URL, HubProjectID: hubProject.ID, Token: grant.Token, Actor: "worker"}
		require.NoError(t, SyncFederationOnce(ctx, home.DB, adopted.Binding, credentials[hubProject.UID], credentials))
	}

	seen := map[[3]string]bool{}
	var got [][3]string
	for _, edge := range want {
		issue, err := target.DB.IssueByUID(ctx, edge[0], db.IncludeDeletedNo)
		require.NoError(t, err)
		links, err := target.DB.LinksByIssue(ctx, issue.ID)
		require.NoError(t, err)
		for _, link := range links {
			key := [3]string{link.FromIssueUID, link.ToIssueUID, link.Type}
			if !seen[key] {
				seen[key] = true
				got = append(got, key)
			}
		}
	}
	require.ElementsMatch(t, want, got, "the target hub holds every link between the two projects")
}
