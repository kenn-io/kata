package jsonl_test

import (
	"bytes"
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
// when the second project arrives. With LinkExistingIssues it is created then.
func checkMergeRestoresCrossProjectLinks(t *testing.T, target db.Storage) {
	source := newCrossProjectLinkSource(t)
	merge := func(export []byte) {
		require.NoError(t, jsonl.ImportWithOptions(t.Context(), bytes.NewReader(export), target,
			jsonl.ImportOptions{MergeProject: true, LinkExistingIssues: true}))
	}
	var endpoints []string
	for _, edge := range source.edges {
		endpoints = append(endpoints, edge[0])
	}

	merge(source.firstExport)
	merge(source.secondExport)
	require.ElementsMatch(t, source.edges, crossProjectLinkEdges(t, target, endpoints...),
		"every link between the two projects exists once both are merged")

	links, err := target.LinksByIssue(t.Context(), mustIssueID(t, target, source.edges[0][0]))
	require.NoError(t, err)
	require.Len(t, links, 1)
	require.Equal(t, "link-author", links[0].Author, "the link keeps its author")
}

func mustIssueID(t *testing.T, store db.Storage, issueUID string) int64 {
	t.Helper()
	issue, err := store.IssueByUID(t.Context(), issueUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	return issue.ID
}

func TestMergeRestoresCrossProjectLinksSQLite(t *testing.T) {
	checkMergeRestoresCrossProjectLinks(t, openImportTargetDB(t))
}

func TestMergeRestoresCrossProjectLinksPostgres(t *testing.T) {
	checkMergeRestoresCrossProjectLinks(t, openPostgresImportTarget(t))
}

// Without the option a merge never touches another project's links.
func TestMergeWithoutLinkExistingLeavesOtherProjectsUnlinked(t *testing.T) {
	source := newCrossProjectLinkSource(t)
	target := openImportTargetDB(t)
	for _, export := range [][]byte{source.firstExport, source.secondExport} {
		require.NoError(t, jsonl.ImportWithOptions(t.Context(), bytes.NewReader(export), target, jsonl.ImportOptions{MergeProject: true}))
	}
	var endpoints []string
	for _, edge := range source.edges {
		endpoints = append(endpoints, edge[0])
	}
	require.Empty(t, crossProjectLinkEdges(t, target, endpoints...))
}

func TestLinkExistingRequiresMerge(t *testing.T) {
	source := newCrossProjectLinkSource(t)
	err := jsonl.ImportWithOptions(t.Context(), bytes.NewReader(source.firstExport), openImportTargetDB(t),
		jsonl.ImportOptions{LinkExistingIssues: true})
	require.ErrorContains(t, err, "applies only to a project merge")
}
