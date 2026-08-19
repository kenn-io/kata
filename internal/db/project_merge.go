package db

import (
	"encoding/json"
	"fmt"
	"math"

	katauid "go.kenn.io/kata/internal/uid"
)

// ProjectMergeOffsets reserves fresh numeric ID ranges in an existing target.
// TargetProjectID is the exact new project ID; every other value is added to
// source IDs from the project snapshot.
type ProjectMergeOffsets struct {
	TargetProjectID int64
	Alias           int64
	SyncBinding     int64
	Recurrence      int64
	Issue           int64
	Comment         int64
	Link            int64
	ImportMapping   int64
	Quarantine      int64
	Enrollment      int64
	Claim           int64
	PendingClaim    int64
	Event           int64
	PurgeLog        int64
	ProjectPurgeLog int64
}

// ExistingIssueLookup resolves an issue UID already present in the merge
// target. It is used only for informational event references; merge never
// creates relationship rows that involve an existing project.
type ExistingIssueLookup func(uid string) (id int64, found bool, err error)

// PrepareProjectMergeRecords validates a single-project snapshot, clones its
// records, remaps numeric IDs, and drops whole-database metadata/sequences.
func PrepareProjectMergeRecords(
	recs []ImportRecord,
	offsets ProjectMergeOffsets,
	lookupExistingIssue ExistingIssueLookup,
) ([]ImportRecord, error) {
	if err := ValidateImportRecords(recs); err != nil {
		return nil, err
	}
	var project *ProjectExport
	importedIssues := make(map[int64]string)
	for _, rec := range recs {
		if rec.Project != nil {
			if rec.Project.UID == SystemProjectUID || rec.Project.Name == SystemProjectName {
				return nil, fmt.Errorf("project merge requires one non-system project")
			}
			if project != nil {
				return nil, fmt.Errorf("project merge requires exactly one project record")
			}
			p := *rec.Project
			project = &p
		}
		if rec.Issue != nil {
			importedIssues[rec.Issue.ID] = rec.Issue.UID
		}
	}
	if project == nil {
		return nil, fmt.Errorf("project merge requires exactly one project record")
	}
	if offsets.TargetProjectID <= 0 {
		return nil, fmt.Errorf("project merge target project ID must be positive")
	}

	resolveIssue := func(sourceID int64, uid string) (int64, error) {
		if _, ok := importedIssues[sourceID]; ok {
			return addMergeOffset(sourceID, offsets.Issue, "issue")
		}
		if uid == "" || lookupExistingIssue == nil {
			return 0, nil
		}
		id, found, err := lookupExistingIssue(uid)
		if err != nil {
			return 0, err
		}
		if !found {
			return 0, nil
		}
		return id, nil
	}
	resolveImportedIssue := func(sourceID int64, uid, kind string) (int64, error) {
		importedUID, ok := importedIssues[sourceID]
		if !ok {
			return 0, fmt.Errorf("project merge %s issue %d is not part of the imported project", kind, sourceID)
		}
		if uid != "" && uid != importedUID {
			return 0, fmt.Errorf("project merge %s issue UID %q does not match imported issue %d UID %q",
				kind, uid, sourceID, importedUID)
		}
		return addMergeOffset(sourceID, offsets.Issue, kind+" issue")
	}
	resolveLinkIssue := func(sourceID int64) (int64, error) {
		if _, ok := importedIssues[sourceID]; !ok {
			return 0, nil
		}
		return addMergeOffset(sourceID, offsets.Issue, "link issue")
	}
	projectID := project.ID
	out := make([]ImportRecord, 0, len(recs)-2)
	for _, rec := range recs {
		if rec.Kind == ImportKindMeta {
			continue
		}
		cloned := cloneImportRecord(rec)
		var err error
		switch cloned.Kind {
		case ImportKindProject:
			cloned.Project.ID = offsets.TargetProjectID
		case ImportKindProjectAlias:
			if err = requireMergeProjectID(cloned.Alias.ProjectID, projectID, cloned.Kind); err == nil {
				cloned.Alias.ID, err = addMergeOffset(cloned.Alias.ID, offsets.Alias, cloned.Kind)
				cloned.Alias.ProjectID = offsets.TargetProjectID
			}
		case ImportKindIssueSyncBinding:
			if err = requireMergeProjectID(cloned.IssueSyncBinding.ProjectID, projectID, cloned.Kind); err == nil {
				cloned.IssueSyncBinding.ID, err = addMergeOffset(cloned.IssueSyncBinding.ID, offsets.SyncBinding, cloned.Kind)
				cloned.IssueSyncBinding.ProjectID = offsets.TargetProjectID
			}
		case ImportKindIssueSyncStatus:
			if err = requireMergeProjectID(cloned.IssueSyncStatus.ProjectID, projectID, cloned.Kind); err == nil {
				cloned.IssueSyncStatus.BindingID, err = addMergeOffset(cloned.IssueSyncStatus.BindingID, offsets.SyncBinding, cloned.Kind)
				cloned.IssueSyncStatus.ProjectID = offsets.TargetProjectID
			}
		case ImportKindRecurrence:
			if err = requireMergeProjectID(cloned.Recurrence.ProjectID, projectID, cloned.Kind); err == nil {
				cloned.Recurrence.ID, err = addMergeOffset(cloned.Recurrence.ID, offsets.Recurrence, cloned.Kind)
				cloned.Recurrence.ProjectID = offsets.TargetProjectID
			}
		case ImportKindIssue:
			if err = requireMergeProjectID(cloned.Issue.ProjectID, projectID, cloned.Kind); err == nil {
				cloned.Issue.ID, err = addMergeOffset(cloned.Issue.ID, offsets.Issue, cloned.Kind)
				cloned.Issue.ProjectID = offsets.TargetProjectID
				if err == nil && cloned.Issue.RecurrenceID != nil {
					*cloned.Issue.RecurrenceID, err = addMergeOffset(*cloned.Issue.RecurrenceID, offsets.Recurrence, cloned.Kind+" recurrence")
				}
			}
		case ImportKindComment:
			cloned.Comment.ID, err = addMergeOffset(cloned.Comment.ID, offsets.Comment, cloned.Kind)
			if err == nil {
				cloned.Comment.IssueID, err = resolveImportedIssue(cloned.Comment.IssueID, "", "comment")
			}
		case ImportKindIssueLabel:
			cloned.Label.IssueID, err = resolveImportedIssue(cloned.Label.IssueID, "", "label")
		case ImportKindLink:
			_, fromImported := importedIssues[cloned.Link.FromIssueID]
			_, toImported := importedIssues[cloned.Link.ToIssueID]
			if !fromImported && !toImported {
				err = fmt.Errorf("project merge link must include an issue from the imported project")
			}
			if err == nil {
				cloned.Link.ID, err = addMergeOffset(cloned.Link.ID, offsets.Link, cloned.Kind)
			}
			if err == nil {
				cloned.Link.FromIssueID, err = resolveLinkIssue(cloned.Link.FromIssueID)
			}
			if err == nil {
				cloned.Link.ToIssueID, err = resolveLinkIssue(cloned.Link.ToIssueID)
			}
		case ImportKindImportMapping:
			if err = requireMergeProjectID(cloned.ImportMapping.ProjectID, projectID, cloned.Kind); err == nil {
				cloned.ImportMapping.ID, err = addMergeOffset(cloned.ImportMapping.ID, offsets.ImportMapping, cloned.Kind)
				cloned.ImportMapping.ProjectID = offsets.TargetProjectID
				if err == nil && cloned.ImportMapping.IssueID != nil {
					*cloned.ImportMapping.IssueID, err = addMergeOffset(*cloned.ImportMapping.IssueID, offsets.Issue, cloned.Kind+" issue")
				}
				if err == nil && cloned.ImportMapping.CommentID != nil {
					*cloned.ImportMapping.CommentID, err = addMergeOffset(*cloned.ImportMapping.CommentID, offsets.Comment, cloned.Kind+" comment")
				}
				if err == nil && cloned.ImportMapping.LinkID != nil {
					*cloned.ImportMapping.LinkID, err = addMergeOffset(*cloned.ImportMapping.LinkID, offsets.Link, cloned.Kind+" link")
				}
			}
		case ImportKindFederationBinding, ImportKindFederationSyncStatus,
			ImportKindFederationQuarantine, ImportKindFederationEnrollment:
			// Federation cursors are split between local and remote event ID
			// spaces, while enrollment token hashes are live credentials. Drop
			// all federation state so the imported project must rejoin cleanly.
			continue
		case ImportKindIssueClaim:
			if err = requireMergeProjectID(cloned.IssueClaim.ProjectID, projectID, cloned.Kind); err == nil {
				cloned.IssueClaim.ID, err = addMergeOffset(cloned.IssueClaim.ID, offsets.Claim, cloned.Kind)
				cloned.IssueClaim.ProjectID = offsets.TargetProjectID
				if err == nil {
					cloned.IssueClaim.IssueID, err = resolveImportedIssue(
						cloned.IssueClaim.IssueID, cloned.IssueClaim.IssueUID, "claim",
					)
				}
			}
		case ImportKindPendingClaimRequest:
			if err = requireMergeProjectID(cloned.PendingClaimRequest.ProjectID, projectID, cloned.Kind); err == nil {
				cloned.PendingClaimRequest.ID, err = addMergeOffset(cloned.PendingClaimRequest.ID, offsets.PendingClaim, cloned.Kind)
				cloned.PendingClaimRequest.ProjectID = offsets.TargetProjectID
				if err == nil {
					cloned.PendingClaimRequest.IssueID, err = resolveImportedIssue(
						cloned.PendingClaimRequest.IssueID, cloned.PendingClaimRequest.IssueUID, "pending claim",
					)
				}
			}
		case ImportKindEvent:
			if err = requireMergeProjectID(cloned.Event.ProjectID, projectID, cloned.Kind); err == nil {
				cloned.Event.ID, err = addMergeOffset(cloned.Event.ID, offsets.Event, cloned.Kind)
				cloned.Event.ProjectID = offsets.TargetProjectID
				if err == nil && cloned.Event.IssueID != nil {
					*cloned.Event.IssueID, err = resolveImportedIssue(
						*cloned.Event.IssueID, mergeStringValue(cloned.Event.IssueUID), "event",
					)
				}
				if err == nil && cloned.Event.RelatedIssueID != nil {
					*cloned.Event.RelatedIssueID, err = resolveIssue(*cloned.Event.RelatedIssueID, mergeStringValue(cloned.Event.RelatedIssueUID))
				}
				if err == nil {
					err = remapMergedEventLinkID(cloned.Event, offsets.Link, project.UID)
				}
			}
		case ImportKindPurgeLog:
			if err = requireMergeProjectID(cloned.PurgeLog.ProjectID, projectID, cloned.Kind); err == nil {
				cloned.PurgeLog.ID, err = addMergeOffset(cloned.PurgeLog.ID, offsets.PurgeLog, cloned.Kind)
				cloned.PurgeLog.ProjectID = offsets.TargetProjectID
				if err == nil {
					cloned.PurgeLog.PurgedIssueID, err = addMergeOffset(cloned.PurgeLog.PurgedIssueID, offsets.Issue, cloned.Kind+" issue")
				}
				if err == nil {
					err = shiftEventPointers(
						cloned.PurgeLog.EventsDeletedMinID,
						cloned.PurgeLog.EventsDeletedMaxID,
						cloned.PurgeLog.PurgeResetAfterEventID,
						offsets.Event,
					)
				}
			}
		case ImportKindProjectPurgeLog:
			return nil, fmt.Errorf("project merge does not accept project_purge_log records")
		case ImportKindSQLiteSequence:
			var keep bool
			cloned.Sequence.Seq, keep, err = remapProjectMergeSequence(
				cloned.Sequence.Name, cloned.Sequence.Seq, projectID, offsets,
			)
			if !keep {
				continue
			}
		}
		if err != nil {
			return nil, err
		}
		out = append(out, cloned)
	}
	return out, nil
}

