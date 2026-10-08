package commentref

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
)

// Record adds read-time annotations to a persisted comment. UIDs are canonical;
// handles and issue short IDs are computed afresh for each authorized view.
type Record = CommentOut

// CommentOut carries the current issue identity, handle, and visible edges.
type CommentOut struct {
	db.Comment
	IssueUID           string `json:"issue_uid,omitempty"`
	IssueShortID       string `json:"issue_short_id,omitempty"`
	ProjectUID         string `json:"project_uid,omitempty"`
	ProjectName        string `json:"project_name,omitempty"`
	ProjectID          int64  `json:"project_id,omitempty"`
	Handle             string `json:"handle,omitempty"`
	Reply              *Link  `json:"reply,omitempty"`
	Backlinks          []Link `json:"backlinks,omitempty"`
	BacklinksTruncated bool   `json:"backlinks_truncated,omitempty"`
}

// Link describes one visible reply target or backlink.
type Link = CommentLink

// CommentLink retains canonical endpoint identities for client navigation.
type CommentLink struct {
	UID          string     `json:"uid,omitempty"`
	Handle       string     `json:"handle,omitempty"`
	Kind         string     `json:"kind"`
	Author       string     `json:"author,omitempty"`
	Teammate     string     `json:"teammate,omitempty"`
	IssueUID     string     `json:"issue_uid,omitempty"`
	IssueShortID string     `json:"issue_short_id,omitempty"`
	ProjectUID   string     `json:"project_uid,omitempty"`
	ProjectID    int64      `json:"project_id,omitempty"`
	Status       string     `json:"status,omitempty"`
	TargetEdited bool       `json:"target_edited,omitempty"`
	Body         string     `json:"body,omitempty"`
	CreatedAt    time.Time  `json:"created_at,omitzero"`
	EditedAt     *time.Time `json:"edited_at,omitempty"`
}

// TargetState is supplied only after authorization and lookup of unavailable
// endpoints. Missing entries mean pending replication; hidden entries redact
// the immutable edge itself. Removed requires purge evidence, moved a live UID.
type TargetState struct {
	Status string
	Target *Record
}

// Project annotates an authorized graph with handles, reply lines, and backlinks.
func Project(records []Record, states map[string]TargetState) []Record {
	uids := make([]string, len(records))
	byUID := make(map[string]int, len(records))
	for i, r := range records {
		uids[i] = r.UID
		byUID[r.UID] = i
	}
	handles := Handles(uids)
	result := slices.Clone(records)
	for i := range result {
		result[i].Handle = "c:" + handles[result[i].UID]
		result[i].Backlinks = []Link{}
		result[i].Reply = nil
	}
	for i, r := range records {
		if r.ReplyToUID == "" {
			continue
		}
		if targetIndex, ok := byUID[r.ReplyToUID]; ok {
			target := records[targetIndex]
			link := recordLink(target, r.ReplyKind, handles[target.UID], r.IssueUID)
			link.TargetEdited = target.EditedAt != nil && target.EditedAt.After(r.CreatedAt)
			result[i].Reply = &link
			backlink := recordLink(r, r.ReplyKind, handles[r.UID], target.IssueUID)
			backlink.TargetEdited = link.TargetEdited
			result[targetIndex].Backlinks = append(result[targetIndex].Backlinks, backlink)
			continue
		}
		state, ok := states[r.ReplyToUID]
		if state.Status == "hidden" {
			result[i].ReplyToUID = ""
			result[i].ReplyKind = ""
			continue
		}
		if !ok || state.Status == "" {
			state.Status = "pending"
		}
		link := Link{UID: r.ReplyToUID, Kind: r.ReplyKind, Status: state.Status}
		if state.Target != nil {
			suffix := strings.TrimPrefix(state.Target.Handle, "c:")
			if suffix == "" {
				suffix = strings.ToLower(state.Target.UID)
			}
			link = recordLink(*state.Target, r.ReplyKind, suffix, r.IssueUID)
			if state.Status == "moved" && state.Target.ProjectName != "" {
				link.Handle = state.Target.ProjectName + "#" + link.Handle
			}
			link.Status = state.Status
			link.TargetEdited = state.Target.EditedAt != nil && state.Target.EditedAt.After(r.CreatedAt)
		}
		result[i].Reply = &link
	}
	for i := range result {
		slices.SortFunc(result[i].Backlinks, func(a, b Link) int { return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.UID, b.UID)) })
		if len(result[i].Backlinks) <= 50 {
			continue
		}
		counts := make(map[string]int, 4)
		retained := make([]Link, 0, min(len(result[i].Backlinks), 200))
		for _, link := range slices.Backward(result[i].Backlinks) {
			if counts[link.Kind] == 50 {
				result[i].BacklinksTruncated = true
				continue
			}
			counts[link.Kind]++
			retained = append(retained, link)
		}
		slices.Reverse(retained)
		result[i].Backlinks = retained
	}
	return result
}

