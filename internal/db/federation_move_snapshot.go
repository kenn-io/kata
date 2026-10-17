package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"
)

// FederationMoveSnapshotEvent exposes a move's document to the existing issue
// validators without modifying the retained event or its hash.
func FederationMoveSnapshotEvent(event RemoteEvent) RemoteEvent {
	if event.Type == "issue.moved" {
		event.Type = "issue.snapshot"
		event.Payload = PayloadMap(event.Payload)["snapshot"]
	}
	return event
}

type moveSnapshotComment struct {
	UID        string `json:"comment_uid"`
	Author     string `json:"author"`
	Body       string `json:"body"`
	CreatedAt  string `json:"created_at"`
	EditedAt   string `json:"edited_at"`
	ReplyToUID string `json:"reply_to_uid"`
	ReplyKind  string `json:"reply_kind"`
}

func (c moveSnapshotComment) normalized() moveSnapshotComment {
	c.Author = strings.TrimSpace(c.Author)
	c.CreatedAt, c.EditedAt = moveSnapshotTime(c.CreatedAt), moveSnapshotTime(c.EditedAt)
	return c
}

func (c moveSnapshotComment) identity() moveSnapshotComment {
	c = c.normalized()
	c.Body, c.EditedAt = "", ""
	return c
}

func moveSnapshotTime(value string) string {
	if at, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return at.UTC().Format(time.RFC3339Nano)
	}
	return value
}

type moveSnapshotLink struct {
	ToIssueUID string `json:"to_issue_uid"`
	Type       string `json:"type"`
	Incoming   bool   `json:"incoming"`
	Author     string `json:"author"`
}

type moveSnapshotLinkIdentity struct {
	FoldLinkKey
	Author string
}

func snapshotLinkIdentity(key FoldLinkKey, author string) moveSnapshotLinkIdentity {
	if key.Type == "related" && key.FromUID > key.ToUID {
		key.FromUID, key.ToUID = key.ToUID, key.FromUID
	}
	return moveSnapshotLinkIdentity{key, strings.TrimSpace(author)}
}

func (l moveSnapshotLink) identity(uid, actor string) moveSnapshotLinkIdentity {
	key := FoldLinkKey{FromUID: uid, ToUID: l.ToIssueUID, Type: l.Type}
	if l.Incoming && l.Type != "related" {
		key.FromUID, key.ToUID = key.ToUID, key.FromUID
	}
	if l.Author != "" {
		actor = l.Author
	}
	return snapshotLinkIdentity(key, actor)
}

type moveSnapshotProof struct {
	authors    map[string]bool
	comments   map[moveSnapshotComment]bool
	identities map[moveSnapshotComment]bool
	links      map[moveSnapshotLinkIdentity]bool
}

func (p *moveSnapshotProof) addComment(comment moveSnapshotComment) {
	comment = comment.normalized()
	p.comments[comment], p.identities[comment.identity()] = true, true
}

// FederationMoveSnapshotValidator verifies all distributed move documents:
// a snapshot ignored here may become another replica's cold fallback. Only
// trusted rows and real ordinary history preserve foreign attribution/refs.
// Nested move snapshots and unvalidated batch baselines are never evidence.
type FederationMoveSnapshotValidator struct {
	tx            *sql.Tx
	proofs        map[string]*moveSnapshotProof
	edits         map[string][]FoldEvent
	commentOwners map[string]string
}

// NewFederationMoveSnapshotValidator creates one transaction's proof cache.
func NewFederationMoveSnapshotValidator(tx *sql.Tx) *FederationMoveSnapshotValidator {
	return &FederationMoveSnapshotValidator{tx: tx, proofs: map[string]*moveSnapshotProof{}, edits: map[string][]FoldEvent{}, commentOwners: map[string]string{}}
}

