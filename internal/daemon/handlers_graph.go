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
	allowed      map[int64]struct{}
	scoped       bool
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
	allowed, scoped, err := issueScopedAllowedIDSet(ctx, store)
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
		allowed:      allowed,
		scoped:       scoped,
	}

	dist, err := w.traverse(ctx, depth)
	if err != nil {
		return nil, internalAPIError(err)
	}

	nodes := make([]api.ReachableGraphNode, 0, len(dist))
	for id := range dist {
		issue := w.issueByID[id]
		project, err := w.project(ctx, issue.ProjectID)
		if err != nil {
			return nil, internalAPIError(err)
		}
		nodes = append(nodes, api.ReachableGraphNode{
			Issue:       issue,
			QualifiedID: qualifiedID(project.Name, issue.ShortID),
		})
	}
	slices.SortFunc(nodes, api.ReachableGraphNode.Compare)

	links, err := w.linksForReached(ctx, dist)
	if err != nil {
		return nil, internalAPIError(err)
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
	if w.scoped {
		if _, ok := w.allowed[issueID]; !ok {
			w.hidden[issueID] = struct{}{}
			return db.Issue{}, false, nil
		}
	}
	issue, err := w.store.IssueByID(ctx, issueID)
	if errors.Is(err, db.ErrNotFound) {
		return db.Issue{}, false, nil
	}
	if err != nil {
		return db.Issue{}, false, err
	}
	ctx, err = authorizeHostProjectScope(ctx, []int64{issue.ProjectID}, nil, false)
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
	db.RecordIssueScopeTarget(ctx, issueID)
	return issue, true, nil
}

