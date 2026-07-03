package daemon_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
)

type graphResponseTest struct {
	SourceUID      string                `json:"source_uid"`
	Depth          string                `json:"depth"`
	HideDone       bool                  `json:"hide_done"`
	Nodes          []graphNodeTest       `json:"nodes"`
	Edges          []graphEdgeTest       `json:"edges"`
	UnresolvedRefs []graphUnresolvedTest `json:"unresolved_refs"`
}

type graphNodeTest struct {
	UID         string `json:"uid"`
	ShortID     string `json:"short_id"`
	QualifiedID string `json:"qualified_id"`
	Title       string `json:"title"`
	Status      string `json:"status"`
}

type graphEdgeTest struct {
	FromUID string `json:"from_uid"`
	ToUID   string `json:"to_uid"`
	Kind    string `json:"kind"`
	Layout  bool   `json:"layout"`
}

type graphUnresolvedTest struct {
	UID   string `json:"uid"`
	Side  string `json:"side"`
	Kind  string `json:"kind"`
	Other string `json:"other_uid"`
}

func TestReachableGraph_SourceOnly(t *testing.T) {
	h, ts, pid := graphProject(t)
	source := graphIssue(t, h, pid, "01HZNQ7VFPK1XGD8R5MABCD4EX", "source")

	graph := getGraph(t, ts, pid, source.ShortID, "")

	assert.Equal(t, source.UID, graph.SourceUID)
	assert.Equal(t, "full", graph.Depth)
	assert.False(t, graph.HideDone)
	require.Len(t, graph.Nodes, 1)
	assert.Equal(t, source.UID, graph.Nodes[0].UID)
	assert.Equal(t, "kata#"+source.ShortID, graph.Nodes[0].QualifiedID)
	assert.Empty(t, graph.Edges)
	assert.Empty(t, graph.UnresolvedRefs)
}

func TestReachableGraph_ParentChildDirectionAndReachability(t *testing.T) {
	h, ts, pid := graphProject(t)
	parent := graphIssue(t, h, pid, "01HZNQ7VFPK1XGD8R5MABCD4E1", "parent")
	child := graphIssue(t, h, pid, "01HZNQ7VFPK1XGD8R5MABCD4E2", "child")
	grandchild := graphIssue(t, h, pid, "01HZNQ7VFPK1XGD8R5MABCD4E3", "grandchild")
	graphLink(t, h, pid, child.ID, parent.ID, "parent")
	graphLink(t, h, pid, grandchild.ID, child.ID, "parent")

	depthOne := getGraph(t, ts, pid, parent.ShortID, "?depth=1")
	assert.Equal(t, []string{parent.UID, child.UID}, graphNodeUIDs(depthOne))
	assert.Equal(t, []string{"parent:" + parent.UID + "->" + child.UID + ":true"}, graphEdgeKeys(depthOne))

	full := getGraph(t, ts, pid, parent.ShortID, "?depth=full")
	assert.Equal(t, []string{parent.UID, child.UID, grandchild.UID}, graphNodeUIDs(full))
	assert.Equal(t, []string{
		"parent:" + parent.UID + "->" + child.UID + ":true",
		"parent:" + child.UID + "->" + grandchild.UID + ":true",
	}, graphEdgeKeys(full))
}

func TestReachableGraph_BlocksDirectionAndDedupe(t *testing.T) {
	h, ts, pid := graphProject(t)
	blocker := graphIssue(t, h, pid, "01HZNQ7VFPK1XGD8R5MABCD4E1", "blocker")
	blocked := graphIssue(t, h, pid, "01HZNQ7VFPK1XGD8R5MABCD4E2", "blocked")
	graphLink(t, h, pid, blocker.ID, blocked.ID, "blocks")

	graph := getGraph(t, ts, pid, blocked.ShortID, "?depth=1")

	assert.Equal(t, []string{blocker.UID, blocked.UID}, graphNodeUIDs(graph))
	assert.Equal(t, []string{"blocks:" + blocker.UID + "->" + blocked.UID + ":true"}, graphEdgeKeys(graph))
}