// ObservePrepared preserves earlier ordinary edit versions after envelope,
// structural and bound-actor checks. The edit's independent project-access
// checks still run before this transaction commits; edits cannot prove new
// authors or references, only versions of an already trusted comment identity.
func (v *FederationMoveSnapshotValidator) ObservePrepared(event RemoteEvent) {
	if event.Type == "issue.comment_edited" && event.IssueUID != nil {
		uid := *event.IssueUID
		v.edits[uid] = append(v.edits[uid], moveSnapshotFoldEvent(event))
		delete(v.proofs, uid)
	}
}

func moveSnapshotFoldEvent(event RemoteEvent) FoldEvent {
	uid := FederationEventIssueUID(event.IssueUID, string(event.Payload))
	return FoldEvent{UID: event.EventUID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID, IssueUID: uid, Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, CreatedAt: event.CreatedAt.UTC().Format(time.RFC3339Nano), Payload: event.Payload}
}

// Validate runs after duplicate recognition and never modifies event bytes.
func (v *FederationMoveSnapshotValidator) Validate(ctx context.Context, event RemoteEvent, boundActor string) error {
	if event.Type != "issue.moved" || boundActor == "" {
		return nil
	}
	proof, err := v.proof(ctx, event)
	if err != nil {
		return err
	}
	var snapshot struct {
		Author   string                `json:"author"`
		Comments []moveSnapshotComment `json:"comments"`
		Links    []moveSnapshotLink    `json:"links"`
	}
	if err := json.Unmarshal(FederationMoveSnapshotEvent(event).Payload, &snapshot); err != nil {
		return fmt.Errorf("%w: invalid move snapshot: %v", ErrFederationIngestValidation, err)
	}
	actor := strings.TrimSpace(boundActor)
	if author := strings.TrimSpace(snapshot.Author); author != actor && !proof.authors[author] {
		return fmt.Errorf("%w: unverified move snapshot author", ErrFederationIngestValidation)
	}
	uid := FederationEventIssueUID(event.IssueUID, string(event.Payload))
	for _, comment := range snapshot.Comments {
		comment = comment.normalized()
		owner, known := v.commentOwners[comment.UID]
		if !known {
			err := v.tx.QueryRowContext(ctx, `SELECT i.uid FROM comments c JOIN issues i ON i.id=c.issue_id WHERE c.uid=$1`, comment.UID).Scan(&owner)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			v.commentOwners[comment.UID] = owner
		}
		if owner != "" && owner != uid {
			return fmt.Errorf("%w: move snapshot comment belongs to another issue", ErrFederationIngestValidation)
		}
		if comment.Author == actor || proof.comments[comment] {
			continue
		}
		if proof.identities[comment.identity()] && comment.EditedAt != "" {
			if _, err := time.Parse(time.RFC3339Nano, comment.EditedAt); err == nil {
				return fmt.Errorf("%w: move snapshot comment edit has not arrived", ErrNotFound)
			}
		}
		return fmt.Errorf("%w: unverified move snapshot comment attribution/version", ErrFederationIngestValidation)
	}
	for _, link := range snapshot.Links {
		if author := strings.TrimSpace(link.Author); author != "" && author != actor && !proof.links[link.identity(uid, actor)] {
			return fmt.Errorf("%w: unverified move snapshot link attribution", ErrFederationIngestValidation)
		}
	}
	return nil
}

