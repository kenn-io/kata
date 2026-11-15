package jsonl_test

import (
	"bytes"
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/jsonl"
)

type crossProjectLinkSource struct {
	firstExport, secondExport []byte
	// edges are the links between the two projects, as from UID, to UID, type.
	edges [][3]string
}

// newCrossProjectLinkSource builds a source with two projects whose tasks are
// linked across the project boundary by every link type, and exports each
// project on its own.
func newCrossProjectLinkSource(t *testing.T) crossProjectLinkSource {
	t.Helper()
	ctx := t.Context()
	source := openExportTestDB(t)
	first, err := source.CreateProject(ctx, "first-project")
	require.NoError(t, err)
	second, err := source.CreateProject(ctx, "second-project")
	require.NoError(t, err)
	create := func(projectID int64, title string) db.Issue {
		issue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projectID, Title: title, Author: "author"})
		require.NoError(t, err)
		return issue
	}
	blocker, epic, peer := create(first.ID, "blocker"), create(first.ID, "epic"), create(first.ID, "peer")
	blocked, child, related := create(second.ID, "blocked"), create(second.ID, "child"), create(second.ID, "related")
	var out crossProjectLinkSource
	for _, link := range []struct {
		from, to db.Issue
		typ      string
	}{{blocker, blocked, "blocks"}, {child, epic, "parent"}, {peer, related, "related"}} {
		created, err := source.CreateLink(ctx, db.CreateLinkParams{FromIssueID: link.from.ID, ToIssueID: link.to.ID, Type: link.typ, Author: "link-author"})
		require.NoError(t, err)
		out.edges = append(out.edges, [3]string{created.FromIssueUID, created.ToIssueUID, link.typ})
	}
	export := func(projectID int64) []byte {
		var buffer bytes.Buffer
		require.NoError(t, jsonl.Export(ctx, source, &buffer, jsonl.ExportOptions{IncludeDeleted: true, ProjectID: projectID}))
		return buffer.Bytes()
	}
	out.firstExport, out.secondExport = export(first.ID), export(second.ID)
	return out
}

func crossProjectLinkEdges(t *testing.T, store db.Storage, issueUIDs ...string) [][3]string {
	t.Helper()
	seen := map[[3]string]bool{}
	var edges [][3]string
	for _, issueUID := range issueUIDs {
		issue, err := store.IssueByUID(t.Context(), issueUID, db.IncludeDeletedNo)
		require.NoError(t, err)
		links, err := store.LinksByIssue(t.Context(), issue.ID)
		require.NoError(t, err)
		for _, link := range links {
			edge := [3]string{link.FromIssueUID, link.ToIssueUID, link.Type}
			if !seen[edge] {
				seen[edge] = true
				edges = append(edges, edge)
			}
		}
	}
	return edges
}

// Projects move one at a time. A link between two of them can only be created
// when the second project arrives, and the merge creates it then.
func checkMergeRestoresCrossProjectLinks(t *testing.T, target db.Storage, secondFirst bool) {
	source := newCrossProjectLinkSource(t)
	if secondFirst {
		source.firstExport, source.secondExport = source.secondExport, source.firstExport
	}
	merge := func(export []byte) {
		require.NoError(t, jsonl.ImportWithOptions(t.Context(), bytes.NewReader(export), target,
			jsonl.ImportOptions{MergeProject: true}))
	}
	var endpoints []string
	for _, edge := range source.edges {
		endpoints = append(endpoints, edge[0])
	}

	merge(source.firstExport)
	merge(source.secondExport)
	require.ElementsMatch(t, undirectedRelatedEdges(source.edges), undirectedRelatedEdges(crossProjectLinkEdges(t, target, endpoints...)),
		"every link between the two projects exists once both are merged")

	links, err := target.LinksByIssue(t.Context(), mustIssueID(t, target, source.edges[0][0]))
	require.NoError(t, err)
	require.Len(t, links, 1)
	require.Equal(t, "link-author", links[0].Author, "the link keeps its author")
}

// A related link has no direction. The store keeps the lower local issue ID
// first, so which UID comes first depends on the order the issues arrived.
func undirectedRelatedEdges(edges [][3]string) [][3]string {
	out := make([][3]string, 0, len(edges))
	for _, edge := range edges {
		if edge[2] == "related" && edge[0] > edge[1] {
			edge[0], edge[1] = edge[1], edge[0]
		}
		out = append(out, edge)
	}
	return out
}

