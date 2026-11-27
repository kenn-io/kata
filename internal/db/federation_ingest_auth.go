package db

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"
)

// FederationRefusedDependencies tracks issues, comments, and cron runs whose
// producing events were refused earlier in a push batch or pass so later
// causally dependent events are deferred without sending them out of order.
type FederationRefusedDependencies struct {
	issues   map[string]*FederationProjectNotEnrolledError
	comments map[string]*FederationProjectNotEnrolledError
	cronRuns map[string]*FederationProjectNotEnrolledError
}

// NewFederationRefusedDependencies returns an initialized dependency tracker.
func NewFederationRefusedDependencies() *FederationRefusedDependencies {
	return &FederationRefusedDependencies{
		issues:   map[string]*FederationProjectNotEnrolledError{},
		comments: map[string]*FederationProjectNotEnrolledError{},
		cronRuns: map[string]*FederationProjectNotEnrolledError{},
	}
}

// Record stores artifacts produced by a refused event. When commentAlreadyPresent
// is non-nil, comments that already exist on the hub or were created by an
// earlier surviving event are not recorded as missing.
func (d *FederationRefusedDependencies) Record(
	event RemoteEvent,
	refusal *FederationProjectNotEnrolledError,
	commentAlreadyPresent func(commentUID string) (bool, error),
) error {
	switch event.Type {
	case "issue.created", "issue.snapshot", "issue.moved":
		if uid := EventIssueUID(event.IssueUID); uid != "" {
			if _, exists := d.issues[uid]; !exists {
				d.issues[uid] = refusal
			}
		}
		for _, commentUID := range federationSnapshotCommentUIDs(event) {
			if _, exists := d.comments[commentUID]; exists {
				continue
			}
			if commentAlreadyPresent != nil {
				present, err := commentAlreadyPresent(commentUID)
				if err != nil {
					return err
				}
				if present {
					continue
				}
			}
			d.comments[commentUID] = refusal
		}
	case "issue.commented":
		if commentUID := federationCommentEventUID(event); commentUID != "" {
			if _, exists := d.comments[commentUID]; !exists {
				if commentAlreadyPresent != nil {
					present, err := commentAlreadyPresent(commentUID)
					if err != nil {
						return err
					}
					if present {
						return nil
					}
				}
				d.comments[commentUID] = refusal
			}
		}
	case "cron.run.observed", "cron.run.snapshot":
		var run CronRunObservation
		if json.Unmarshal(event.Payload, &run) == nil && run.UID != "" {
			if _, exists := d.cronRuns[run.UID]; !exists {
				d.cronRuns[run.UID] = refusal
			}
		}
	}
	return nil
}

// Dependency returns the refusal of an earlier event that event causally
// depends on, or nil if event has no refused dependency.
func (d *FederationRefusedDependencies) Dependency(event RemoteEvent) *FederationProjectNotEnrolledError {
	switch {
	case strings.HasPrefix(event.Type, "issue."):
		if primary := EventIssueUID(event.IssueUID); primary != "" {
			if refusal, ok := d.issues[primary]; ok {
				return refusal
			}
		}
		switch event.Type {
		case "issue.comment_edited":
			if commentUID, ok := StringValue(PayloadMap(event.Payload)["comment_uid"]); ok {
				if refusal, exists := d.comments[commentUID]; exists {
					return refusal
				}
			}
		case "issue.commented":
			if replyTo, ok := StringValue(PayloadMap(event.Payload)["reply_to_uid"]); ok {
				if refusal, exists := d.comments[replyTo]; exists {
					return refusal
				}
			}
		case "issue.moved":
			for _, commentUID := range federationSnapshotCommentRefs(event) {
				if refusal, exists := d.comments[commentUID]; exists {
					return refusal
				}
			}
		}
	case event.Type == "cron.run.observed" || event.Type == "cron.run.snapshot":
		var run CronRunObservation
		if json.Unmarshal(event.Payload, &run) == nil {
			if run.UID != "" {
				if refusal, ok := d.cronRuns[run.UID]; ok {
					return refusal
				}
			}
			if run.IssueUID != nil {
				if refusal, ok := d.issues[*run.IssueUID]; ok {
					return refusal
				}
			}
		}
	}
	return nil
}