func remapProjectMergeSequence(
	name string,
	seq int64,
	sourceProjectID int64,
	offsets ProjectMergeOffsets,
) (int64, bool, error) {
	if seq < 0 {
		return 0, false, fmt.Errorf("project merge sequence %s has negative floor %d", name, seq)
	}
	if name == "projects" {
		if seq <= sourceProjectID {
			return offsets.TargetProjectID, true, nil
		}
		shifted, err := addSequenceFloor(seq-sourceProjectID, offsets.TargetProjectID, name)
		return shifted, true, err
	}
	offsetByName := map[string]int64{
		"project_aliases":        offsets.Alias,
		"issue_sync_bindings":    offsets.SyncBinding,
		"recurrences":            offsets.Recurrence,
		"issues":                 offsets.Issue,
		"comments":               offsets.Comment,
		"links":                  offsets.Link,
		"import_mappings":        offsets.ImportMapping,
		"federation_quarantine":  offsets.Quarantine,
		"federation_enrollments": offsets.Enrollment,
		"issue_claims":           offsets.Claim,
		"pending_claim_requests": offsets.PendingClaim,
		"events":                 offsets.Event,
		"purge_log":              offsets.PurgeLog,
		"project_purge_log":      offsets.ProjectPurgeLog,
	}
	offset, ok := offsetByName[name]
	if !ok {
		return 0, false, nil
	}
	shifted, err := addSequenceFloor(seq, offset, name)
	return shifted, true, err
}