func mustIssueID(t *testing.T, store db.Storage, issueUID string) int64 {
	t.Helper()
	issue, err := store.IssueByUID(t.Context(), issueUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	return issue.ID
}

// Merge order must not matter. A related link is stored with its lower local
// issue ID first, and the merge assigns new local IDs.
func TestMergeRestoresCrossProjectLinksSQLite(t *testing.T) {
	for name, secondFirst := range map[string]bool{"source order": false, "reverse order": true} {
		t.Run(name, func(t *testing.T) {
			checkMergeRestoresCrossProjectLinks(t, openImportTargetDB(t), secondFirst)
		})
	}
}

func TestMergeRestoresCrossProjectLinksPostgres(t *testing.T) {
	for name, secondFirst := range map[string]bool{"source order": false, "reverse order": true} {
		t.Run(name, func(t *testing.T) {
			checkMergeRestoresCrossProjectLinks(t, openPostgresImportTarget(t), secondFirst)
		})
	}
}

// Ordinary link writes refuse a link between projects when either one is
// relayed between hubs, because the relay cannot deliver it. A merge leaves
// that link out and still merges the project.
func checkMergeSkipsLinkAcrossRelayedProject(t *testing.T, target db.Storage) {
	ctx := t.Context()
	source := newCrossProjectLinkSource(t)
	require.NoError(t, jsonl.ImportWithOptions(ctx, bytes.NewReader(source.firstExport), target, jsonl.ImportOptions{MergeProject: true}))
	first, err := target.ProjectByName(ctx, "first-project")
	require.NoError(t, err)
	_, err = target.EnableProjectFederation(ctx, first.ID, "operator")
	require.NoError(t, err)
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, target.PinRootAuthority(ctx, db.RootKeyPin{
		ProjectUID: first.UID, AuthorityUID: target.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public,
	}))
	//nolint:gosec // This token is a deterministic fixture credential, never an operational secret.
	parent, _, err := target.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "relay-parent-test-token", Actor: "member", AdminActor: "operator",
	})
	require.NoError(t, err)
	//nolint:gosec // This token is a deterministic fixture credential, never an operational secret.
	_, err = target.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{
		ProjectID: first.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006",
		ProtocolVersion: db.RelayProtocolVersion, Token: "relay-peer-test-token",
	})
	require.NoError(t, err)

	require.NoError(t, jsonl.ImportWithOptions(ctx, bytes.NewReader(source.secondExport), target, jsonl.ImportOptions{MergeProject: true}))
	_, err = target.ProjectByName(ctx, "second-project")
	require.NoError(t, err, "the project merges")
	var endpoints []string
	for _, edge := range source.edges {
		endpoints = append(endpoints, edge[0])
	}
	require.Empty(t, crossProjectLinkEdges(t, target, endpoints...), "no link reaches the relayed project")
}

func TestMergeSkipsLinkAcrossRelayedProjectSQLite(t *testing.T) {
	checkMergeSkipsLinkAcrossRelayedProject(t, openImportTargetDB(t))
}

func TestMergeSkipsLinkAcrossRelayedProjectPostgres(t *testing.T) {
	checkMergeSkipsLinkAcrossRelayedProject(t, openPostgresImportTarget(t))
}

