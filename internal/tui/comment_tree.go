package tui

import (
	"cmp"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/commentref"
)

const maxCommentDepth = 4

type commentLeaf struct {
	link  commentref.Link
	depth int
	tags  string
}

func compareCommentLinks(a, b commentref.Link) int {
	return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.UID, b.UID))
}

// threadOrder nests local replies and foreign evidence in time order, keeping
// roots in input order and retaining malformed cycles once.
func threadOrder(cs []CommentEntry) []CommentEntry {
	byUID := make(map[string]int, len(cs))
	children := make(map[string][]commentref.Link, len(cs))
	for i, c := range cs {
		children[c.UID] = nil
		if c.UID != "" {
			byUID[c.UID] = i
		}
	}
	for _, c := range cs {
		for _, link := range c.Backlinks {
			if _, local := byUID[link.UID]; !local {
				children[c.UID] = append(children[c.UID], link)
				children[link.UID] = nil
			}
		}
	}
	var roots []int
	for i, c := range cs {
		parent, visible := "", false
		if c.Reply != nil {
			parent = c.Reply.UID
			_, visible = children[parent]
		}
		if visible {
			children[parent] = append(children[parent], commentref.Link{UID: c.UID, CreatedAt: c.CreatedAt, Kind: c.Reply.Kind, TargetEdited: c.Reply.TargetEdited})
		} else {
			roots = append(roots, i)
		}
	}
	for _, links := range children {
		slices.SortStableFunc(links, compareCommentLinks)
	}
	ordered := make([]CommentEntry, 0, len(cs))
	visited := make([]bool, len(cs))
	var visit func(int, int)
	visit = func(i, depth int) {
		if visited[i] {
			return
		}
		visited[i] = true
		c := cs[i]
		c.depth, c.leaves = depth, nil
		ordered = append(ordered, c)
		for _, link := range children[c.UID] {
			childDepth := min(depth+1, maxCommentDepth)
			if child, local := byUID[link.UID]; local {
				visit(child, childDepth)
			} else {
				last := &ordered[len(ordered)-1]
				last.leaves = append(last.leaves, commentLeaf{link: link, depth: childDepth, tags: commentTags(CommentEntry{Backlinks: children[link.UID]})})
				for _, child := range children[link.UID] {
					visit(byUID[child.UID], min(childDepth+1, maxCommentDepth))
				}
			}
		}
	}
	for _, root := range roots {
		visit(root, 0)
	}
	for i := range cs {
		visit(i, 0)
	}
	return ordered
}

func commentTags(c CommentEntry) string {
	var tags []string
	for _, kind := range []string{"confirm", "refute"} {
		present, fresh := false, false
		for _, link := range c.Backlinks {
			if link.Kind == kind {
				present = true
				fresh = fresh || !link.TargetEdited
			}
		}
		if !present {
			continue
		}
		tag := kind
		// Assumes projection retains the newest replies per kind, so omitted
		// replies also predate the edit when every retained reply does.
		if !fresh {
			tag += " (before edit)"
		}
		if kind == "confirm" {
			tag = openStyle.Render(tag)
		} else {
			tag = errorStyle.Render(tag)
		}
		tags = append(tags, tag)
	}
	return strings.Join(tags, " · ")
}
