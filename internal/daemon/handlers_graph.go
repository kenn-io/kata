package daemon

import (
	"context"
	"errors"
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
	Full bool
	Max  int
}

func (d parsedGraphDepth) String() string {
	if d.Full {
		return "full"
	}
	return strconv.Itoa(d.Max)
}

func parseReachableGraphDepth(raw string) (parsedGraphDepth, error) {
	if raw == "" || raw == "full" {
		return parsedGraphDepth{Full: true}, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return parsedGraphDepth{}, api.NewError(400, "validation",
			"depth must be full or a non-negative integer", "", nil)
	}
	return parsedGraphDepth{Max: n}, nil
}

// graphWalk holds per-request traversal state: lazily hydrated issues and
// projects, memoized link reads, and link endpoints that exist in storage but
// are hidden from the active graph (soft-deleted issues, archived projects).
type graphWalk struct {
	store        db.Storage
	sourceID     int64
	hideDone     bool
	issueByID    map[int64]db.Issue
	projectByID  map[int64]db.Project
	linksByIssue map[int64][]db.Link
	hidden       map[int64]struct{}
}

func buildReachableIssueGraph(
	ctx context.Context,
	store db.Storage,
	source db.Issue,
	opts reachableGraphOptions,
) (*api.ReachableGraphResponse, error) {
	depth, err := parseReachableGraphDepth(opts.Depth)
	if err != nil {
		return nil, err
	}
	w := &graphWalk{
		store:        store,
		sourceID:     source.ID,
		hideDone:     opts.HideDone,
		issueByID:    map[int64]db.Issue{source.ID: source},
		projectByID:  map[int64]db.Project{},
		linksByIssue: map[int64][]db.Link{},
		hidden:       map[int64]struct{}{},
	}

	dist, err := w.traverse(ctx, depth)
	if err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}

	nodes := make([]api.ReachableGraphNode, 0, len(dist))
	for id := range dist {
		issue := w.issueByID[id]
		project, err := w.project(ctx, issue.ProjectID)
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		nodes = append(nodes, api.ReachableGraphNode{
			Issue:       issue,
			QualifiedID: qualifiedID(project.Name, issue.ShortID),
		})
	}
	slices.SortFunc(nodes, api.ReachableGraphNode.Compare)

	links, err := w.linksForReached(ctx, dist)
	if err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}
	edges, unresolved := w.edgesAndUnresolved(links, dist, depth)
	markTransitiveBlockLayout(edges)

	out := &api.ReachableGraphResponse{}
	out.Body.SourceUID = source.UID
	out.Body.Depth = depth.String()
	out.Body.HideDone = opts.HideDone
	out.Body.Nodes = nodes
	out.Body.Edges = edges
	out.Body.UnresolvedRefs = unresolved
	return out, nil
}

// visible reports whether an issue participates in the graph under the
// hide-done option. The source issue is always shown.
func (w *graphWalk) visible(issue db.Issue) bool {
	return issue.ID == w.sourceID || !w.hideDone || issue.Status != "closed"
}

// issue lazily hydrates an issue by id. ok is false when the issue does not
// exist or is hidden (soft-deleted, or its project is archived); hidden
// endpoints are recorded so edge building can drop their links instead of
// reporting them as unresolved references.
func (w *graphWalk) issue(ctx context.Context, issueID int64) (db.Issue, bool, error) {
	if issue, ok := w.issueByID[issueID]; ok {
		return issue, true, nil
	}
	if _, ok := w.hidden[issueID]; ok {
		return db.Issue{}, false, nil
	}
	issue, err := w.store.IssueByID(ctx, issueID)
	if errors.Is(err, db.ErrNotFound) {
		return db.Issue{}, false, nil
	}
	if err != nil {
		return db.Issue{}, false, err
	}
	if issue.DeletedAt != nil {
		w.hidden[issueID] = struct{}{}
		return db.Issue{}, false, nil
	}
	project, err := w.project(ctx, issue.ProjectID)
	if errors.Is(err, db.ErrNotFound) {
		return db.Issue{}, false, nil
	}
	if err != nil {
		return db.Issue{}, false, err
	}
	if project.DeletedAt != nil {
		w.hidden[issueID] = struct{}{}
		return db.Issue{}, false, nil
	}
	w.issueByID[issueID] = issue
	return issue, true, nil
}

func (w *graphWalk) project(ctx context.Context, projectID int64) (db.Project, error) {
	if project, ok := w.projectByID[projectID]; ok {
		return project, nil
	}
	project, err := w.store.ProjectByID(ctx, projectID)
	if err != nil {
		return db.Project{}, err
	}
	w.projectByID[projectID] = project
	return project, nil
}