func addSequenceFloor(seq, offset int64, name string) (int64, error) {
	if offset < 0 || offset > math.MaxInt64-seq {
		return 0, fmt.Errorf("project merge sequence %s floor overflows", name)
	}
	return seq + offset, nil
}

func remapMergedEventLinkID(event *EventExport, offset int64, projectUID string) error {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return fmt.Errorf("project merge event %d payload: %w", event.ID, err)
	}
	rawLinkID, ok := payload["link_id"]
	if !ok {
		return nil
	}
	var linkID int64
	if err := json.Unmarshal(rawLinkID, &linkID); err != nil {
		return fmt.Errorf("project merge event %d link_id: %w", event.ID, err)
	}
	shifted, err := addMergeOffset(linkID, offset, "event link")
	if err != nil {
		return err
	}
	payload["link_id"], err = json.Marshal(shifted)
	if err != nil {
		return fmt.Errorf("project merge event %d link_id: %w", event.ID, err)
	}
	event.Payload, err = json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("project merge event %d payload: %w", event.ID, err)
	}
	event.UID, err = katauid.New()
	if err != nil {
		return fmt.Errorf("project merge event %d uid: %w", event.ID, err)
	}
	event.ContentHash, err = EventContentHash(EventHashInput{
		UID:               event.UID,
		OriginInstanceUID: event.OriginInstanceUID,
		ProjectUID:        projectUID,
		ProjectName:       event.ProjectName,
		IssueUID:          event.IssueUID,
		RelatedIssueUID:   event.RelatedIssueUID,
		Type:              event.Type,
		Actor:             event.Actor,
		HLCPhysicalMS:     event.HLCPhysicalMS,
		HLCCounter:        event.HLCCounter,
		CreatedAt:         event.CreatedAt,
		Payload:           event.Payload,
	})
	if err != nil {
		return fmt.Errorf("project merge event %d content_hash: %w", event.ID, err)
	}
	return nil
}