func TestReachableGraph_RelatedEdges(t *testing.T) {
	h, ts, pid := graphProject(t)
	a := graphIssue(t, h, pid, "01HZNQ7VFPK1XGD8R5MABCD4E1", "a")
	b := graphIssue(t, h, pid, "01HZNQ7VFPK1XGD8R5MABCD4E2", "b")
	graphRelated(t, h, pid, b.ID, a.ID)

	graph := getGraph(t, ts, pid, b.ShortID, "?depth=1")

	assert.Equal(t, []string{a.UID, b.UID}, graphNodeUIDs(graph))
	assert.Equal(t, []string{"related:" + a.UID + "->" + b.UID + ":true"}, graphEdgeKeys(graph))
}

func TestReachableGraph_TraversesCrossProjectLinksWithQualifiedIDs(t *testing.T) {
	h, ts, sourceProjectID := graphProject(t)
	ctx := context.Background()
	foreignProject, err := h.DB().CreateProject(ctx, "foreign")
	require.NoError(t, err)
	source := graphIssue(t, h, sourceProjectID, "01HZNQ7VFPK1XGD8R5MABCD4E1", "source")
	foreign := graphIssue(t, h, foreignProject.ID, "01HZNQ7VFPK1XGD8R5MABCD4E2", "foreign")
	foreignChild := graphIssue(t, h, foreignProject.ID, "01HZNQ7VFPK1XGD8R5MABCD4E3", "foreign child")
	graphLink(t, h, sourceProjectID, source.ID, foreign.ID, "blocks")
	graphLink(t, h, foreignProject.ID, foreign.ID, foreignChild.ID, "blocks")

	graph := getGraph(t, ts, sourceProjectID, source.ShortID, "?depth=full")

	assert.Equal(t, []string{source.UID, foreign.UID, foreignChild.UID}, graphNodeUIDs(graph))
	assert.Equal(t, "kata#"+source.ShortID, graphNodeByUID(t, graph, source.UID).QualifiedID)
	assert.Equal(t, "foreign#"+foreign.ShortID, graphNodeByUID(t, graph, foreign.UID).QualifiedID)
	assert.Equal(t, "foreign#"+foreignChild.ShortID, graphNodeByUID(t, graph, foreignChild.UID).QualifiedID)
	assert.Equal(t, []string{
		"blocks:" + source.UID + "->" + foreign.UID + ":true",
		"blocks:" + foreign.UID + "->" + foreignChild.UID + ":true",
	}, graphEdgeKeys(graph))
	assert.Empty(t, graph.UnresolvedRefs)
}

func TestReachableGraph_BoundedDepthVsFullTraversal(t *testing.T) {
	h, ts, pid := graphProject(t)
	a := graphIssue(t, h, pid, "01HZNQ7VFPK1XGD8R5MABCD4E1", "a")
	b := graphIssue(t, h, pid, "01HZNQ7VFPK1XGD8R5MABCD4E2", "b")
	c := graphIssue(t, h, pid, "01HZNQ7VFPK1XGD8R5MABCD4E3", "c")
	graphLink(t, h, pid, a.ID, b.ID, "blocks")
	graphLink(t, h, pid, b.ID, c.ID, "blocks")

	depthOne := getGraph(t, ts, pid, a.ShortID, "?depth=1")
	assert.Equal(t, []string{a.UID, b.UID}, graphNodeUIDs(depthOne))

	full := getGraph(t, ts, pid, a.ShortID, "?depth=full")
	assert.Equal(t, []string{a.UID, b.UID, c.UID}, graphNodeUIDs(full))
}

