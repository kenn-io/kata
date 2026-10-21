package db

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"strings"
	"time"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/shortid"
	"go.kenn.io/kata/internal/uid"
)

var (
	// ErrFederationPublicationConflict indicates that the operation conflicts with existing destination state.
	ErrFederationPublicationConflict = errors.New("federation publication conflicts with existing state")
	// ErrFederationPublicationInvalid indicates that the closed document violates the publication contract.
	ErrFederationPublicationInvalid = errors.New("invalid federation publication")
	// ErrFederationPublicationTooLarge indicates that the document exceeds publication capacity.
	ErrFederationPublicationTooLarge = errors.New("federation publication exceeds capacity")
)

// MaxFederationPublicationRequestBytes allows the full envelope plus its fixed
// HTTP request wrapper. The false preflight value is the longer representation.
const MaxFederationPublicationRequestBytes = MaxRelayEnvelopeBytes + len(`{"preflight":false,"envelope":}`)

// FederationPublication is a closed, portable project document. Capture and
// decoding return owned records; callers must treat a validated document as
// immutable. No credential or source host authority belongs in this document.
type FederationPublication struct {
	OperationUID, SourceInstanceUID, ProjectUID, TargetInstanceUID string
	ProjectName, Digest                                            string
	Records                                                        []ImportRecord
}

// FederationPublicationCounts reports the issue, comment, and event totals in a publication.
type FederationPublicationCounts struct{ Issues, Comments, Events int64 }

// FederationPublicationResult describes the published project and retry state.
type FederationPublicationResult struct {
	Project      Project
	CatalogEvent *Event // nil on an exact committed retry
	Replayed     bool
}

// FederationPublicationStore atomically publishes a project and its retained history.
type FederationPublicationStore interface {
	// The actor is resolved from current server authentication, never from the
	// source document's historical authors.
	PublishFederationProject(context.Context, FederationPublication, string) (FederationPublicationResult, error)
}

// FederationPublicationSnapshotReader reads original event rows through the
// export snapshot, with no owner-export scrubbing, hash repair or visibility
// projection. Both native stores implement this optional interface.
type FederationPublicationSnapshotReader interface {
	ExportFederationPublicationEvents(context.Context, int64) iter.Seq2[EventExport, error]
	FederationPublicationCapacity() int64
	FederationPublicationCheckpointSize(context.Context, int64) (RelayCheckpointSize, error)
}

type publicationIndex struct {
	project   *ProjectExport
	issues    map[int64]*IssueExport
	issueUIDs map[string]*IssueExport
	shortIDs  map[string]*IssueExport
	comments  map[string]*CommentExport
	labels    map[FoldLabelKey]bool
	links     map[FoldLinkKey]*LinkExport
	events    []EventExport
}

// ValidateFederationPublication validates identities, the retained prefix, typed
// references and its folded projection without modifying any input record.
// The codec additionally verifies the digest against canonical wire bytes.
func ValidateFederationPublication(p FederationPublication) (FederationPublicationCounts, error) {
	counts, err := validatePublication(p)
	if err != nil {
		return FederationPublicationCounts{}, fmt.Errorf("%w: %w", ErrFederationPublicationInvalid, err)
	}
	return counts, nil
}