func requireMergeProjectID(got, want int64, kind string) error {
	if got != want {
		return fmt.Errorf("project merge %s references project %d, want %d", kind, got, want)
	}
	return nil
}

func addMergeOffset(id, offset int64, kind string) (int64, error) {
	if id <= 0 {
		return 0, fmt.Errorf("project merge %s ID must be positive, got %d", kind, id)
	}
	if offset > math.MaxInt64-id {
		return 0, fmt.Errorf("project merge %s ID overflows", kind)
	}
	return offset + id, nil
}

func shiftEventPointers(first, last, reset *int64, offset int64) error {
	for _, value := range []*int64{first, last, reset} {
		if value == nil {
			continue
		}
		shifted, err := addMergeOffset(*value, offset, ImportKindEvent)
		if err != nil {
			return err
		}
		*value = shifted
	}
	return nil
}

func cloneImportRecord(rec ImportRecord) ImportRecord {
	out := ImportRecord{Kind: rec.Kind}
	if rec.Meta != nil {
		v := *rec.Meta
		out.Meta = &v
	}
	if rec.Project != nil {
		v := *rec.Project
		out.Project = &v
	}
	if rec.Alias != nil {
		v := *rec.Alias
		out.Alias = &v
	}
	if rec.IssueSyncBinding != nil {
		v := *rec.IssueSyncBinding
		out.IssueSyncBinding = &v
	}
	if rec.IssueSyncStatus != nil {
		v := *rec.IssueSyncStatus
		out.IssueSyncStatus = &v
	}
	if rec.Recurrence != nil {
		v := *rec.Recurrence
		out.Recurrence = &v
	}
	if rec.Issue != nil {
		v := *rec.Issue
		out.Issue = &v
		out.Issue.RecurrenceID = cloneInt64Ptr(v.RecurrenceID)
	}
	if rec.IssueEmbedding != nil {
		v := *rec.IssueEmbedding
		out.IssueEmbedding = &v
	}
	if rec.Comment != nil {
		v := *rec.Comment
		out.Comment = &v
	}
	if rec.Label != nil {
		v := *rec.Label
		out.Label = &v
	}
	if rec.Link != nil {
		v := *rec.Link
		out.Link = &v
	}
	if rec.ImportMapping != nil {
		v := *rec.ImportMapping
		out.ImportMapping = &v
		out.ImportMapping.IssueID = cloneInt64Ptr(v.IssueID)
		out.ImportMapping.CommentID = cloneInt64Ptr(v.CommentID)
		out.ImportMapping.LinkID = cloneInt64Ptr(v.LinkID)
	}
	if rec.FederationBinding != nil {
		v := *rec.FederationBinding
		out.FederationBinding = &v
	}
	if rec.FederationSyncStatus != nil {
		v := *rec.FederationSyncStatus
		out.FederationSyncStatus = &v
	}
	if rec.FederationQuarantine != nil {
		v := *rec.FederationQuarantine
		out.FederationQuarantine = &v
	}
	if rec.FederationEnrollment != nil {
		v := *rec.FederationEnrollment
		out.FederationEnrollment = &v
		out.FederationEnrollment.ProjectID = cloneInt64Ptr(v.ProjectID)
	}
	if rec.IssueClaim != nil {
		v := *rec.IssueClaim
		out.IssueClaim = &v
	}
	if rec.PendingClaimRequest != nil {
		v := *rec.PendingClaimRequest
		out.PendingClaimRequest = &v
	}
	if rec.Event != nil {
		v := *rec.Event
		out.Event = &v
		out.Event.IssueID = cloneInt64Ptr(v.IssueID)
		out.Event.RelatedIssueID = cloneInt64Ptr(v.RelatedIssueID)
	}
	if rec.PurgeLog != nil {
		v := *rec.PurgeLog
		out.PurgeLog = &v
		out.PurgeLog.EventsDeletedMinID = cloneInt64Ptr(v.EventsDeletedMinID)
		out.PurgeLog.EventsDeletedMaxID = cloneInt64Ptr(v.EventsDeletedMaxID)
		out.PurgeLog.PurgeResetAfterEventID = cloneInt64Ptr(v.PurgeResetAfterEventID)
	}
	if rec.ProjectPurgeLog != nil {
		v := *rec.ProjectPurgeLog
		out.ProjectPurgeLog = &v
	}
	if rec.Sequence != nil {
		v := *rec.Sequence
		out.Sequence = &v
	}
	return out
}

func mergeStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