func (w *graphWalk) links(ctx context.Context, issueID int64) ([]db.Link, error) {
	if links, ok := w.linksByIssue[issueID]; ok {
		return links, nil
	}
	links, err := w.store.LinksByIssue(ctx, issueID)
	if err != nil {
		return nil, err
	}
	w.linksByIssue[issueID] = links
	return links, nil
}

// traverse runs a breadth-first walk over links from the source, returning
// each reached issue's hop distance. Only visible issues are entered.
func (w *graphWalk) traverse(ctx context.Context, depth parsedGraphDepth) (map[int64]int, error) {
	dist := map[int64]int{w.sourceID: 0}
	queue := []int64{w.sourceID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if !depth.Full && dist[id] >= depth.Max {
			continue
		}
		links, err := w.links(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, link := range links {
			neighborID := link.ToIssueID
			if link.ToIssueID == id {
				neighborID = link.FromIssueID
			}
			if _, seen := dist[neighborID]; seen {
				continue
			}
			neighbor, ok, err := w.issue(ctx, neighborID)
			if err != nil {
				return nil, err
			}
			if !ok || !w.visible(neighbor) {
				continue
			}
			dist[neighborID] = dist[id] + 1
			queue = append(queue, neighborID)
		}
	}
	return dist, nil
}

func (w *graphWalk) linksForReached(ctx context.Context, dist map[int64]int) ([]db.Link, error) {
	seen := map[int64]struct{}{}
	var out []db.Link
	for issueID := range dist {
		links, err := w.links(ctx, issueID)
		if err != nil {
			return nil, err
		}
		for _, link := range links {
			if _, ok := seen[link.ID]; ok {
				continue
			}
			seen[link.ID] = struct{}{}
			out = append(out, link)
		}
	}
	return out, nil
}

func (w *graphWalk) edgesAndUnresolved(
	links []db.Link,
	dist map[int64]int,
	depth parsedGraphDepth,
) ([]api.ReachableGraphEdge, []api.ReachableGraphUnresolvedRef) {
	visible := func(id int64) bool {
		issue, ok := w.issueByID[id]
		return ok && w.visible(issue)
	}

	edgeSeen := map[string]struct{}{}
	unresolvedSeen := map[string]struct{}{}
	var edges []api.ReachableGraphEdge
	var unresolved []api.ReachableGraphUnresolvedRef
	for _, link := range links {
		fromID, toID, fromUID, toUID := canonicalGraphEdge(link)
		if _, ok := w.hidden[fromID]; ok {
			continue
		}
		if _, ok := w.hidden[toID]; ok {
			continue
		}
		fromIssue, fromExists := w.issueByID[fromID]
		toIssue, toExists := w.issueByID[toID]
		fromReachedDepth, fromReached := dist[fromID]
		toReachedDepth, toReached := dist[toID]

		include := false
		if fromExists && toExists {
			include = fromReached && toReached && visible(fromID) && visible(toID)
		} else if fromExists && fromReached && visible(fromID) && canExpandUnresolved(fromReachedDepth, depth) {
			include = true
			unresolved = appendUnresolvedRef(unresolved, unresolvedSeen, toUID, "to", link.Type, fromIssue.UID)
		} else if toExists && toReached && visible(toID) && canExpandUnresolved(toReachedDepth, depth) {
			include = true
			unresolved = appendUnresolvedRef(unresolved, unresolvedSeen, fromUID, "from", link.Type, toIssue.UID)
		}
		if !include {
			continue
		}
		key := link.Type + "\x00" + fromUID + "\x00" + toUID
		if _, ok := edgeSeen[key]; ok {
			continue
		}
		edgeSeen[key] = struct{}{}
		edges = append(edges, api.ReachableGraphEdge{
			FromUID: fromUID,
			ToUID:   toUID,
			Kind:    link.Type,
			Layout:  true,
		})
	}

	slices.SortFunc(edges, api.ReachableGraphEdge.Compare)
	slices.SortFunc(unresolved, api.ReachableGraphUnresolvedRef.Compare)
	return edges, unresolved
}

// canonicalGraphEdge orients an edge for display: parent links are stored
// child -> parent but rendered parent -> child.
func canonicalGraphEdge(link db.Link) (fromID, toID int64, fromUID, toUID string) {
	if link.Type == "parent" {
		return link.ToIssueID, link.FromIssueID, link.ToIssueUID, link.FromIssueUID
	}
	return link.FromIssueID, link.ToIssueID, link.FromIssueUID, link.ToIssueUID
}

// canExpandUnresolved reports whether traversal would have expanded a node's
// neighbors, i.e. whether a dangling endpoint next to it is genuinely
// unresolved rather than simply beyond the depth bound.
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