func validatePublication(p FederationPublication) (FederationPublicationCounts, error) {
	var counts FederationPublicationCounts
	for _, identity := range []string{p.OperationUID, p.SourceInstanceUID, p.ProjectUID, p.TargetInstanceUID} {
		if !uid.Valid(identity) {
			return counts, errors.New("invalid operation, source, project or target identity")
		}
	}
	if err := config.ValidateProjectName(p.ProjectName); err != nil {
		return counts, err
	}
	if p.ProjectName == SystemProjectName || (p.Digest != "" && !ValidReplayContentHash(p.Digest)) {
		return counts, errors.New("invalid name or digest")
	}
	if err := ValidateImportRecords(p.Records); err != nil {
		return counts, err
	}
	x := publicationIndex{issues: map[int64]*IssueExport{}, issueUIDs: map[string]*IssueExport{}, shortIDs: map[string]*IssueExport{}, comments: map[string]*CommentExport{}, labels: map[FoldLabelKey]bool{}, links: map[FoldLinkKey]*LinkExport{}}
	ids, uids := map[string]map[int64]bool{}, map[string]bool{}
	identity := func(kind string, id int64, value string) error {
		if id <= 0 || !uid.Valid(value) || uids[value] {
			return errors.New("invalid or duplicate row identity")
		}
		if ids[kind] == nil {
			ids[kind] = map[int64]bool{}
		}
		if ids[kind][id] {
			return errors.New("duplicate numeric row identity")
		}
		ids[kind][id], uids[value] = true, true
		return nil
	}
	for _, record := range p.Records {
		var err error
		switch r := record.(type) {
		case *ProjectExport:
			if x.project != nil || r.UID != p.ProjectUID || r.Name != p.ProjectName || r.DeletedAt != nil || r.Revision < 1 || !publicationObject(r.Metadata) || !publicationTime(r.CreatedAt) {
				return counts, errors.New("invalid source project")
			}
			x.project = r
			err = identity("project", r.ID, r.UID)
		case *IssueExport:
			err = identity("issue", r.ID, r.UID)
			if !shortid.Valid(r.ShortID) || !strings.HasSuffix(strings.ToLower(r.UID), r.ShortID) || x.shortIDs[r.ShortID] != nil || r.Revision < 1 || r.ContentRevision < 0 || !publicationObject(r.Metadata) || !publicationTime(r.CreatedAt) || !publicationTime(r.UpdatedAt) || (r.Status != "open" && r.Status != "closed") || strings.TrimSpace(r.Title) == "" || strings.TrimSpace(r.Author) == "" || (r.Priority != nil && (*r.Priority < 0 || *r.Priority > 4)) {
				return counts, errors.New("invalid issue projection")
			}
			if r.RecurrenceID != nil || r.RecurrenceUID != nil || r.OccurrenceKey != nil {
				return counts, errors.New("recurring issues are unsupported")
			}
			for _, stamp := range []*string{r.ClosedAt, r.DeletedAt, r.AssignmentExpiresOn} {
				if stamp != nil && !publicationTime(*stamp) {
					return counts, errors.New("invalid issue timestamp")
				}
			}
			x.issues[r.ID], x.issueUIDs[r.UID], x.shortIDs[r.ShortID] = r, r, r
			counts.Issues++
		case *CommentExport:
			err = identity("comment", r.ID, r.UID)
			if !publicationTime(r.CreatedAt) || strings.TrimSpace(r.Author) == "" {
				return counts, errors.New("invalid comment")
			}
			x.comments[r.UID] = r
			counts.Comments++
		case *IssueLabelExport:
			// Foreign keys and duplicate label edges are checked after issues.
		case *LinkExport:
			if r.ID <= 0 {
				return counts, errors.New("invalid link id")
			}
			if ids["link"] == nil {
				ids["link"] = map[int64]bool{}
			}
			if ids["link"][r.ID] {
				return counts, errors.New("duplicate link id")
			}
			ids["link"][r.ID] = true
		case *EventExport:
			err = identity("event", r.ID, r.UID)
			if len(x.events) > 0 && r.ID <= x.events[len(x.events)-1].ID {
				return counts, errors.New("events must remain in stored id order")
			}
			if !uid.Valid(r.OriginInstanceUID) || !publicationTime(r.CreatedAt) || strings.TrimSpace(r.Actor) == "" {
				return counts, errors.New("invalid event identity or timestamp")
			}
			x.events = append(x.events, *r)
			counts.Events++
		default:
			return counts, fmt.Errorf("unsupported publication record %q", record.ImportKind())
		}
		if err != nil {
			return counts, err
		}
	}
	if x.project == nil {
		return counts, errors.New("exactly one project is required")
	}
	for _, i := range x.issues {
		if i.ProjectID != x.project.ID {
			return counts, errors.New("issue outside source project")
		}
	}
	for _, c := range x.comments {
		if x.issues[c.IssueID] == nil {
			return counts, errors.New("comment outside source project")
		}
		if c.ReplyToUID != "" && x.comments[c.ReplyToUID] == nil && x.issueUIDs[c.ReplyToUID] == nil {
			return counts, errors.New("unresolved comment reply")
		}
	}
	for _, record := range p.Records {
		switch r := record.(type) {
		case *IssueLabelExport:
			i := x.issues[r.IssueID]
			if i == nil || r.Label == "" || !publicationTime(r.CreatedAt) {
				return counts, errors.New("invalid label reference")
			}
			key := FoldLabelKey{IssueUID: i.UID, Label: r.Label}
			if x.labels[key] {
				return counts, errors.New("duplicate label")
			}
			x.labels[key] = true
		case *LinkExport:
			if x.issues[r.FromIssueID] == nil || x.issues[r.ToIssueID] == nil || x.issues[r.FromIssueID].UID != r.FromIssueUID || x.issues[r.ToIssueID].UID != r.ToIssueUID || r.FromIssueUID == r.ToIssueUID || !publicationLinkType(r.Type) || !publicationTime(r.CreatedAt) {
				return counts, errors.New("invalid link reference")
			}
			key := publicationLinkKey(r.FromIssueUID, r.ToIssueUID, r.Type)
			if x.links[key] != nil {
				return counts, errors.New("duplicate link")
			}
			x.links[key] = r
		}
	}
	reset, err := RelayHistoryNeedsReset(x.events)
	if err != nil {
		return counts, err
	}
	if reset {
		return counts, errors.New("retained history needs reset")
	}
	introduced, commentIntroduced := map[string]bool{}, map[string]bool{}
	fold := make([]FoldEvent, 0, len(x.events))
	for _, e := range x.events {
		if e.ProjectID != x.project.ID || (e.ProjectUID != "" && e.ProjectUID != p.ProjectUID) {
			return counts, errors.New("event outside source project")
		}
		if _, err := ReplayEventProjectName(&e, p.ProjectName, false); err != nil || e.ProjectName == "" {
			return counts, errors.New("event missing historical project name")
		}
		if err := PrepareReplayEvent(&e, p.ProjectUID, e.ProjectName, false); err != nil {
			return counts, err
		}
		if err := x.validateEvent(e, introduced, commentIntroduced); err != nil {
			return counts, fmt.Errorf("event %d: %w", e.ID, err)
		}
		f := FoldEvent{UID: e.UID, OriginInstanceUID: e.OriginInstanceUID, ProjectUID: p.ProjectUID, Type: e.Type, Actor: e.Actor, HLCPhysicalMS: e.HLCPhysicalMS, HLCCounter: e.HLCCounter, CreatedAt: e.CreatedAt, Payload: e.Payload}
		if e.IssueUID != nil {
			f.IssueUID = *e.IssueUID
		}
		if e.RelatedIssueUID != nil {
			f.RelatedIssueUID = *e.RelatedIssueUID
		}
		fold = append(fold, f)
	}
	if err := x.validateProjection(FoldEvents(fold)); err != nil {
		return counts, err
	}
	return counts, nil
}

