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
	var refused map[string]*FederationProjectNotEnrolledError
	var refusedIssues map[string]*FederationProjectNotEnrolledError
	var refusedComments map[string]*FederationProjectNotEnrolledError
	var refusedCronRuns map[string]*FederationProjectNotEnrolledError
	if allowPartial {
		refused = map[string]*FederationProjectNotEnrolledError{}
		refusedIssues = map[string]*FederationProjectNotEnrolledError{}
		refusedComments = map[string]*FederationProjectNotEnrolledError{}
		refusedCronRuns = map[string]*FederationProjectNotEnrolledError{}
	}
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
		staged := map[string]int64{}
		checked := map[checkedProject]bool{}
		newlyRefused := false
		for _, event := range surviving {
			if allowPartial {
				if depErr := federationRefusedDependencyError(event, refusedIssues, refusedComments, refusedCronRuns); depErr != nil {
					refused[event.EventUID] = depErr
					recordFederationRefusedArtifacts(event, depErr, refusedIssues, refusedComments, refusedCronRuns)
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
				recordFederationRefusedArtifacts(event, eventRefusal, refusedIssues, refusedComments, refusedCronRuns)
				newlyRefused = true
				continue
			}
			switch event.Type {
			case "issue.created", "issue.snapshot":
				if _, exists := staged[primary]; !exists {
					staged[primary] = p.ProjectID
				}
			case "issue.moved":
				_, to, err := FederationMoveProjects(ctx, tx, p.ProjectID, event)
				if err != nil {
					return nil, err
				}
				staged[primary] = to
			}
		}
		if !newlyRefused {
			return refused, parentErr
		}
	}
}

func recordFederationRefusedArtifacts(
	event RemoteEvent,
	refusal *FederationProjectNotEnrolledError,
	refusedIssues map[string]*FederationProjectNotEnrolledError,
	refusedComments map[string]*FederationProjectNotEnrolledError,
	refusedCronRuns map[string]*FederationProjectNotEnrolledError,
) {
	switch event.Type {
	case "issue.created", "issue.snapshot", "issue.moved":
		if uid := EventIssueUID(event.IssueUID); uid != "" {
			if _, exists := refusedIssues[uid]; !exists {
				refusedIssues[uid] = refusal
			}
		}
		for _, commentUID := range federationSnapshotCommentUIDs(event) {
			if _, exists := refusedComments[commentUID]; !exists {
				refusedComments[commentUID] = refusal
			}
		}
	case "issue.commented":
		m := PayloadMap(event.Payload)
		commentUID, _ := StringValue(m["comment_uid"])
		if commentUID == "" {
			commentUID, _ = StringValue(m["uid"])
		}
		if commentUID != "" {
			if _, exists := refusedComments[commentUID]; !exists {
				refusedComments[commentUID] = refusal
			}
		}
	case "cron.run.observed", "cron.run.snapshot":
		var run CronRunObservation
		if json.Unmarshal(event.Payload, &run) == nil && run.UID != "" {
			if _, exists := refusedCronRuns[run.UID]; !exists {
				refusedCronRuns[run.UID] = refusal
			}
		}
	}
}

func federationRefusedDependencyError(
	event RemoteEvent,
	refusedIssues map[string]*FederationProjectNotEnrolledError,
	refusedComments map[string]*FederationProjectNotEnrolledError,
	refusedCronRuns map[string]*FederationProjectNotEnrolledError,
) *FederationProjectNotEnrolledError {
	switch {
	case strings.HasPrefix(event.Type, "issue."):
		if primary := EventIssueUID(event.IssueUID); primary != "" {
			if refusal, ok := refusedIssues[primary]; ok {
				return refusal
			}
		}
		switch event.Type {
		case "issue.comment_edited":
			if commentUID, ok := StringValue(PayloadMap(event.Payload)["comment_uid"]); ok {
				if refusal, exists := refusedComments[commentUID]; exists {
					return refusal
				}
			}
		case "issue.commented":
			if replyTo, ok := StringValue(PayloadMap(event.Payload)["reply_to_uid"]); ok {
				if refusal, exists := refusedComments[replyTo]; exists {
					return refusal
				}
			}
		case "issue.moved":
			for _, commentUID := range federationSnapshotCommentUIDs(event) {
				if refusal, exists := refusedComments[commentUID]; exists {
					return refusal
				}
			}
		}
	case event.Type == "cron.run.observed" || event.Type == "cron.run.snapshot":
		var run CronRunObservation
		if json.Unmarshal(event.Payload, &run) == nil {
			if run.UID != "" {
				if refusal, ok := refusedCronRuns[run.UID]; ok {
					return refusal
				}
			}
			if run.IssueUID != nil {
				if refusal, ok := refusedIssues[*run.IssueUID]; ok {
					return refusal
				}
			}
		}
	}
	return nil
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