func TestReachableGraph_HideDoneFiltersClosedTraversal(t *testing.T) {
	h, ts, pid := graphProject(t)
	a := graphIssue(t, h, pid, "01HZNQ7VFPK1XGD8R5MABCD4E1", "a")
	b := graphIssue(t, h, pid, "01HZNQ7VFPK1XGD8R5MABCD4E2", "b")
	c := graphIssue(t, h, pid, "01HZNQ7VFPK1XGD8R5MABCD4E3", "c")
	graphLink(t, h, pid, a.ID, b.ID, "blocks")
	graphLink(t, h, pid, b.ID, c.ID, "blocks")
	_, _, changed, err := h.DB().CloseIssue(context.Background(), b.ID, "done", "tester", "closed for graph test", nil)
	require.NoError(t, err)
	require.True(t, changed)

	graph := getGraph(t, ts, pid, a.ShortID, "?depth=full&hide_done=true")

	assert.Equal(t, []string{a.UID}, graphNodeUIDs(graph))
	assert.Empty(t, graph.Edges)
}

func TestReachableGraph_UnresolvedReferences(t *testing.T) {
	h, ts, pid := graphProject(t)
	source := graphIssue(t, h, pid, "01HZNQ7VFPK1XGD8R5MABCD4E1", "source")
	missingUID := "01HZNQ7VFPK1XGD8R5MABCD4E2"
	insertUnresolvedGraphLink(t, h, pid, source.ID, 9001, source.UID, missingUID, "blocks")

	graph := getGraph(t, ts, pid, source.ShortID, "?depth=full")

	assert.Equal(t, []string{source.UID}, graphNodeUIDs(graph))
	assert.Equal(t, []string{"blocks:" + source.UID + "->" + missingUID + ":true"}, graphEdgeKeys(graph))
	require.Len(t, graph.UnresolvedRefs, 1)
	assert.Equal(t, missingUID, graph.UnresolvedRefs[0].UID)
	assert.Equal(t, "to", graph.UnresolvedRefs[0].Side)
	assert.Equal(t, "blocks", graph.UnresolvedRefs[0].Kind)
	assert.Equal(t, source.UID, graph.UnresolvedRefs[0].Other)
}

func TestReachableGraph_OmitsDeletedAndArchivedEndpoints(t *testing.T) {
	h, ts, pid := graphProject(t)
	ctx := context.Background()
	source := graphIssue(t, h, pid, "01HZNQ7VFPK1XGD8R5MABCD4E1", "source")
	deleted := graphIssue(t, h, pid, "01HZNQ7VFPK1XGD8R5MABCD4E2", "deleted")
	graphLink(t, h, pid, source.ID, deleted.ID, "blocks")
	_, _, changed, err := h.DB().SoftDeleteIssue(ctx, deleted.ID, "tester")
	require.NoError(t, err)
	require.True(t, changed)

	archivedProject, err := h.DB().CreateProject(ctx, "archived-project")
	require.NoError(t, err)
	archived := graphIssue(t, h, archivedProject.ID, "01HZNQ7VFPK1XGD8R5MABCD4E3", "archived")
	graphLink(t, h, pid, source.ID, archived.ID, "blocks")
	_, _, err = h.DB().RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: archivedProject.ID,
		Actor:     "tester",
		Force:     true,
	})
	require.NoError(t, err)

	graph := getGraph(t, ts, pid, source.ShortID, "?depth=full")

	assert.Equal(t, []string{source.UID}, graphNodeUIDs(graph))
	assert.Empty(t, graph.Edges)
	assert.Empty(t, graph.UnresolvedRefs)
}

func TestReachableGraph_TransitiveBlockPruningForLayoutOnlyAndStableOrdering(t *testing.T) {
	h, ts, pid := graphProject(t)
	c := graphIssue(t, h, pid, "01HZNQ7VFPK1XGD8R5MABCD4E3", "c")
	a := graphIssue(t, h, pid, "01HZNQ7VFPK1XGD8R5MABCD4E1", "a")
	b := graphIssue(t, h, pid, "01HZNQ7VFPK1XGD8R5MABCD4E2", "b")
	graphLink(t, h, pid, b.ID, c.ID, "blocks")
	graphLink(t, h, pid, a.ID, c.ID, "blocks")
	graphLink(t, h, pid, a.ID, b.ID, "blocks")

	graph := getGraph(t, ts, pid, a.UID, "?depth=full")

	assert.Equal(t, []string{a.UID, b.UID, c.UID}, graphNodeUIDs(graph))
	assert.Equal(t, []string{
		"blocks:" + a.UID + "->" + b.UID + ":true",
		"blocks:" + a.UID + "->" + c.UID + ":false",
		"blocks:" + b.UID + "->" + c.UID + ":true",
	}, graphEdgeKeys(graph))
}