func publicationObject(raw jsontext.Value) bool {
	var m map[string]jsontext.Value
	return json.Unmarshal(raw, &m) == nil && m != nil
}
func publicationTime(s string) bool     { _, err := time.Parse(time.RFC3339Nano, s); return err == nil }
func publicationLinkType(s string) bool { return s == "parent" || s == "blocks" || s == "related" }
func publicationLinkKey(from, to, kind string) FoldLinkKey {
	if kind == "related" && from > to {
		from, to = to, from
	}
	return FoldLinkKey{FromUID: from, ToUID: to, Type: kind}
}

func (x publicationIndex) validateEvent(e EventExport, introduced, comments map[string]bool) error {
	if e.Type == "project.author_rewritten" {
		return errors.New("author-rewrite history cannot be relayed")
	}
	switch e.Type {
	case "issue.created", "issue.snapshot", "issue.updated", "issue.assigned", "issue.unassigned", "issue.assignment_renewed", "issue.assignment_expired", "issue.priority_set", "issue.priority_cleared", "issue.closed", "issue.reopened", "issue.soft_deleted", "issue.restored", "issue.commented", "issue.comment_edited", "issue.labeled", "issue.unlabeled", "issue.linked", "issue.unlinked", "issue.links_changed", "issue.metadata_updated", "project.created", "project.renamed", "project.metadata_updated":
	default:
		return errors.New("unsupported publication event type")
	}
	var payload map[string]jsontext.Value
	if json.Unmarshal(e.Payload, &payload) != nil || payload == nil {
		return errors.New("event payload must be an object")
	}
	if err := ValidateFederationEntries(e.Type, e.UID, e.Payload); err != nil {
		return err
	}
	if err := publicationPayloadShape(e.Type, payload); err != nil {
		return err
	}
	subject := ""
	if e.IssueUID != nil {
		subject = *e.IssueUID
	}
	subjectIssue := x.issueUIDs[subject]
	for _, key := range []string{"uid", "issue_uid"} {
		if raw, ok := payload[key]; ok {
			s, valid := StringValue(raw)
			if !valid || (subject != "" && subject != s) {
				return errors.New("payload issue identity disagreement")
			}
			subject = s
		}
	}
	if strings.HasPrefix(e.Type, "issue.") {
		subjectIssue = x.issueUIDs[subject]
		if subjectIssue == nil {
			return errors.New("unknown issue subject")
		}
		if e.Type == "issue.created" || e.Type == "issue.snapshot" {
			introduced[subject] = true
		}
		if !introduced[subject] {
			return errors.New("issue used before introduction")
		}
	} else if subject != "" || e.IssueID != nil || e.RelatedIssueID != nil || e.RelatedIssueUID != nil {
		return errors.New("catalog event carries issue identity")
	}
	checkIssue := func(value string) error {
		if value == "" || x.issueUIDs[value] == nil || !introduced[value] {
			return errors.New("outside, unresolved or not yet introduced issue reference")
		}
		return nil
	}
	if e.IssueID != nil {
		issue := x.issues[*e.IssueID]
		if issue == nil || subjectIssue == nil || issue.UID != subject {
			return errors.New("event subject foreign key disagreement")
		}
	}
	if e.RelatedIssueUID != nil {
		if err := checkIssue(*e.RelatedIssueUID); err != nil {
			return err
		}
	}
	if e.RelatedIssueID != nil {
		issue := x.issues[*e.RelatedIssueID]
		if e.RelatedIssueUID == nil || issue == nil || issue.UID != *e.RelatedIssueUID {
			return errors.New("event peer foreign key disagreement")
		}
	}
	for _, key := range []string{"from_uid", "to_uid", "from_issue_uid", "to_issue_uid", "link_from_uid", "link_to_uid", "parent_uid", "parent_set_uid", "parent_removed_uid"} {
		if raw, ok := payload[key]; ok {
			value, valid := StringValue(raw)
			if !valid {
				return errors.New("typed issue reference must be a string")
			}
			if value != "" {
				if err := checkIssue(value); err != nil {
					return err
				}
			}
		}
	}
	for _, key := range []string{"blocks_added_uids", "blocks_removed_uids", "blocked_by_added_uids", "blocked_by_removed_uids", "related_added_uids", "related_removed_uids"} {
		if raw, ok := payload[key]; ok {
			var refs []string
			if json.Unmarshal(raw, &refs) != nil || refs == nil {
				return errors.New("typed issue references must be an array")
			}
			for _, ref := range refs {
				if err := checkIssue(ref); err != nil {
					return err
				}
			}
		}
	}
	if raw, ok := payload["project_uid"]; ok {
		value, valid := StringValue(raw)
		if !valid || value != x.project.UID {
			return errors.New("payload project identity disagreement")
		}
	}
	if raw, ok := payload["parent_short_id"]; ok {
		value, valid := StringValue(raw)
		if !valid {
			return errors.New("invalid parent short id")
		}
		if value != "" {
			i := x.shortIDs[value]
			if i == nil {
				return errors.New("unresolved parent short id")
			}
			if err := checkIssue(i.UID); err != nil {
				return err
			}
			if parent, ok := StringValue(payload["parent_uid"]); ok && parent != "" && parent != i.UID {
				return errors.New("parent identity disagreement")
			}
		}
	}
	if raw, ok := payload["evidence"]; ok {
		var evidence []Evidence
		if err := json.Unmarshal(raw, &evidence, json.RejectUnknownMembers(true)); err != nil {
			return err
		}
		for _, item := range evidence {
			if item.Type == "duplicate-of" || item.Type == "superseded-by" {
				ref, err := shortid.Parse(item.IssueRef)
				if err != nil || (ref.Project != "" && ref.Project != x.project.Name && ref.Project != e.ProjectName) {
					return errors.New("outside close evidence reference")
				}
				i := x.shortIDs[ref.ShortID]
				if ref.ULID != "" {
					i = x.issueUIDs[ref.ULID]
				}
				if i == nil {
					return errors.New("unresolved close evidence reference")
				}
				if err := checkIssue(i.UID); err != nil {
					return err
				}
			}
		}
	}
	var entries struct {
		Links []struct {
			Type       string `json:"type"`
			ToIssueUID string `json:"to_issue_uid"`
			ToShortID  string `json:"to_short_id"`
			Incoming   bool   `json:"incoming"`
			Author     string `json:"author"`
			CreatedAt  string `json:"created_at"`
		} `json:"links"`
		Comments []struct {
			CommentUID string `json:"comment_uid"`
			ReplyToUID string `json:"reply_to_uid"`
			ReplyKind  string `json:"reply_kind"`
			Author     string `json:"author"`
			Teammate   string `json:"teammate"`
			Body       string `json:"body"`
			CreatedAt  string `json:"created_at"`
			EditedAt   string `json:"edited_at"`
		} `json:"comments"`
	}
	if raw, ok := payload["links"]; ok {
		if err := json.Unmarshal(raw, &entries.Links, json.RejectUnknownMembers(true)); err != nil {
			return err
		}
	}
	if raw, ok := payload["comments"]; ok {
		if err := json.Unmarshal(raw, &entries.Comments, json.RejectUnknownMembers(true)); err != nil {
			return err
		}
	}
	for _, link := range entries.Links {
		if !publicationLinkType(link.Type) || link.ToIssueUID == subject {
			return errors.New("invalid embedded link")
		}
		if err := checkIssue(link.ToIssueUID); err != nil {
			return err
		}
		peer := x.issueUIDs[link.ToIssueUID]
		if peer == nil {
			return errors.New("unknown embedded link peer")
		}
		if link.ToShortID != "" && peer.ShortID != link.ToShortID {
			return errors.New("embedded link short id disagreement")
		}
	}
	checkReply := func(value, kind string) error {
		if value == "" {
			return nil
		}
		if !ValidCommentReplyKind(kind) || x.comments[value] == nil || !comments[value] {
			return errors.New("unresolved comment reply")
		}
		return nil
	}
	frameComments := map[string]bool{}
	for _, c := range entries.Comments {
		comment := x.comments[c.CommentUID]
		if comment == nil || subjectIssue == nil || comment.IssueID != subjectIssue.ID {
			return errors.New("unknown embedded comment")
		}
		if frameComments[c.CommentUID] {
			return errors.New("duplicate embedded comment")
		}
		frameComments[c.CommentUID] = true
		comments[c.CommentUID] = true
	}
	for _, c := range entries.Comments {
		if err := checkReply(c.ReplyToUID, c.ReplyKind); err != nil {
			return err
		}
	}
	if e.Type == "issue.commented" || e.Type == "issue.comment_edited" {
		value, ok := StringValue(payload["comment_uid"])
		comment := x.comments[value]
		if !ok || comment == nil || subjectIssue == nil || comment.IssueID != subjectIssue.ID {
			return errors.New("invalid comment identity")
		}
		if e.Type == "issue.comment_edited" && !comments[value] {
			return errors.New("comment edit precedes introduction")
		}
		comments[value] = true
		reply, _ := StringValue(payload["reply_to_uid"])
		kind, _ := StringValue(payload["reply_kind"])
		if err := checkReply(reply, kind); err != nil {
			return err
		}
	}
	return nil
}