func (w *graphWalk) project(ctx context.Context, projectID int64) (db.Project, error) {
	if project, ok := w.projectByID[projectID]; ok {
		return project, nil
	}
	ctx, err := authorizeHostProjectScope(ctx, []int64{projectID}, nil, false)
	if err != nil {
		return db.Project{}, err
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

// markTransitiveBlockLayout expects edges deduplicated by kind and endpoints,
// as edgesAndUnresolved provides. Keep in sync with markTransitiveBlockLayout
// in web/src/lib/kata/projection.ts.
func markTransitiveBlockLayout(edges []api.ReachableGraphEdge) {
	if len(edges) == 0 {
		return
	}
	ids := map[string]int{}
	idOf := func(uid string) int {
		if id, ok := ids[uid]; ok {
			return id
		}
		id := len(ids)
		ids[uid] = id
		return id
	}
	blocks := []int{}
	var ends [][2]int
	for i, edge := range edges {
		if edge.Kind == "blocks" {
			blocks = append(blocks, i)
			ends = append(ends, [2]int{idOf(edge.FromUID), idOf(edge.ToUID)})
		}
	}
	count := len(ids)
	targets := make([][]int, count)
	for _, end := range ends {
		targets[end[0]] = append(targets[end[0]], end[1])
	}

	// Iterative Tarjan traversal numbers strongly connected components sinks-first.
	componentOf, order := make([]int, count), make([]int, count)
	low, cursor := make([]int, count), make([]int, count)
	for node := range count {
		componentOf[node], order[node] = -1, -1
	}
	var stack []int
	var groups [][]int
	visited := 0
	for root := range targets {
		if order[root] >= 0 {
			continue
		}
		path := []int{root}
		order[root], low[root] = visited, visited
		visited++
		stack = append(stack, root)
		for len(path) > 0 {
			node := path[len(path)-1]
			if cursor[node] < len(targets[node]) {
				child := targets[node][cursor[node]]
				cursor[node]++
				if order[child] < 0 {
					order[child], low[child] = visited, visited
					visited++
					stack = append(stack, child)
					path = append(path, child)
				} else if componentOf[child] < 0 {
					low[node] = min(low[node], order[child])
				}
				continue
			}
			path = path[:len(path)-1]
			if len(path) > 0 {
				parent := path[len(path)-1]
				low[parent] = min(low[parent], low[node])
			}
			if low[node] == order[node] {
				var group []int
				for {
					member := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					componentOf[member] = len(groups)
					group = append(group, member)
					if member == node {
						break
					}
				}
				groups = append(groups, group)
			}
		}
	}

	// Only components with multiple incoming links can be hidden targets.
	incoming := make([]int, len(groups))
	for node := range count {
		for _, to := range targets[node] {
			if componentOf[to] != componentOf[node] {
				incoming[componentOf[to]]++
			}
		}
	}
	bit := make([]int, len(groups))
	bits := 0
	for component := range groups {
		bit[component] = -1
		if incoming[component] > 1 {
			bit[component] = bits
			bits++
		}
	}
	words := (bits + 63) / 64
	reach := make([][]uint64, len(groups))
	absorb := func(row []uint64, component int) {
		if slot := bit[component]; slot >= 0 {
			row[slot>>6] |= uint64(1) << (slot & 63)
		}
		for word := range words {
			row[word] |= reach[component][word]
		}
	}
	hidden := map[int]bool{}
	for component, group := range groups {
		row := make([]uint64, words)
		reach[component] = row
		if len(group) > 1 {
			for _, member := range group {
				for _, target := range targets[member] {
					if componentOf[target] != component {
						absorb(row, componentOf[target])
					}
				}
			}
			continue
		}
		// Acyclic sources cannot lie on a path from one target to another.
		// Read nearest targets first, then hide targets already in the row.
		node := group[0]
		near := slices.Clone(targets[node])
		slices.SortFunc(near, func(a, b int) int { return componentOf[b] - componentOf[a] })
		for index, target := range near {
			targetComponent := componentOf[target]
			if slot := bit[targetComponent]; slot >= 0 {
				if row[slot>>6]&(uint64(1)<<(slot&63)) != 0 {
					hidden[node*count+target] = true
				}
				if index > 0 && componentOf[near[index-1]] == targetComponent {
					hidden[node*count+near[index-1]] = true
				}
			}
			absorb(row, targetComponent)
		}
	}

	seen, first, second := make([]int, count), make([]int, count), make([]int, count)
	queue, origins := make([]int, 2*count), make([]int, 2*count)
	for node := range count {
		seen[node] = -1
	}
	for _, group := range groups {
		if len(group) == 1 {
			continue
		}
		// Cyclic sources need paths that never return through the source.
		// Two distinct starting targets per task suffice to detect alternate paths.
		for _, source := range group {
			if len(targets[source]) < 2 {
				continue
			}
			pending := slices.Clone(targets[source])
			slices.SortFunc(pending, func(a, b int) int { return componentOf[a] - componentOf[b] })
			remaining, lowest := len(pending), 0
			floor := componentOf[pending[lowest]]
			tail := 0
			for _, target := range pending {
				seen[target], first[target], second[target] = source, target, -1
				origins[tail], queue[tail] = target, target
				tail++
			}
			for head := 0; head < tail && remaining > 0; head++ {
				current := queue[head]
				if componentOf[current] < floor {
					continue
				}
				for _, target := range targets[current] {
					if target == source || componentOf[target] < floor {
						continue
					}
					origin := origins[head]
					if seen[target] != source {
						seen[target], first[target], second[target] = source, origin, -1
					} else if first[target] == origin || second[target] != -1 {
						continue
					} else {
						second[target] = origin
						if first[target] == target {
							hidden[source*count+target] = true
							remaining--
							if remaining == 0 {
								break
							}
							// Hidden targets no longer require exploring lower components.
							for hidden[source*count+pending[lowest]] {
								lowest++
							}
							floor = componentOf[pending[lowest]]
						}
					}
					origins[tail], queue[tail] = origin, target
					tail++
				}
			}
		}
	}
	for index, end := range ends {
		if hidden[end[0]*count+end[1]] {
			edges[blocks[index]].Layout = false
		}
	}
}
