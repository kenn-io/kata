package tui

import "sort"

type childSortMode int

const (
	childSortTopological childSortMode = iota
	childSortTemporal
)

func (m childSortMode) label() string {
	if m == childSortTemporal {
		return "temporal"
	}
	return "topological"
}

type issueListViewMode int

const (
	issueListViewNested issueListViewMode = iota
	issueListViewFlat
)

func (m issueListViewMode) label() string {
	if m == issueListViewFlat {
		return "flat"
	}
	return "nested"
}

// issueKey is the (project_id, short_id) tuple used as a map key for
// queue rendering and expansion state. short_id is the project-scoped
// display ref; the daemon guarantees uniqueness within a project so
// the tuple is collision-free.
type issueKey struct {
	projectID int64
	shortID   string
}

// issueKeysByUID indexes the loaded issues' keys by issue UID, for resolving
// parent peers. Issues without a UID (a daemon predating the field) are not
// indexed; their children render top-level.
func issueKeysByUID(issues []Issue) map[string]issueKey {
	keys := make(map[string]issueKey, len(issues))
	for _, iss := range issues {
		if iss.UID == "" {
			continue
		}
		keys[iss.UID] = issueKey{projectID: iss.ProjectID, shortID: iss.ShortID}
	}
	return keys
}

// parentKeyIn returns the key of iss's parent when that parent is among the
// indexed issues, matched by UID. Parents may live in another project, so a
// short_id match inside the child's project would be wrong twice over: it
// misses the real parent and can capture an unrelated issue that reuses the
// short_id.
func parentKeyIn(iss Issue, keysByUID map[string]issueKey) (issueKey, bool) {
	if iss.Parent == nil || iss.Parent.UID == "" {
		return issueKey{}, false
	}
	key, ok := keysByUID[iss.Parent.UID]
	return key, ok
}

type queueRow struct {
	issue       Issue
	key         issueKey
	depth       int
	hasChildren bool
	expanded    bool
	context     bool
	lastChild   bool
}

type expansionSet map[issueKey]bool

func buildQueueRows(issues []Issue, filter ListFilter, expanded expansionSet) []queueRow {
	return buildQueueRowsWithSort(issues, filter, expanded, childSortTopological)
}

func buildQueueRowsWithSort(
	issues []Issue, filter ListFilter, expanded expansionSet, childSort childSortMode,
) []queueRow {
	return buildQueueRowsWithView(issues, filter, expanded, childSort, issueListViewNested)
}

func buildQueueRowsWithView(
	issues []Issue,
	filter ListFilter,
	expanded expansionSet,
	childSort childSortMode,
	viewMode issueListViewMode,
) []queueRow {
	if viewMode == issueListViewFlat {
		return buildFlatQueueRows(issues, filter)
	}
	state := newQueueBuildState(issues, filter, expanded, childSort)
	for _, key := range state.order {
		iss := state.byKey[key]
		if _, ok := state.parentKey(iss); ok {
			continue
		}
		state.appendNode(key, 0, false, nil)
	}
	for _, key := range state.order {
		if state.emitted[key] || !state.included[key] {
			continue
		}
		if state.hasIncludedParent(key) {
			continue
		}
		state.appendNode(key, 0, false, nil)
	}
	return state.rows
}

func buildFlatQueueRows(issues []Issue, filter ListFilter) []queueRow {
	filterActive := hasActiveQueueFilter(filter)
	rows := make([]queueRow, 0, len(issues))
	for _, iss := range issues {
		if filterActive && !matchesFilter(iss, filter) {
			continue
		}
		key := issueKey{projectID: iss.ProjectID, shortID: iss.ShortID}
		rows = append(rows, queueRow{issue: iss, key: key})
	}
	return rows
}

type queueBuildState struct {
	byKey            map[issueKey]Issue
	keysByUID        map[string]issueKey
	childrenByParent map[issueKey][]issueKey
	order            []issueKey
	filter           ListFilter
	filterActive     bool
	revealMatches    bool
	expanded         expansionSet
	childSort        childSortMode
	matched          map[issueKey]bool
	included         map[issueKey]bool
	emitted          map[issueKey]bool
	rows             []queueRow
}

