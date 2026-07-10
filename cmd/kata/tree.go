package main

// treeRows reorders rows into parent/child tree order for human `kata
// list` output, setting each row's TreePrefix to bd-style box-drawing
// connectors ("├─ " / "└─ ", with "│  " / "   " continuation rails for
// deeper levels). keys[i] identifies rows[i] and parents[i] names its
// parent ("" for none) — callers pass qualified ids for both, since
// parent links can cross projects and a bare short_id is only unique
// within one project (a foreign parent's short_id could collide with a
// local issue).
//
// Roots keep their input order. A child whose parent is not in the
// fetched set (the parent didn't match the active filter, lives in
// another project, or --limit truncated it away) is rendered flat at
// top level rather than dropped. Parent chains nest recursively; a
// visited guard keeps a (never expected) parent cycle from looping or
// dropping rows — cycle members degrade to flat top-level rows.
func treeRows(rows []issueRow, keys, parents []string) []issueRow {
	index := make(map[string]int, len(rows))
	for i := range rows {
		index[keys[i]] = i
	}
	childrenOf := make(map[string][]int)
	out := make([]issueRow, 0, len(rows))
	visited := make([]bool, len(rows))

	var emit func(i int, prefix string, rail string)
	emit = func(i int, prefix, rail string) {
		visited[i] = true
		row := rows[i]
		row.TreePrefix = prefix
		out = append(out, row)
		kids := childrenOf[keys[i]]
		for k, child := range kids {
			if visited[child] {
				continue
			}
			connector, childRail := "├─ ", rail+"│  "
			if k == len(kids)-1 {
				connector, childRail = "└─ ", rail+"   "
			}
			emit(child, rail+connector, childRail)
		}
	}

	// A row is a child only if its parent is present in the fetched set;
	// otherwise it stays a top-level (flat) row.
	isChild := make([]bool, len(rows))
	for i, parent := range parents {
		if parent == "" {
			continue
		}
		if _, ok := index[parent]; ok {
			isChild[i] = true
			childrenOf[parent] = append(childrenOf[parent], i)
		}
	}
	for i := range rows {
		if !isChild[i] && !visited[i] {
			emit(i, "", "")
		}
	}
	// Cycle fallback: rows reachable only through a parent cycle were
	// never emitted above; render them flat so nothing is dropped.
	for i := range rows {
		if !visited[i] {
			emit(i, "", "")
		}
	}
	return out
}
