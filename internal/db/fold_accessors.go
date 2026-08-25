package db

import "sort"

// SortedIssues returns every folded issue in ascending UID order. Federated
// short_id assignment consumes this order, so it is part of the materialized
// result, not a presentation detail.
func (p FoldProjection) SortedIssues() []FoldIssue {
	uids := make([]string, 0, len(p.Issues))
	for issueUID := range p.Issues {
		uids = append(uids, issueUID)
	}
	sort.Strings(uids)
	issues := make([]FoldIssue, 0, len(uids))
	for _, issueUID := range uids {
		issues = append(issues, p.Issues[issueUID])
	}
	return issues
}

// SortedComments returns every folded comment in ascending UID order.
func (p FoldProjection) SortedComments() []FoldComment {
	uids := make([]string, 0, len(p.Comments))
	for commentUID := range p.Comments {
		uids = append(uids, commentUID)
	}
	sort.Strings(uids)
	comments := make([]FoldComment, 0, len(uids))
	for _, commentUID := range uids {
		comments = append(comments, p.Comments[commentUID])
	}
	return comments
}

// PresentLabels returns the label edges that survive their tombstones,
// ordered by issue UID then label. Skipping removed edges is the invariant
// that keeps deleted labels out of a materialized project.
func (p FoldProjection) PresentLabels() []FoldLabelKey {
	keys := make([]FoldLabelKey, 0, len(p.Labels))
	for key, state := range p.Labels {
		if state.Present {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].IssueUID != keys[j].IssueUID {
			return keys[i].IssueUID < keys[j].IssueUID
		}
		return keys[i].Label < keys[j].Label
	})
	return keys
}

// PresentLinks returns the link edges that survive their tombstones, ordered
// by from-UID, to-UID, then type. The state travels with the key because
// backends record the edge author from it.
func (p FoldProjection) PresentLinks() []FoldLinkEdge {
	edges := make([]FoldLinkEdge, 0, len(p.Links))
	for key, state := range p.Links {
		if state.Present {
			edges = append(edges, FoldLinkEdge{Key: key, State: state})
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].Key.FromUID != edges[j].Key.FromUID {
			return edges[i].Key.FromUID < edges[j].Key.FromUID
		}
		if edges[i].Key.ToUID != edges[j].Key.ToUID {
			return edges[i].Key.ToUID < edges[j].Key.ToUID
		}
		return edges[i].Key.Type < edges[j].Key.Type
	})
	return edges
}