func newQueueBuildState(
	issues []Issue, filter ListFilter, expanded expansionSet, childSort childSortMode,
) *queueBuildState {
	state := &queueBuildState{
		byKey:            make(map[issueKey]Issue, len(issues)),
		keysByUID:        issueKeysByUID(issues),
		childrenByParent: make(map[issueKey][]issueKey),
		order:            make([]issueKey, 0, len(issues)),
		filter:           filter,
		filterActive:     hasActiveQueueFilter(filter),
		revealMatches:    hasRevealQueueFilter(filter),
		expanded:         expanded,
		childSort:        childSort,
		matched:          make(map[issueKey]bool, len(issues)),
		included:         make(map[issueKey]bool, len(issues)),
		emitted:          make(map[issueKey]bool, len(issues)),
	}
	for _, iss := range issues {
		key := issueKey{projectID: iss.ProjectID, shortID: iss.ShortID}
		state.byKey[key] = iss
		state.order = append(state.order, key)
	}
	for _, key := range state.order {
		iss := state.byKey[key]
		if pk, ok := state.parentKey(iss); ok {
			state.childrenByParent[pk] = append(state.childrenByParent[pk], key)
		}
	}
	state.computeIncluded()
	return state
}

func (s *queueBuildState) computeIncluded() {
	if !s.filterActive {
		return
	}
	for _, key := range s.order {
		iss := s.byKey[key]
		if !matchesFilter(iss, s.filter) {
			continue
		}
		s.matched[key] = true
		s.included[key] = true
	}
	for _, key := range s.order {
		if !s.matched[key] {
			continue
		}
		if s.revealMatches {
			s.includeAncestors(key)
		} else {
			s.includeAncestorsWhenTheyConnectToMatchedAncestor(key)
		}
	}
}

// parentKey returns the in-list key of iss's parent (matched by UID) and
// ok=true; ok=false when the issue has no parent or the parent is not among
// the loaded issues.
func (s *queueBuildState) parentKey(iss Issue) (issueKey, bool) {
	return parentKeyIn(iss, s.keysByUID)
}

func (s *queueBuildState) includeAncestors(key issueKey) {
	seen := map[issueKey]bool{key: true}
	for {
		iss := s.byKey[key]
		pk, ok := s.parentKey(iss)
		if !ok || seen[pk] {
			return
		}
		s.included[pk] = true
		seen[pk] = true
		key = pk
	}
}

func (s *queueBuildState) includeAncestorsWhenTheyConnectToMatchedAncestor(key issueKey) {
	path := []issueKey{}
	seen := map[issueKey]bool{key: true}
	for {
		iss := s.byKey[key]
		pk, ok := s.parentKey(iss)
		if !ok || seen[pk] {
			return
		}
		path = append(path, pk)
		if s.matched[pk] {
			for _, ancestor := range path {
				s.included[ancestor] = true
			}
			return
		}
		seen[pk] = true
		key = pk
	}
}

func (s *queueBuildState) appendNode(key issueKey, depth int, lastChild bool, seenPath map[issueKey]bool) {
	if s.filterActive && !s.included[key] {
		return
	}
	if seenPath == nil {
		seenPath = map[issueKey]bool{}
	}
	if seenPath[key] {
		return
	}
	seenPath[key] = true
	iss := s.byKey[key]
	hasChildren := len(s.childrenByParent[key]) > 0
	isExpanded := s.expanded != nil && s.expanded[key]
	if s.shouldAutoExpand(key) {
		isExpanded = true
	}
	s.rows = append(s.rows, queueRow{
		issue:       iss,
		key:         key,
		depth:       depth,
		hasChildren: hasChildren,
		expanded:    isExpanded,
		context:     s.filterActive && s.included[key] && !s.matched[key],
		lastChild:   lastChild,
	})
	s.emitted[key] = true
	childKeys := s.visibleChildKeys(key, isExpanded)
	for i, childKey := range childKeys {
		nextSeen := make(map[issueKey]bool, len(seenPath)+1)
		for seenKey, seen := range seenPath {
			nextSeen[seenKey] = seen
		}
		s.appendNode(childKey, depth+1, i == len(childKeys)-1, nextSeen)
	}
}

func (s *queueBuildState) shouldAutoExpand(key issueKey) bool {
	if !s.filterActive || len(s.visibleChildKeys(key, true)) == 0 {
		return false
	}
	if s.revealMatches {
		return true
	}
	if s.matched[key] {
		for _, childKey := range s.visibleChildKeys(key, true) {
			if !s.matched[childKey] {
				return true
			}
		}
		return false
	}
	return !s.matched[key]
}