func graphProject(t *testing.T) (*httptestServerHandle, *httptest.Server, int64) {
	t.Helper()
	h, pid := bootstrapProject(t)
	return h, h.ts.(*httptest.Server), pid
}

func graphIssue(t *testing.T, h *httptestServerHandle, projectID int64, uid, title string) db.Issue {
	t.Helper()
	issue, _, err := h.DB().CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: projectID,
		UID:       uid,
		Title:     title,
		Author:    "tester",
	})
	require.NoError(t, err)
	return issue
}

func graphLink(t *testing.T, h *httptestServerHandle, _ int64, fromID, toID int64, kind string) {
	t.Helper()
	_, err := h.DB().CreateLink(context.Background(), db.CreateLinkParams{
		FromIssueID: fromID,
		ToIssueID:   toID,
		Type:        kind,
		Author:      "tester",
	})
	require.NoError(t, err)
}

func graphRelated(t *testing.T, h *httptestServerHandle, projectID, aID, bID int64) {
	t.Helper()
	if aID > bID {
		aID, bID = bID, aID
	}
	graphLink(t, h, projectID, aID, bID, "related")
}

func insertUnresolvedGraphLink(
	t *testing.T,
	h *httptestServerHandle,
	_ int64, fromID, toID int64,
	fromUID, toUID, kind string,
) {
	t.Helper()
	_, err := h.DB().ExecContext(context.Background(), `PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	_, err = h.DB().ExecContext(context.Background(),
		`INSERT INTO links(from_issue_id, to_issue_id, from_issue_uid, to_issue_uid, type, author)
		 VALUES(?, ?, ?, ?, ?, ?)`,
		fromID, toID, fromUID, toUID, kind, "tester")
	require.NoError(t, err)
}

func getGraph(t *testing.T, ts *httptest.Server, projectID int64, ref, query string) graphResponseTest {
	t.Helper()
	resp, bs := getStatusBody(t, ts, "/api/v1/projects/"+
		strconv.FormatInt(projectID, 10)+"/issues/"+ref+"/graph"+query)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(bs))
	var graph graphResponseTest
	require.NoError(t, json.Unmarshal(bs, &graph), string(bs))
	if graph.Nodes == nil {
		graph.Nodes = []graphNodeTest{}
	}
	if graph.Edges == nil {
		graph.Edges = []graphEdgeTest{}
	}
	if graph.UnresolvedRefs == nil {
		graph.UnresolvedRefs = []graphUnresolvedTest{}
	}
	return graph
}

func graphNodeUIDs(graph graphResponseTest) []string {
	out := make([]string, 0, len(graph.Nodes))
	for _, node := range graph.Nodes {
		out = append(out, node.UID)
	}
	return out
}

func graphNodeByUID(t *testing.T, graph graphResponseTest, uid string) graphNodeTest {
	t.Helper()
	for _, node := range graph.Nodes {
		if node.UID == uid {
			return node
		}
	}
	t.Fatalf("graph missing node uid %s", uid)
	return graphNodeTest{}
}

func graphEdgeKeys(graph graphResponseTest) []string {
	out := make([]string, 0, len(graph.Edges))
	for _, edge := range graph.Edges {
		out = append(out, edge.Kind+":"+edge.FromUID+"->"+edge.ToUID+":"+strconv.FormatBool(edge.Layout))
	}
	return out
}