// AuthorizeFederationCurrentIssuesTx checks current-project push authority for
// every fresh event in events. Fresh events retain their authored envelope, but
// authorization follows every affected issue's current owner. Duplicates
// already passed this acceptance boundary and must not acquire new authority
// merely to acknowledge a retry.
//
// When allowPartial is false, the first FederationProjectNotEnrolledError is
// returned immediately so the caller rolls back the entire batch. When
// allowPartial is true, events hitting FederationProjectNotEnrolledError (and
// later events in the same batch that causally depend on them) are collected in
// the returned map keyed by event UID; all other errors still fail the batch.
func AuthorizeFederationCurrentIssuesTx(
	ctx context.Context,
	tx *sql.Tx,
	p FederationIngestParams,
	events []RemoteEvent,
	moveSnapshots *FederationMoveSnapshotValidator,
	allowPartial bool,
	payloadRefs func(RemoteEvent) ([]string, error),
	authorizeProject func(ctx context.Context, projectID int64, projectUID string, reference bool) error,
) (map[string]*FederationProjectNotEnrolledError, error) {
	type checkedProject struct {
		id        int64
		reference bool
	}
	refused := map[string]*FederationProjectNotEnrolledError{}
	hubComments := map[string]bool{}
	for {
		surviving := events
		if len(refused) > 0 {
			surviving = make([]RemoteEvent, 0, len(events)-len(refused))
			for _, event := range events {
				if _, isRefused := refused[event.EventUID]; !isRefused {
					surviving = append(surviving, event)
				}
			}
		}
		// Report graph validation only after checking current project authority.
		parentRemovals, parentErr := DirectParentRemovals(ctx, tx, surviving, p.AuthorizeReference != nil)
		// Rebuild forward-only causal dependencies in original batch order on
		// each pass so a later refused event never blocks earlier independent work.
		deps := NewFederationRefusedDependencies()
		staged := map[string]int64{}
		stagedComments := map[string]bool{}
		checked := map[checkedProject]bool{}
		commentAlreadyPresent := func(commentUID string) (bool, error) {
			if stagedComments[commentUID] {
				return true, nil
			}
			if present, known := hubComments[commentUID]; known {
				return present, nil
			}
			var one int
			err := tx.QueryRowContext(ctx, `SELECT 1 FROM comments WHERE uid=$1`, commentUID).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				hubComments[commentUID] = false
				return false, nil
			}
			if err != nil {
				return false, err
			}
			hubComments[commentUID] = true
			return true, nil
		}
		newlyRefused := false
		for _, event := range events {
			if allowPartial {
				if existingRefusal, isRefused := refused[event.EventUID]; isRefused {
					if err := deps.Record(event, existingRefusal, commentAlreadyPresent); err != nil {
						return nil, err
					}
					continue
				}
				if depErr := deps.Dependency(event); depErr != nil {
					refused[event.EventUID] = depErr
					if err := deps.Record(event, depErr, commentAlreadyPresent); err != nil {
						return nil, err
					}
					newlyRefused = true
					continue
				}
			}
			referenceEvent, err := moveSnapshots.UnprovenReferences(ctx, event)
			if err != nil {
				return nil, err
			}
			var primary string
			var refs []string
			switch {
			case strings.HasPrefix(event.Type, "issue."):
				primary = EventIssueUID(event.IssueUID)
				refs, err = payloadRefs(referenceEvent)
				if err != nil {
					return nil, err
				}
			case event.Type == "cron.run.observed" || event.Type == "cron.run.snapshot":
				// A run's UID identifies the run, not its optional issue reference.
				var run CronRunObservation
				if err := json.Unmarshal(event.Payload, &run); err != nil {
					return nil, err
				}
				if run.IssueUID != nil {
					primary = *run.IssueUID
				}
			default:
				continue
			}
			if event.Type == "issue.moved" {
				replyIssues, err := FederationMoveSnapshotReplyIssues(ctx, tx, referenceEvent)
				if err != nil {
					return nil, err
				}
				refs = append(refs, replyIssues...)
			}
			peers, err := DirectLinkPeers(FoldEvent{Type: event.Type, UID: event.EventUID, IssueUID: EventIssueUID(event.IssueUID), Payload: event.Payload})
			if err != nil {
				return nil, err
			}
			explicitRefs := refs
			if p.AuthorizeReference != nil {
				for _, previous := range parentRemovals[event.EventUID] {
					refs = append(refs, previous)
					delete(peers, previous)
				}
			}
			refs = append([]string{primary}, refs...)
			var eventRefusal *FederationProjectNotEnrolledError
			for _, issueUID := range refs {
				if issueUID == "" {
					continue
				}
				current, known := staged[issueUID]
				if !known {
					err := tx.QueryRowContext(ctx, `SELECT project_id FROM issues WHERE uid=$1`, issueUID).Scan(&current)
					if errors.Is(err, sql.ErrNoRows) {
						// Existing validation permits deferred links and fresh creation.
						continue
					}
					if err != nil {
						return nil, err
					}
					staged[issueUID] = current
				}
				reference := issueUID != primary && peers[issueUID]
				key := checkedProject{current, reference}
				if current == p.ProjectID || checked[key] {
					continue
				}
				var projectUID string
				err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1 AND deleted_at IS NULL`, current).Scan(&projectUID)
				if errors.Is(err, sql.ErrNoRows) {
					return nil, ErrNotFound
				}
				if err != nil {
					return nil, err
				}
				if err := authorizeProject(ctx, current, projectUID, reference); err != nil {
					if errors.Is(err, ErrFederationMoveSourceUnauthorized) {
						if issueUID != primary && !slices.Contains(explicitRefs, issueUID) {
							issueUID = ""
						}
						notEnrolled := &FederationProjectNotEnrolledError{IssueUID: issueUID, Err: err}
						if !allowPartial {
							return nil, notEnrolled
						}
						eventRefusal = notEnrolled
						break
					}
					return nil, err
				}
				checked[key] = true
			}
			if eventRefusal != nil {
				refused[event.EventUID] = eventRefusal
				if err := deps.Record(event, eventRefusal, commentAlreadyPresent); err != nil {
					return nil, err
				}
				newlyRefused = true
				continue
			}
			switch event.Type {
			case "issue.created", "issue.snapshot":
				if _, exists := staged[primary]; !exists {
					staged[primary] = p.ProjectID
				}
				for _, commentUID := range federationSnapshotCommentUIDs(event) {
					stagedComments[commentUID] = true
				}
			case "issue.moved":
				_, to, err := FederationMoveProjects(ctx, tx, p.ProjectID, event)
				if err != nil {
					return nil, err
				}
				staged[primary] = to
				for _, commentUID := range federationSnapshotCommentUIDs(event) {
					stagedComments[commentUID] = true
				}
			case "issue.commented":
				if commentUID := federationCommentEventUID(event); commentUID != "" {
					stagedComments[commentUID] = true
				}
			}
		}
		if !newlyRefused {
			return refused, parentErr
		}
	}
}

func federationCommentEventUID(event RemoteEvent) string {
	m := PayloadMap(event.Payload)
	if commentUID, ok := StringValue(m["comment_uid"]); ok && commentUID != "" {
		return commentUID
	}
	commentUID, _ := StringValue(m["uid"])
	return commentUID
}

func federationSnapshotCommentUIDs(event RemoteEvent) []string {
	snapshot := event
	if event.Type == "issue.moved" {
		snapshot = FederationMoveSnapshotEvent(event)
	}
	if len(snapshot.Payload) == 0 {
		return nil
	}
	var doc struct {
		Comments []struct {
			CommentUID string `json:"comment_uid"`
			UID        string `json:"uid"`
		} `json:"comments"`
	}
	if json.Unmarshal(snapshot.Payload, &doc) != nil {
		return nil
	}
	var out []string
	for _, comment := range doc.Comments {
		if id := cmp.Or(comment.CommentUID, comment.UID); id != "" {
			out = append(out, id)
		}
	}
	return out
}

func federationSnapshotCommentRefs(event RemoteEvent) []string {
	snapshot := event
	if event.Type == "issue.moved" {
		snapshot = FederationMoveSnapshotEvent(event)
	}
	if len(snapshot.Payload) == 0 {
		return nil
	}
	var doc struct {
		Comments []struct {
			CommentUID string `json:"comment_uid"`
			UID        string `json:"uid"`
			ReplyToUID string `json:"reply_to_uid"`
		} `json:"comments"`
	}
	if json.Unmarshal(snapshot.Payload, &doc) != nil {
		return nil
	}
	var out []string
	for _, comment := range doc.Comments {
		if id := cmp.Or(comment.CommentUID, comment.UID); id != "" {
			out = append(out, id)
		}
		if comment.ReplyToUID != "" {
			out = append(out, comment.ReplyToUID)
		}
	}
	return out
}

// SummarizeRefusedFederationIngestEvents maps refused fresh event UIDs back to
// input batch order, computes the highest contiguous non-refused prefix
// PushCursorEventID (0 if the first input event was refused), and returns the
// refused event summaries.
func SummarizeRefusedFederationIngestEvents(
	inputs []FederationIngestEvent,
	refused map[string]*FederationProjectNotEnrolledError,
) ([]FederationIngestRefusedEvent, int64) {
	out := make([]FederationIngestRefusedEvent, 0, len(refused))
	var cursor int64
	prefixOpen := true
	for _, in := range inputs {
		if refusal, ok := refused[in.Event.EventUID]; ok {
			prefixOpen = false
			out = append(out, FederationIngestRefusedEvent{
				SourceEventID: in.SourceEventID,
				EventUID:      in.Event.EventUID,
				Code:          FederationProjectNotEnrolledCode,
				IssueUID:      refusal.IssueUID,
				Message:       refusal.Error(),
			})
			continue
		}
		if prefixOpen && in.SourceEventID > cursor {
			cursor = in.SourceEventID
		}
	}
	return out, cursor
}