// UnprovenReferences creates an ephemeral validation view. Historical links
// and unchanged replies have already passed their original authority checks;
// every new reference must pass the caller's current project grants even if
// this hub's fold ignores the snapshot. The signed envelope remains unchanged.
func (v *FederationMoveSnapshotValidator) UnprovenReferences(ctx context.Context, event RemoteEvent) (RemoteEvent, error) {
	if event.Type != "issue.moved" {
		return event, nil
	}
	proof, err := v.proof(ctx, event)
	if err != nil {
		return RemoteEvent{}, err
	}
	document := PayloadMap(event.Payload)
	snapshot := PayloadMap(document["snapshot"])
	uid := FederationEventIssueUID(event.IssueUID, string(event.Payload))
	var links, comments, remaining []jsontext.Value
	if err := json.Unmarshal(snapshot["links"], &links); err != nil && len(snapshot["links"]) != 0 {
		return RemoteEvent{}, err
	}
	for _, raw := range links {
		var link moveSnapshotLink
		if err := json.Unmarshal(raw, &link); err != nil {
			return RemoteEvent{}, err
		}
		if !proof.links[link.identity(uid, event.Actor)] {
			remaining = append(remaining, raw)
		}
	}
	snapshot["links"], err = json.Marshal(remaining)
	if err != nil {
		return RemoteEvent{}, err
	}
	remaining = nil
	if err := json.Unmarshal(snapshot["comments"], &comments); err != nil && len(snapshot["comments"]) != 0 {
		return RemoteEvent{}, err
	}
	for _, raw := range comments {
		var comment moveSnapshotComment
		if err := json.Unmarshal(raw, &comment); err != nil {
			return RemoteEvent{}, err
		}
		if !proof.identities[comment.identity()] {
			remaining = append(remaining, raw)
		}
	}
	snapshot["comments"], err = json.Marshal(remaining)
	if err != nil {
		return RemoteEvent{}, err
	}
	document["snapshot"], err = json.Marshal(snapshot)
	if err != nil {
		return RemoteEvent{}, err
	}
	event.Payload, err = json.Marshal(document)
	return event, err
}