// Two snapshots taken at different times can each be valid and still disagree.
// The first project was exported while A1 was a child of A2. That link was
// then removed, and A2 became a child of B, and B a child of A1. Merging both
// must not join the two states into a parent cycle, so the merge leaves out
// the link that would close it.
func checkMergeSkipsParentCycle(t *testing.T, target db.Storage) {
	ctx := t.Context()
	source := openExportTestDB(t)
	first, err := source.CreateProject(ctx, "first-project")
	require.NoError(t, err)
	second, err := source.CreateProject(ctx, "second-project")
	require.NoError(t, err)
	create := func(projectID int64, title string) db.Issue {
		issue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projectID, Title: title, Author: "author"})
		require.NoError(t, err)
		return issue
	}
	parent := func(child, of db.Issue) db.Link {
		link, err := source.CreateLink(ctx, db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: of.ID, Type: "parent", Author: "link-author"})
		require.NoError(t, err)
		return link
	}
	export := func(projectID int64) []byte {
		var buffer bytes.Buffer
		require.NoError(t, jsonl.Export(ctx, source, &buffer, jsonl.ExportOptions{IncludeDeleted: true, ProjectID: projectID}))
		return buffer.Bytes()
	}
	a1, a2, b := create(first.ID, "a1"), create(first.ID, "a2"), create(second.ID, "b")
	earlier := parent(a1, a2)
	firstExport := export(first.ID)
	require.NoError(t, source.DeleteLinkByID(ctx, earlier.ID))
	parent(a2, b)
	parent(b, a1)
	secondExport := export(second.ID)

	require.NoError(t, jsonl.ImportWithOptions(ctx, bytes.NewReader(firstExport), target, jsonl.ImportOptions{MergeProject: true}))
	require.NoError(t, jsonl.ImportWithOptions(ctx, bytes.NewReader(secondExport), target, jsonl.ImportOptions{MergeProject: true}))
	_, err = target.ProjectByName(ctx, "second-project")
	require.NoError(t, err, "the project merges")
	parents := crossProjectLinkEdges(t, target, a1.UID, a2.UID, b.UID)
	require.Contains(t, parents, [3]string{a1.UID, a2.UID, "parent"}, "the link the target already held stays")
	require.Len(t, parents, 2, "one of the two new parent links is left out, so no cycle forms: %v", parents)
}

func TestMergeSkipsParentCycleSQLite(t *testing.T) {
	checkMergeSkipsParentCycle(t, openImportTargetDB(t))
}

func TestMergeSkipsParentCyclePostgres(t *testing.T) {
	checkMergeSkipsParentCycle(t, openPostgresImportTarget(t))
}

// The first project was exported while its task was a child of a task in the
// same project. The task then moved under a task of the second project, and
// the second project's snapshot carries that newer parent link. A task has one
// parent, so the merge keeps the parent the target already holds and leaves
// the other link out.
func checkMergeKeepsExistingParent(t *testing.T, target db.Storage) {
	ctx := t.Context()
	source := openExportTestDB(t)
	first, err := source.CreateProject(ctx, "first-project")
	require.NoError(t, err)
	second, err := source.CreateProject(ctx, "second-project")
	require.NoError(t, err)
	create := func(projectID int64, title string) db.Issue {
		issue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projectID, Title: title, Author: "author"})
		require.NoError(t, err)
		return issue
	}
	parent := func(child, of db.Issue) db.Link {
		link, err := source.CreateLink(ctx, db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: of.ID, Type: "parent", Author: "link-author"})
		require.NoError(t, err)
		return link
	}
	export := func(projectID int64) []byte {
		var buffer bytes.Buffer
		require.NoError(t, jsonl.Export(ctx, source, &buffer, jsonl.ExportOptions{IncludeDeleted: true, ProjectID: projectID}))
		return buffer.Bytes()
	}
	child, oldParent, newParent := create(first.ID, "child"), create(first.ID, "old parent"), create(second.ID, "new parent")
	earlier := parent(child, oldParent)
	firstExport := export(first.ID)
	require.NoError(t, source.DeleteLinkByID(ctx, earlier.ID))
	parent(child, newParent)
	secondExport := export(second.ID)

	require.NoError(t, jsonl.ImportWithOptions(ctx, bytes.NewReader(firstExport), target, jsonl.ImportOptions{MergeProject: true}))
	require.NoError(t, jsonl.ImportWithOptions(ctx, bytes.NewReader(secondExport), target, jsonl.ImportOptions{MergeProject: true}))
	_, err = target.ProjectByName(ctx, "second-project")
	require.NoError(t, err, "the project merges")
	require.Equal(t, [][3]string{{child.UID, oldParent.UID, "parent"}},
		crossProjectLinkEdges(t, target, child.UID, oldParent.UID, newParent.UID),
		"the task keeps the parent the target already held")
}

func TestMergeKeepsExistingParentSQLite(t *testing.T) {
	checkMergeKeepsExistingParent(t, openImportTargetDB(t))
}

func TestMergeKeepsExistingParentPostgres(t *testing.T) {
	checkMergeKeepsExistingParent(t, openPostgresImportTarget(t))
}
