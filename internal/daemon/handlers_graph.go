package daemon

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

type reachableGraphOptions struct {
	Depth    string
	HideDone bool
}

type parsedGraphDepth struct {
	Label string
	Full  bool
	Max   int
}

type graphLinkRow struct {
	ID           int64
	FromIssueID  int64
	ToIssueID    int64
	FromIssueUID string
	ToIssueUID   string
	Kind         string
}

type graphNeighbor struct {
	IssueID  int64
	IssueUID string
}

func (n graphNeighbor) Compare(other graphNeighbor) int {
	return cmp.Compare(n.IssueUID, other.IssueUID)
}

func buildReachableIssueGraph(
	ctx context.Context,
	store *db.DB,
	projectID int64,
	source db.Issue,
	opts reachableGraphOptions,
) (*api.ReachableGraphResponse, error) {
	depth, err := parseReachableGraphDepth(opts.Depth)
	if err != nil {
		return nil, err
	}
	issues, err := store.ListIssues(ctx, db.ListIssuesParams{ProjectID: projectID})
	if err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}
	links, err := listGraphLinks(ctx, store, projectID)
	if err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}
	project, err := store.ProjectByID(ctx, projectID)
	if err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}

	issueByID := make(map[int64]db.Issue, len(issues))
	for _, issue := range issues {
		issueByID[issue.ID] = issue
	}
	if _, ok := issueByID[source.ID]; !ok {
		issueByID[source.ID] = source
	}

	visible := func(issue db.Issue) bool {
		return issue.ID == source.ID || !opts.HideDone || issue.Status != "closed"
	}
	adjacent := make(map[int64][]graphNeighbor)
	for _, link := range links {
		from, fromOK := issueByID[link.FromIssueID]
		to, toOK := issueByID[link.ToIssueID]
		if !fromOK || !toOK || !visible(from) || !visible(to) {
			continue
		}
		adjacent[link.FromIssueID] = append(adjacent[link.FromIssueID], graphNeighbor{
			IssueID:  link.ToIssueID,
			IssueUID: to.UID,
		})
		adjacent[link.ToIssueID] = append(adjacent[link.ToIssueID], graphNeighbor{
			IssueID:  link.FromIssueID,
			IssueUID: from.UID,
		})
	}
	for id := range adjacent {
		slices.SortFunc(adjacent[id], graphNeighbor.Compare)
	}

	dist := traverseGraph(source.ID, adjacent, depth)

	nodes := make([]api.ReachableGraphNode, 0, len(dist))
	for id := range dist {
		issue, ok := issueByID[id]
		if !ok || !visible(issue) {
			continue
		}
		nodes = append(nodes, api.ReachableGraphNode{
			Issue:       issue,
			QualifiedID: project.Name + "#" + issue.ShortID,
		})
	}
	slices.SortFunc(nodes, api.ReachableGraphNode.Compare)

	edges, unresolved := graphEdgesAndUnresolved(links, issueByID, dist, depth, opts.HideDone, source.ID)
	markTransitiveBlockLayout(edges)

	out := &api.ReachableGraphResponse{}
	out.Body.SourceUID = source.UID
	out.Body.Depth = depth.Label
	out.Body.HideDone = opts.HideDone
	out.Body.Nodes = nodes
	out.Body.Edges = edges
	out.Body.UnresolvedRefs = unresolved
	return out, nil
}

func parseReachableGraphDepth(raw string) (parsedGraphDepth, error) {
	if raw == "" || raw == "full" {
		return parsedGraphDepth{Label: "full", Full: true}, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return parsedGraphDepth{}, api.NewError(400, "validation",
			"depth must be full or a non-negative integer", "", nil)
	}
	return parsedGraphDepth{Label: strconv.Itoa(n), Max: n}, nil
}