func (x publicationIndex) validateProjection(p FoldProjection) error {
	if len(p.Issues) != len(x.issues) || len(p.Comments) != len(x.comments) {
		return errors.New("projection/history entity disagreement")
	}
	for _, i := range x.issues {
		f := p.Issues[i.UID]
		if f.UID != i.UID || f.ShortID != i.ShortID || f.ProjectUID != x.project.UID || f.Title != i.Title || f.Body != i.Body || f.Author != i.Author || f.Status != i.Status || !reflect.DeepEqual(f.Owner, i.Owner) || !reflect.DeepEqual(f.Priority, i.Priority) || !reflect.DeepEqual(f.ClosedReason, i.ClosedReason) || !publicationEqualTime(f.CreatedAt, i.CreatedAt) || !publicationEqualTime(f.UpdatedAt, i.UpdatedAt) || !publicationEqualTimePtr(f.ClosedAt, i.ClosedAt) || !publicationEqualTimePtr(f.DeletedAt, i.DeletedAt) || !publicationEqualTimePtr(f.AssignmentExpiresOn, i.AssignmentExpiresOn) || !publicationEqualJSON(p.IssueMetadata[i.UID], i.Metadata) {
			return errors.New("issue projection/history disagreement")
		}
	}
	for _, c := range x.comments {
		f := p.Comments[c.UID]
		issue := x.issues[c.IssueID]
		if issue == nil || f.UID != c.UID || f.IssueUID != issue.UID || f.Author != c.Author || f.Teammate != c.Teammate || f.Body != c.Body || f.ReplyToUID != c.ReplyToUID || f.ReplyKind != c.ReplyKind || !publicationEqualTime(f.CreatedAt, c.CreatedAt) || !publicationEqualTime(f.EditedAt, c.EditedAt) {
			return errors.New("comment projection/history disagreement")
		}
	}
	n := 0
	for key, state := range p.Labels {
		if state.Present {
			n++
			if !x.labels[key] {
				return errors.New("label projection/history disagreement")
			}
		}
	}
	if n != len(x.labels) {
		return errors.New("label projection/history disagreement")
	}
	n = 0
	for key, state := range p.Links {
		if state.Present {
			n++
			link := x.links[key]
			if link == nil || link.Author != state.Author || (state.CreatedAt != "" && !publicationEqualTime(state.CreatedAt, link.CreatedAt)) {
				return errors.New("link projection/history disagreement")
			}
		}
	}
	if n != len(x.links) {
		return errors.New("link projection/history disagreement")
	}
	if !publicationEqualJSON(p.ProjectMetadata[x.project.UID], x.project.Metadata) {
		return errors.New("project metadata/history disagreement")
	}
	return nil
}