func (s *queueBuildState) visibleChildKeys(parent issueKey, expanded bool) []issueKey {
	children := s.childrenByParent[parent]
	if len(children) == 0 {
		return nil
	}
	if !s.filterActive {
		if !expanded {
			return nil
		}
		if s.childSort == childSortTopological {
			children = s.topologicalChildKeys(children)
		}
		return children
	}
	if !expanded {
		return nil
	}
	out := make([]issueKey, 0, len(children))
	for _, child := range children {
		if s.included[child] {
			out = append(out, child)
		}
	}
	if s.childSort == childSortTopological {
		out = s.topologicalChildKeys(out)
	}
	return out
}

func (s *queueBuildState) topologicalChildKeys(children []issueKey) []issueKey {
	if len(children) < 2 {
		return children
	}
	index := make(map[issueKey]int, len(children))
	shortIDSiblings := make(map[string][]issueKey, len(children))
	for i, key := range children {
		index[key] = i
		shortIDSiblings[key.shortID] = append(shortIDSiblings[key.shortID], key)
	}
	// resolveBlocked maps a Blocks peer to a sibling key. The UID match is
	// authoritative — siblings may span projects, so two of them can share a
	// short_id. A peer without a UID (a daemon predating the field) falls
	// back to short_id only while exactly one sibling carries it.
	resolveBlocked := func(peer LinkPeer) (issueKey, bool) {
		if peer.UID != "" {
			key, ok := s.keysByUID[peer.UID]
			if !ok {
				return issueKey{}, false
			}
			_, isSibling := index[key]
			return key, isSibling
		}
		siblings := shortIDSiblings[peer.ShortID]
		if len(siblings) != 1 {
			return issueKey{}, false
		}
		return siblings[0], true
	}
	outgoing := make(map[issueKey][]issueKey, len(children))
	indegree := make(map[issueKey]int, len(children))
	for _, key := range children {
		for _, blocked := range s.byKey[key].Blocks {
			blockedKey, ok := resolveBlocked(blocked)
			if !ok || blockedKey == key {
				continue
			}
			outgoing[key] = append(outgoing[key], blockedKey)
			indegree[blockedKey]++
		}
	}
	for key := range outgoing {
		sort.SliceStable(outgoing[key], func(i, j int) bool {
			return index[outgoing[key][i]] < index[outgoing[key][j]]
		})
	}
	ready := make([]issueKey, 0, len(children))
	for _, key := range children {
		if indegree[key] == 0 {
			ready = append(ready, key)
		}
	}
	sorted := make([]issueKey, 0, len(children))
	emitted := make(map[issueKey]bool, len(children))
	for len(ready) > 0 {
		key := ready[0]
		ready = ready[1:]
		if emitted[key] {
			continue
		}
		sorted = append(sorted, key)
		emitted[key] = true
		for _, blockedKey := range outgoing[key] {
			indegree[blockedKey]--
			if indegree[blockedKey] == 0 {
				ready = insertReadyByOriginalOrder(ready, blockedKey, index)
			}
		}
	}
	if len(sorted) == len(children) {
		return sorted
	}
	for _, key := range children {
		if !emitted[key] {
			sorted = append(sorted, key)
		}
	}
	return sorted
}

func insertReadyByOriginalOrder(
	ready []issueKey, key issueKey, index map[issueKey]int,
) []issueKey {
	pos := sort.Search(len(ready), func(i int) bool {
		return index[ready[i]] > index[key]
	})
	ready = append(ready, issueKey{})
	copy(ready[pos+1:], ready[pos:])
	ready[pos] = key
	return ready
}

func (s *queueBuildState) hasIncludedParent(key issueKey) bool {
	pk, ok := s.parentKey(s.byKey[key])
	return ok && s.included[pk]
}

func hasActiveQueueFilter(f ListFilter) bool {
	return f.Status != "" || f.Owner != "" || f.Author != "" || f.Search != "" || len(f.Labels) > 0
}

func hasRevealQueueFilter(f ListFilter) bool {
	return f.Owner != "" || f.Author != "" || f.Search != "" || len(f.Labels) > 0
}