func listGraphLinks(ctx context.Context, store *db.DB, projectID int64) ([]graphLinkRow, error) {
	rows, err := store.QueryContext(ctx,
		`SELECT id, from_issue_id, to_issue_id, from_issue_uid, to_issue_uid, type
		   FROM links
		  WHERE project_id = ?
		  ORDER BY type ASC, from_issue_uid ASC, to_issue_uid ASC, id ASC`,
		projectID)
	if err != nil {
		return nil, fmt.Errorf("list graph links: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []graphLinkRow
	for rows.Next() {
		var link graphLinkRow
		if err := rows.Scan(&link.ID, &link.FromIssueID, &link.ToIssueID,
			&link.FromIssueUID, &link.ToIssueUID, &link.Kind); err != nil {
			return nil, fmt.Errorf("scan graph link: %w", err)
		}
		out = append(out, link)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate graph links: %w", err)
	}
	return out, nil
}

func traverseGraph(sourceID int64, adjacent map[int64][]graphNeighbor, depth parsedGraphDepth) map[int64]int {
	dist := map[int64]int{sourceID: 0}
	queue := []int64{sourceID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		currentDepth := dist[id]
		if !depth.Full && currentDepth >= depth.Max {
			continue
		}
		for _, next := range adjacent[id] {
			if _, seen := dist[next.IssueID]; seen {
				continue
			}
			dist[next.IssueID] = currentDepth + 1
			queue = append(queue, next.IssueID)
		}
	}
	return dist
}

func graphEdgesAndUnresolved(
	links []graphLinkRow,
	issueByID map[int64]db.Issue,
	dist map[int64]int,
	depth parsedGraphDepth,
	hideDone bool,
	sourceID int64,
) ([]api.ReachableGraphEdge, []api.ReachableGraphUnresolvedRef) {
	visible := func(id int64) bool {
		issue, ok := issueByID[id]
		if !ok {
			return false
		}
		return id == sourceID || !hideDone || issue.Status != "closed"
	}

	edgeSeen := map[string]struct{}{}
	unresolvedSeen := map[string]struct{}{}
	var edges []api.ReachableGraphEdge
	var unresolved []api.ReachableGraphUnresolvedRef
	for _, link := range links {
		fromID, toID, fromUID, toUID := canonicalGraphEdge(link)
		fromIssue, fromExists := issueByID[fromID]
		toIssue, toExists := issueByID[toID]
		fromReachedDepth, fromReached := dist[fromID]
		toReachedDepth, toReached := dist[toID]

		include := false
		if fromExists && toExists {
			include = fromReached && toReached && visible(fromID) && visible(toID)
		} else if fromExists && fromReached && visible(fromID) && canExpandUnresolved(fromReachedDepth, depth) {
			include = true
			unresolved = appendUnresolvedRef(unresolved, unresolvedSeen, toUID, "to", link.Kind, fromIssue.UID)
		} else if toExists && toReached && visible(toID) && canExpandUnresolved(toReachedDepth, depth) {
			include = true
			unresolved = appendUnresolvedRef(unresolved, unresolvedSeen, fromUID, "from", link.Kind, toIssue.UID)
		}
		if !include {
			continue
		}
		key := link.Kind + "\x00" + fromUID + "\x00" + toUID
		if _, ok := edgeSeen[key]; ok {
			continue
		}
		edgeSeen[key] = struct{}{}
		edges = append(edges, api.ReachableGraphEdge{
			FromUID: fromUID,
			ToUID:   toUID,
			Kind:    link.Kind,
			Layout:  true,
		})
	}

	slices.SortFunc(edges, api.ReachableGraphEdge.Compare)
	slices.SortFunc(unresolved, api.ReachableGraphUnresolvedRef.Compare)
	return edges, unresolved
}

func canonicalGraphEdge(link graphLinkRow) (fromID, toID int64, fromUID, toUID string) {
	if link.Kind == "parent" {
		return link.ToIssueID, link.FromIssueID, link.ToIssueUID, link.FromIssueUID
	}
	return link.FromIssueID, link.ToIssueID, link.FromIssueUID, link.ToIssueUID
}

func canExpandUnresolved(nodeDepth int, depth parsedGraphDepth) bool {
	return depth.Full || nodeDepth < depth.Max
}

func appendUnresolvedRef(
	out []api.ReachableGraphUnresolvedRef,
	seen map[string]struct{},
	uid, side, kind, otherUID string,
) []api.ReachableGraphUnresolvedRef {
	key := uid + "\x00" + side + "\x00" + kind + "\x00" + otherUID
	if _, ok := seen[key]; ok {
		return out
	}
	seen[key] = struct{}{}
	return append(out, api.ReachableGraphUnresolvedRef{
		UID:      uid,
		Side:     side,
		Kind:     kind,
		OtherUID: otherUID,
	})
}

func markTransitiveBlockLayout(edges []api.ReachableGraphEdge) {
	blockAdj := map[string][]string{}
	for _, edge := range edges {
		if edge.Kind != "blocks" {
			continue
		}
		blockAdj[edge.FromUID] = append(blockAdj[edge.FromUID], edge.ToUID)
	}
	for uid := range blockAdj {
		slices.Sort(blockAdj[uid])
	}
	for i := range edges {
		if edges[i].Kind != "blocks" {
			continue
		}
		if hasAlternateBlockPath(blockAdj, edges[i].FromUID, edges[i].ToUID) {
			edges[i].Layout = false
		}
	}
}

func hasAlternateBlockPath(adj map[string][]string, from, to string) bool {
	seen := map[string]struct{}{from: {}}
	queue := append([]string(nil), adj[from]...)
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if next == to {
			continue
		}
		if _, ok := seen[next]; ok {
			continue
		}
		seen[next] = struct{}{}
		for _, peer := range adj[next] {
			if peer == to {
				return true
			}
			if _, ok := seen[peer]; !ok {
				queue = append(queue, peer)
			}
		}
	}
	return false
}