func (v *FederationMoveSnapshotValidator) proof(ctx context.Context, event RemoteEvent) (*moveSnapshotProof, error) {
	uid := FederationEventIssueUID(event.IssueUID, string(event.Payload))
	if proof, known := v.proofs[uid]; known {
		return proof, nil
	}
	proof := &moveSnapshotProof{authors: map[string]bool{}, comments: map[moveSnapshotComment]bool{}, identities: map[moveSnapshotComment]bool{}, links: map[moveSnapshotLinkIdentity]bool{}}
	var author string
	if err := v.tx.QueryRowContext(ctx, `SELECT author FROM issues WHERE uid=$1`, uid).Scan(&author); err == nil {
		proof.authors[strings.TrimSpace(author)] = true
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	rows, err := v.tx.QueryContext(ctx, `SELECT c.uid,c.author,c.body,c.created_at,c.edited_at,COALESCE(c.reply_to_uid,''),COALESCE(c.reply_kind,'') FROM comments c JOIN issues i ON i.id=c.issue_id WHERE i.uid=$1`, uid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var comment moveSnapshotComment
		var created string
		var edited sql.NullString
		if err := rows.Scan(&comment.UID, &comment.Author, &comment.Body, &created, &edited, &comment.ReplyToUID, &comment.ReplyKind); err != nil {
			_ = rows.Close()
			return nil, err
		}
		comment.CreatedAt = created
		if edited.Valid {
			comment.EditedAt = edited.String
		}
		proof.addComment(comment)
	}
	if err := closeMoveSnapshotRows(rows); err != nil {
		return nil, err
	}
	rows, err = v.tx.QueryContext(ctx, `SELECT from_issue_uid,to_issue_uid,type,author FROM links WHERE from_issue_uid=$1 OR to_issue_uid=$1`, uid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var key FoldLinkKey
		var author string
		if err := rows.Scan(&key.FromUID, &key.ToUID, &key.Type, &author); err != nil {
			_ = rows.Close()
			return nil, err
		}
		proof.links[snapshotLinkIdentity(key, author)] = true
	}
	if err := closeMoveSnapshotRows(rows); err != nil {
		return nil, err
	}
	rows, err = v.tx.QueryContext(ctx, `SELECT e.uid,e.origin_instance_uid,p.uid,COALESCE(e.issue_uid,''),COALESCE(e.related_issue_uid,''),e.type,e.actor,e.payload,e.hlc_physical_ms,e.hlc_counter,e.created_at FROM events e JOIN projects p ON p.id=e.project_id WHERE e.issue_uid=$1 OR e.related_issue_uid=$1`, uid)
	if err != nil {
		return nil, err
	}
	var history []FoldEvent
	for rows.Next() {
		var retained FoldEvent
		var payload string
		if err := rows.Scan(&retained.UID, &retained.OriginInstanceUID, &retained.ProjectUID, &retained.IssueUID, &retained.RelatedIssueUID, &retained.Type, &retained.Actor, &payload, &retained.HLCPhysicalMS, &retained.HLCCounter, &retained.CreatedAt); err != nil {
			_ = rows.Close()
			return nil, err
		}
		retained.Payload = []byte(payload)
		history = append(history, retained)
	}
	if err := closeMoveSnapshotRows(rows); err != nil {
		return nil, err
	}
	history = append(history, moveSnapshotFoldEvent(event)) // Departure metadata selects the effective real source baseline.
	history = FederationMoveBaselineEvents(history)
	for _, retained := range history {
		switch retained.Type {
		case "issue.created", "issue.snapshot", "issue.commented", "issue.linked", "issue.links_changed":
		default:
			continue // In particular, nested move snapshots never prove themselves.
		}
		projection := FoldEvents([]FoldEvent{retained})
		if issue := projection.Issues[uid]; issue.Author != "" {
			proof.authors[strings.TrimSpace(issue.Author)] = true
		}
		for _, comment := range projection.Comments {
			if comment.IssueUID == uid && comment.Author != "" {
				proof.addComment(moveSnapshotComment{UID: comment.UID, Author: comment.Author, Body: comment.Body, CreatedAt: comment.CreatedAt, EditedAt: comment.EditedAt, ReplyToUID: comment.ReplyToUID, ReplyKind: comment.ReplyKind})
			}
		}
		for key, state := range projection.Links {
			if state.Present && (key.FromUID == uid || key.ToUID == uid) {
				proof.links[snapshotLinkIdentity(key, state.Author)] = true
			}
		}
	}
	for _, retained := range append(history, v.edits[uid]...) {
		if retained.Type != "issue.comment_edited" || retained.IssueUID != uid {
			continue
		}
		commentUID, body, version := federationCommentEditVersion(retained)
		if version.IsZero() {
			continue
		}
		for identity := range proof.identities {
			if identity.UID == commentUID {
				identity.Body, identity.EditedAt = body, version.UTC().Format(time.RFC3339Nano)
				proof.comments[identity] = true
			}
		}
	}
	v.proofs[uid] = proof
	return proof, nil
}

func closeMoveSnapshotRows(rows *sql.Rows) error {
	readErr, closeErr := rows.Err(), rows.Close()
	if readErr != nil {
		return readErr
	}
	return closeErr
}

// FederationMoveSnapshotReplyIssues resolves known reply targets to their
// current issue so native push applies its existing project authority checks.
// Unknown targets remain deferred under the existing comment replay contract.
func FederationMoveSnapshotReplyIssues(ctx context.Context, tx *sql.Tx, event RemoteEvent) ([]string, error) {
	if event.Type != "issue.moved" {
		return nil, nil
	}
	var snapshot struct {
		Comments []struct {
			ReplyToUID string `json:"reply_to_uid"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(FederationMoveSnapshotEvent(event).Payload, &snapshot); err != nil {
		return nil, err
	}
	var refs []string
	for _, comment := range snapshot.Comments {
		if comment.ReplyToUID == "" {
			continue
		}
		var uid string
		err := tx.QueryRowContext(ctx, `SELECT i.uid FROM comments c JOIN issues i ON i.id=c.issue_id WHERE c.uid=$1`, comment.ReplyToUID).Scan(&uid)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		refs = append(refs, uid)
	}
	return refs, nil
}