func publicationEqualTime(a, b string) bool {
	if a == b {
		return true
	}
	ta, ea := time.Parse(time.RFC3339Nano, a)
	tb, eb := time.Parse(time.RFC3339Nano, b)
	return ea == nil && eb == nil && ta.Equal(tb)
}
func publicationEqualTimePtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return publicationEqualTime(*a, *b)
}
func publicationEqualJSON(a, b jsontext.Value) bool {
	if len(a) == 0 {
		a = jsontext.Value(`{}`)
	}
	if len(b) == 0 {
		b = jsontext.Value(`{}`)
	}
	ca, ea := CanonicalEventJSON(a)
	cb, eb := CanonicalEventJSON(b)
	return ea == nil && eb == nil && string(ca) == string(cb)
}

// Closed payload shapes prevent ignored future fields from concealing typed
// references. All state reduction remains in the shared fold engine.
var publicationPayloadFields = map[string]string{
	"issue.created":            "uid short_id title body author owner assignment_expires_on priority status closed_reason closed_at deleted_at metadata labels links comments created_at updated_at revision idempotency_key idempotency_fingerprint",
	"issue.snapshot":           "uid short_id title body author owner assignment_expires_on priority status closed_reason closed_at deleted_at metadata labels links comments created_at updated_at revision",
	"issue.updated":            "title old_title body owner old_owner assignment_expires_on priority old_priority status closed_reason closed_at deleted_at created_at updated_at",
	"issue.assigned":           "owner previous_owner assignment_expires_on updated_at",
	"issue.unassigned":         "owner previous_owner updated_at",
	"issue.assignment_renewed": "owner old_assignment_expires_on assignment_expires_on updated_at",
	"issue.assignment_expired": "owner previous_owner assignment_expires_on updated_at",
	"issue.priority_set":       "priority previous_priority old_priority updated_at",
	"issue.priority_cleared":   "previous_priority old_priority updated_at",
	"issue.closed":             "reason closed_at message evidence transcript parent_uid parent_short_id idempotency_key idempotency_fingerprint",
	"issue.reopened":           "reopened_at updated_at",
	"issue.soft_deleted":       "deleted_at",
	"issue.restored":           "restored_at updated_at",
	"issue.commented":          "comment_uid author teammate body created_at reply_to_uid reply_kind idempotency_key idempotency_fingerprint",
	"issue.comment_edited":     "comment_uid body edited_at",
	"issue.labeled":            "label updated_at",
	"issue.unlabeled":          "label updated_at",
	"issue.linked":             "link_id type from_uid to_uid from_short_id to_short_id updated_at",
	"issue.unlinked":           "link_id type from_uid to_uid link_from_uid link_to_uid from_short_id to_short_id updated_at",
	"issue.links_changed":      "parent_set parent_set_uid parent_removed parent_removed_uid blocks_added blocks_added_uids blocks_removed blocks_removed_uids blocked_by_added blocked_by_added_uids blocked_by_removed blocked_by_removed_uids related_added related_added_uids related_removed related_removed_uids updated_at",
	"issue.metadata_updated":   "diff revision_new updated_at",
	"project.created":          "name",
	"project.renamed":          "from to",
	"project.metadata_updated": "diff revision_new",
	"project.author_rewritten": "project_uid from to issue_authors issue_owners comment_authors link_authors total updated_at",
}