func recordLink(r Record, kind, suffix, sourceIssue string) Link {
	handle := "c:" + suffix
	if r.IssueUID != sourceIssue {
		handle = r.IssueShortID + ":" + suffix
	}
	return Link{Body: r.Body, CreatedAt: r.CreatedAt, EditedAt: r.EditedAt, UID: r.UID, Handle: handle, Kind: kind, Author: r.Author, Teammate: r.Teammate, IssueUID: r.IssueUID, IssueShortID: r.IssueShortID, ProjectUID: r.ProjectUID, ProjectID: r.ProjectID}
}

var (
	// ErrNotFound reports a reference absent from the caller's visible graph.
	ErrNotFound = errors.New("comment not found")
	// ErrAmbiguous reports a suffix matching multiple visible comments.
	ErrAmbiguous = errors.New("comment reference is ambiguous")
)

// Resolve finds a visible comment by a handle or full UID.
func Resolve(records []Record, issueUID, input, project string) (Record, error) {
	ref, err := Parse(input)
	if err != nil {
		return Record{}, err
	}
	if ref.Project != "" && ref.Project != project {
		return Record{}, ErrNotFound
	}
	var found *Record
	for _, r := range records {
		if ref.UID != "" {
			if !strings.EqualFold(r.UID, ref.UID) {
				continue
			}
		} else {
			if !strings.HasSuffix(strings.ToLower(r.UID), ref.Suffix) {
				continue
			}
			if ref.IssueRef != "" {
				if !strings.EqualFold(r.IssueShortID, ref.IssueRef) {
					continue
				}
			} else if r.IssueUID != issueUID {
				continue
			}
		}
		if found != nil && found.UID != r.UID {
			return Record{}, ErrAmbiguous
		}
		candidate := r
		found = &candidate
	}
	if found == nil {
		return Record{}, ErrNotFound
	}
	return *found, nil
}

// Options selects a thread or inbound replies and optional kind/since filters.
type Options struct {
	Thread  string
	Inbound string
	Kind    string
	Since   string
}

// Selection contains ordered visible comments and the thread truncation marker.
type Selection struct {
	Comments  []Record
	Truncated bool
}

// Select filters an authorized graph and makes its handles usable from issueUID.
func Select(records []Record, issueUID string, opts Options) (Selection, error) {
	if opts.Thread != "" && opts.Inbound != "" {
		return Selection{}, fmt.Errorf("thread and inbound are mutually exclusive")
	}
	if opts.Kind != "" {
		if opts.Thread == "" && opts.Inbound == "" {
			return Selection{}, fmt.Errorf("kind requires thread or inbound")
		}
		if !ValidKind(opts.Kind) {
			return Selection{}, fmt.Errorf("invalid reply kind %q", opts.Kind)
		}
	}
	project := ""
	if len(records) > 0 {
		project = records[0].ProjectName
	}
	for _, r := range records {
		if r.IssueUID == issueUID {
			project = r.ProjectName
			break
		}
	}
	var since *Record
	if opts.Since != "" {
		r, err := Resolve(records, issueUID, opts.Since, project)
		if err != nil {
			return Selection{}, err
		}
		since = &r
	}
	selected := map[string]bool{}
	rootUID := ""
	if opts.Thread != "" {
		root, err := Resolve(records, issueUID, opts.Thread, project)
		if err != nil {
			return Selection{}, err
		}
		rootUID = root.UID
		children := make(map[string][]string)
		for _, r := range records {
			if r.ReplyToUID != "" {
				children[r.ReplyToUID] = append(children[r.ReplyToUID], r.UID)
			}
		}
		queue := []string{root.UID}
		for len(queue) > 0 {
			id := queue[0]
			queue = queue[1:]
			if selected[id] {
				continue
			}
			selected[id] = true
			queue = append(queue, children[id]...)
		}
	} else if opts.Inbound != "" {
		actor, tm, hasTM := strings.Cut(opts.Inbound, "/")
		if actor == "" || (hasTM && tm == "") || strings.Contains(tm, "/") {
			return Selection{}, fmt.Errorf("invalid inbound actor/teammate")
		}
		targets := map[string]bool{}
		for _, r := range records {
			if r.IssueUID == issueUID && r.Author == actor && (!hasTM || r.Teammate == tm) {
				targets[r.UID] = true
			}
		}
		for _, r := range records {
			if targets[r.ReplyToUID] {
				selected[r.UID] = true
			}
		}
	} else {
		for _, r := range records {
			if r.IssueUID == issueUID {
				selected[r.UID] = true
			}
		}
	}
	result := Selection{Comments: []Record{}}
	for _, r := range records {
		if !selected[r.UID] || (opts.Kind != "" && r.UID != rootUID && r.ReplyKind != opts.Kind) {
			continue
		}
		if since != nil && compareRecords(r, *since) <= 0 {
			continue
		}
		r.Handle = HandleForIssue(r.Handle, r.IssueUID, r.IssueShortID, issueUID)
		if r.Reply != nil {
			link := *r.Reply
			link.Handle = HandleForIssue(link.Handle, link.IssueUID, link.IssueShortID, issueUID)
			r.Reply = &link
		}
		r.Backlinks = slices.Clone(r.Backlinks)
		for i := range r.Backlinks {
			link := &r.Backlinks[i]
			link.Handle = HandleForIssue(link.Handle, link.IssueUID, link.IssueShortID, issueUID)
		}
		result.Comments = append(result.Comments, r)
	}
	slices.SortFunc(result.Comments, compareRecords)
	if opts.Thread != "" && len(result.Comments) > 50 {
		// Federation can deliver replies whose timestamps precede the root.
		// Retain that root when limiting, then restore chronological order.
		for _, r := range result.Comments[50:] {
			if r.UID == rootUID {
				result.Comments[49] = r
				slices.SortFunc(result.Comments[:50], compareRecords)
				break
			}
		}
		result.Comments = result.Comments[:50]
		result.Truncated = true
	}
	return result, nil
}

// HandleForIssue makes a projected handle usable from the issue being shown.
// Moved targets retain their explicit project qualification.
func HandleForIssue(handle, endpointIssue, shortID, shownIssue string) string {
	if handle == "" || strings.Contains(handle, "#") {
		return handle
	}
	colon := strings.LastIndexByte(handle, ':')
	if colon < 0 {
		return handle
	}
	suffix := handle[colon+1:]
	if endpointIssue != "" && endpointIssue != shownIssue && shortID != "" {
		return shortID + ":" + suffix
	}
	return "c:" + suffix
}

// ValidKind reports whether kind names a supported typed reply.
func ValidKind(kind string) bool {
	return kind == "reply" || kind == "confirm" || kind == "refute" || kind == "supersede"
}
func compareRecords(a, b Record) int {
	return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.UID, b.UID))
}