func publicationPayloadShape(kind string, p map[string]jsontext.Value) error {
	allowed := " " + publicationPayloadFields[kind] + " "
	for key, raw := range p {
		if (key != "uid" && key != "issue_uid" || !strings.HasPrefix(kind, "issue.")) && !strings.Contains(allowed, " "+key+" ") {
			return fmt.Errorf("unsupported %s payload field %q", kind, key)
		}
		switch key {
		case "link_id", "priority", "previous_priority", "old_priority", "revision", "revision_new", "issue_authors", "issue_owners", "comment_authors", "link_authors", "total":
			if string(raw) == "null" && (key == "priority" || key == "previous_priority" || key == "old_priority") {
				continue
			}
			var value int64
			if json.Unmarshal(raw, &value) != nil {
				return errors.New("numeric payload field must be an integer")
			}
		case "metadata", "diff":
			if !publicationObject(raw) {
				return errors.New("metadata payload must be an object")
			}
		case "links", "comments":
			var entries []map[string]jsontext.Value
			if json.Unmarshal(raw, &entries) != nil || entries == nil {
				return errors.New("embedded content must be an array")
			}
		case "blocks_added", "blocks_removed", "blocked_by_added", "blocked_by_removed", "related_added", "related_removed":
			var refs []string
			if json.Unmarshal(raw, &refs) != nil || refs == nil {
				return errors.New("relationship display references must be an array")
			}
		case "evidence", "transcript": // validated with their typed consumers
		case "labels":
			var values []string
			if json.Unmarshal(raw, &values) != nil || values == nil {
				return errors.New("labels must be an array")
			}
		default:
			if strings.HasSuffix(key, "_uids") {
				continue
			}
			if string(raw) == "null" && (key == "owner" || key == "old_owner" || key == "closed_reason" || key == "closed_at" || key == "deleted_at" || key == "assignment_expires_on") {
				continue
			}
			value, ok := StringValue(raw)
			if !ok {
				return fmt.Errorf("%s must be a string", key)
			}
			if strings.HasSuffix(key, "_at") || strings.HasSuffix(key, "_expires_on") {
				if !publicationTime(value) {
					return errors.New("invalid payload timestamp")
				}
			}
		}
	}
	requireStrings := func(keys ...string) error {
		for _, key := range keys {
			value, ok := StringValue(p[key])
			if !ok || value == "" {
				return fmt.Errorf("missing %s", key)
			}
		}
		return nil
	}
	switch kind {
	case "issue.created", "issue.snapshot":
		return requireStrings("short_id", "title", "author", "status", "created_at")
	case "issue.assigned":
		return requireStrings("owner")
	case "issue.assignment_renewed":
		return requireStrings("owner", "old_assignment_expires_on", "assignment_expires_on")
	case "issue.assignment_expired":
		return requireStrings("previous_owner", "assignment_expires_on")
	case "issue.priority_set":
		if _, ok := int64Value(p["priority"]); !ok {
			return errors.New("missing priority")
		}
	case "issue.closed":
		return requireStrings("reason")
	case "issue.commented":
		return requireStrings("comment_uid", "author")
	case "issue.comment_edited":
		if _, ok := StringValue(p["body"]); !ok {
			return errors.New("missing comment body")
		}
		return requireStrings("comment_uid")
	case "issue.labeled", "issue.unlabeled":
		return requireStrings("label")
	case "issue.linked", "issue.unlinked":
		typ, _ := StringValue(p["type"])
		if !publicationLinkType(typ) {
			return errors.New("invalid link type")
		}
		return requireStrings("from_uid", "to_uid")
	case "issue.metadata_updated", "project.metadata_updated":
		if !publicationObject(p["diff"]) {
			return errors.New("missing metadata diff")
		}
	case "project.created":
		return requireStrings("name")
	case "project.renamed", "project.author_rewritten":
		return requireStrings("from", "to")
	}
	return nil
}
