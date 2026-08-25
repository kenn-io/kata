package db

import (
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// Keep imported rows in the lower half of the signed ID range. This leaves
// ample room for the target's shared sequences after a merge while remaining
// far above any practical database size.
const maxProjectMergeID int64 = math.MaxInt64 / 2

// Imported clocks use the same lower-half limit so a merge cannot exhaust
// the signed HLC range needed by later target-wide events.
const maxProjectMergeHLCValue int64 = maxProjectMergeID

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
	importedIssueUIDs := make(map[string]struct{})
	importedRecurrencesByID := make(map[int64]string)
	importedRecurrencesByUID := make(map[string]int64)
	importedExternalBindings := make(map[string]struct{})
	importedExternalMappings := make(map[externalFieldMappingIdentity]struct{})
	for _, rec := range recs {
		switch rec := rec.(type) {
		case *ProjectExport:
			if rec.UID == SystemProjectUID || rec.Name == SystemProjectName {
				return nil, fmt.Errorf("project merge requires one non-system project")
			}
			if project != nil {
				return nil, fmt.Errorf("project merge requires exactly one project record")
			}
			p := *rec
			project = &p
		case *IssueExport:
			importedIssues[rec.ID] = rec.UID
			importedIssueUIDs[rec.UID] = struct{}{}
		case *RecurrenceExport:
			importedRecurrencesByID[rec.ID] = rec.UID
			importedRecurrencesByUID[rec.UID] = rec.ID
		case *ExternalRootBindingExport:
			importedExternalBindings[rec.UID] = struct{}{}
		case *ExternalFieldMappingExport:
			importedExternalMappings[externalFieldMappingExportIdentity(rec)] = struct{}{}
		}
	}
	if project == nil {
		return nil, fmt.Errorf("project merge requires exactly one project record")
	}
	if offsets.TargetProjectID <= 0 {
		return nil, fmt.Errorf("project merge target project ID must be positive")
	}
	if offsets.TargetProjectID > maxProjectMergeID {
		return nil, fmt.Errorf("project merge target project ID exceeds safe ID range")
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
	resolveIssue := func(sourceID int64, uid string) (int64, error) {
		if _, ok := importedIssues[sourceID]; ok {
			return resolveImportedIssue(sourceID, uid, "event related")
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
	resolveLinkIssue := func(sourceID int64) (int64, error) {
		if _, ok := importedIssues[sourceID]; !ok {
			return 0, nil
		}
		return addMergeOffset(sourceID, offsets.Issue, "link issue")
	}
	projectID := project.ID
	out := make([]ImportRecord, 0, len(recs)-2)
	var issueSequenceFloor, eventSequenceFloor int64
	for _, rec := range recs {
		if _, ok := rec.(*MetaKV); ok {
			continue
		}
		cloned := cloneImportRecord(rec)
		var err error
		switch payload := cloned.(type) {
		case *ProjectExport:
			payload.ID = offsets.TargetProjectID
		case *AliasExport:
			if err = requireMergeProjectID(payload.ProjectID, projectID, payload.ImportKind()); err == nil {
				payload.ID, err = addMergeOffset(payload.ID, offsets.Alias, payload.ImportKind())
				payload.ProjectID = offsets.TargetProjectID
			}
		case *IssueSyncBindingExport:
			if err = requireMergeProjectID(payload.ProjectID, projectID, payload.ImportKind()); err == nil {
				payload.ID, err = addMergeOffset(payload.ID, offsets.SyncBinding, payload.ImportKind())
				payload.ProjectID = offsets.TargetProjectID
			}
		case *IssueSyncStatusExport:
			if err = requireMergeProjectID(payload.ProjectID, projectID, payload.ImportKind()); err == nil {
				payload.BindingID, err = addMergeOffset(payload.BindingID, offsets.SyncBinding, payload.ImportKind())
				payload.ProjectID = offsets.TargetProjectID
			}
		case *RecurrenceExport:
			if err = requireMergeProjectID(payload.ProjectID, projectID, payload.ImportKind()); err == nil {
				payload.ID, err = addMergeOffset(payload.ID, offsets.Recurrence, payload.ImportKind())
				payload.ProjectID = offsets.TargetProjectID
			}
		case *IssueExport:
			if err = requireMergeProjectID(payload.ProjectID, projectID, payload.ImportKind()); err == nil {
				payload.ID, err = addMergeOffset(payload.ID, offsets.Issue, payload.ImportKind())
				payload.ProjectID = offsets.TargetProjectID
				if err == nil && payload.RecurrenceUID != nil {
					sourceRecurrenceID, ok := importedRecurrencesByUID[*payload.RecurrenceUID]
					if !ok {
						err = fmt.Errorf("project merge issue recurrence UID %q is not part of the imported project",
							*payload.RecurrenceUID)
					} else if payload.RecurrenceID == nil {
						payload.RecurrenceID = &sourceRecurrenceID
					} else if *payload.RecurrenceID != sourceRecurrenceID {
						err = fmt.Errorf("project merge issue recurrence ID %d does not match recurrence UID %q",
							*payload.RecurrenceID, *payload.RecurrenceUID)
					}
				}
				if err == nil && payload.RecurrenceID != nil {
					if _, ok := importedRecurrencesByID[*payload.RecurrenceID]; !ok {
						err = fmt.Errorf("project merge issue recurrence ID %d is not part of the imported project",
							*payload.RecurrenceID)
					}
				}
				if err == nil && payload.RecurrenceID != nil {
					*payload.RecurrenceID, err = addMergeOffset(*payload.RecurrenceID, offsets.Recurrence, payload.ImportKind()+" recurrence")
				}
			}
		case *CommentExport:
			payload.ID, err = addMergeOffset(payload.ID, offsets.Comment, payload.ImportKind())
			if err == nil {
				payload.IssueID, err = resolveImportedIssue(payload.IssueID, "", "comment")
			}
		case *IssueLabelExport:
			payload.IssueID, err = resolveImportedIssue(payload.IssueID, "", "label")
		case *LinkExport:
			_, fromImported := importedIssues[payload.FromIssueID]
			_, toImported := importedIssues[payload.ToIssueID]
			if !fromImported && !toImported {
				err = fmt.Errorf("project merge link must include an issue from the imported project")
			}
			if err == nil {
				payload.ID, err = addMergeOffset(payload.ID, offsets.Link, payload.ImportKind())
			}
			if err == nil {
				payload.FromIssueID, err = resolveLinkIssue(payload.FromIssueID)
			}
			if err == nil {
				payload.ToIssueID, err = resolveLinkIssue(payload.ToIssueID)
			}
		case *ImportMappingExport:
			if err = requireMergeProjectID(payload.ProjectID, projectID, payload.ImportKind()); err == nil {
				payload.ID, err = addMergeOffset(payload.ID, offsets.ImportMapping, payload.ImportKind())
				payload.ProjectID = offsets.TargetProjectID
				if err == nil && payload.IssueID != nil {
					*payload.IssueID, err = addMergeOffset(*payload.IssueID, offsets.Issue, payload.ImportKind()+" issue")
				}
				if err == nil && payload.CommentID != nil {
					*payload.CommentID, err = addMergeOffset(*payload.CommentID, offsets.Comment, payload.ImportKind()+" comment")
				}
				if err == nil && payload.LinkID != nil {
					*payload.LinkID, err = addMergeOffset(*payload.LinkID, offsets.Link, payload.ImportKind()+" link")
				}
			}
		case *ExternalFieldMappingExport:
			// Field mappings are global portable descriptor revisions. Preserve
			// their identity; backend replay either reuses an exact existing
			// revision or atomically rejects a conflicting target revision.
		case *ExternalRootBindingExport:
			if payload.ProjectUID != project.UID {
				err = fmt.Errorf("project merge external root binding references project UID %q, want %q",
					payload.ProjectUID, project.UID)
			} else if _, ok := importedIssueUIDs[payload.IssueUID]; !ok {
				err = fmt.Errorf("project merge external root binding issue UID %q is not part of the imported project",
					payload.IssueUID)
			}
		case *ExternalFieldStateExport:
			if _, ok := importedExternalBindings[payload.BindingUID]; !ok {
				err = fmt.Errorf("project merge external field state binding UID %q is not part of the imported project",
					payload.BindingUID)
			} else if _, ok := importedExternalMappings[externalFieldStateMappingIdentity(payload)]; !ok {
				err = fmt.Errorf("project merge external field state mapping is not part of the imported project envelope")
			}
		case *FederationBindingExport, *FederationSyncStatusExport,
			*FederationQuarantineExport, *FederationEnrollmentExport:
			// Federation cursors are split between local and remote event ID
			// spaces, while enrollment token hashes are live credentials. Drop
			// all federation state so the imported project must rejoin cleanly.
			continue
		case *IssueClaimExport:
			if err = requireMergeProjectID(payload.ProjectID, projectID, payload.ImportKind()); err == nil {
				payload.ID, err = addMergeOffset(payload.ID, offsets.Claim, payload.ImportKind())
				payload.ProjectID = offsets.TargetProjectID
				if err == nil {
					payload.IssueID, err = resolveImportedIssue(
						payload.IssueID, payload.IssueUID, "claim",
					)
				}
			}
		case *PendingClaimRequestExport:
			if err = requireMergeProjectID(payload.ProjectID, projectID, payload.ImportKind()); err == nil {
				payload.ID, err = addMergeOffset(payload.ID, offsets.PendingClaim, payload.ImportKind())
				payload.ProjectID = offsets.TargetProjectID
				if err == nil {
					payload.IssueID, err = resolveImportedIssue(
						payload.IssueID, payload.IssueUID, "pending claim",
					)
				}
			}
		case *EventExport:
			if err = requireMergeProjectID(payload.ProjectID, projectID, payload.ImportKind()); err == nil {
				if payload.HLCPhysicalMS > maxProjectMergeHLCValue || payload.HLCCounter > maxProjectMergeHLCValue {
					err = fmt.Errorf("project merge event HLC exceeds safe range")
				}
				// UID, content hash, and payload form the event's portable
				// identity. Only target-local numeric columns are remapped.
				if err == nil {
					payload.ID, err = addMergeOffset(payload.ID, offsets.Event, payload.ImportKind())
				}
				payload.ProjectID = offsets.TargetProjectID
				if err == nil && payload.IssueID != nil {
					*payload.IssueID, err = resolveImportedIssue(
						*payload.IssueID, mergeStringValue(payload.IssueUID), "event",
					)
				}
				if err == nil && payload.RelatedIssueID != nil {
					var resolvedID int64
					resolvedID, err = resolveIssue(*payload.RelatedIssueID, mergeStringValue(payload.RelatedIssueUID))
					if err == nil && resolvedID == 0 {
						payload.RelatedIssueID = nil
					} else if err == nil {
						*payload.RelatedIssueID = resolvedID
					}
				}
			}
		case *PurgeLogExport:
			if err = requireMergeProjectID(payload.ProjectID, projectID, payload.ImportKind()); err == nil {
				payload.ID, err = addMergeOffset(payload.ID, offsets.PurgeLog, payload.ImportKind())
				payload.ProjectID = offsets.TargetProjectID
				if err == nil {
					payload.PurgedIssueID, err = addMergeOffset(payload.PurgedIssueID, offsets.Issue, payload.ImportKind()+" issue")
				}
				if err == nil && payload.PurgedIssueID > issueSequenceFloor {
					issueSequenceFloor = payload.PurgedIssueID
				}
				if err == nil {
					err = shiftEventPointers(
						payload.EventsDeletedMinID,
						payload.EventsDeletedMaxID,
						payload.PurgeResetAfterEventID,
						offsets.Event,
					)
				}
				if err == nil && payload.PurgeResetAfterEventID != nil &&
					*payload.PurgeResetAfterEventID > eventSequenceFloor {
					eventSequenceFloor = *payload.PurgeResetAfterEventID
				}
			}
		case *ProjectPurgeLogExport:
			return nil, fmt.Errorf("project merge does not accept project_purge_log records")
		case *SequenceExport:
			// Scoped exports carry whole-database sequence floors. They do not
			// describe this project. Live imported IDs and the derived tombstone
			// floors below advance target sequences during replay.
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, cloned)
	}
	if issueSequenceFloor > 0 {
		out = append(out, &SequenceExport{
			Name: "issues",
			Seq:  issueSequenceFloor,
		})
	}
	if eventSequenceFloor > 0 {
		out = append(out, &SequenceExport{
			Name: "events",
			Seq:  eventSequenceFloor,
		})
	}
	return out, nil
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
	if offset < 0 || offset > maxProjectMergeID || id > maxProjectMergeID-offset {
		return 0, fmt.Errorf("project merge %s ID exceeds safe ID range", kind)
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
	switch rec := rec.(type) {
	case *MetaKV:
		v := *rec
		return &v
	case *ProjectExport:
		v := *rec
		return &v
	case *AliasExport:
		v := *rec
		return &v
	case *IssueSyncBindingExport:
		v := *rec
		return &v
	case *IssueSyncStatusExport:
		v := *rec
		return &v
	case *RecurrenceExport:
		v := *rec
		return &v
	case *IssueExport:
		v := *rec
		v.RecurrenceID = cloneInt64Ptr(rec.RecurrenceID)
		return &v
	case *IssueEmbeddingExport:
		v := *rec
		return &v
	case *CommentExport:
		v := *rec
		return &v
	case *IssueLabelExport:
		v := *rec
		return &v
	case *LinkExport:
		v := *rec
		return &v
	case *ImportMappingExport:
		v := *rec
		v.IssueID = cloneInt64Ptr(rec.IssueID)
		v.CommentID = cloneInt64Ptr(rec.CommentID)
		v.LinkID = cloneInt64Ptr(rec.LinkID)
		return &v
	case *ExternalFieldMappingExport:
		v := *rec
		v.AcceptedKinds = append([]string(nil), rec.AcceptedKinds...)
		return &v
	case *ExternalRootBindingExport:
		v := *rec
		v.ReceiveCommentsAfter = cloneTimePtr(rec.ReceiveCommentsAfter)
		v.PublishCommentsAfter = cloneTimePtr(rec.PublishCommentsAfter)
		v.PendingCommentStartedAt = cloneTimePtr(rec.PendingCommentStartedAt)
		v.LastAttemptAt = cloneTimePtr(rec.LastAttemptAt)
		v.LastSuccessAt = cloneTimePtr(rec.LastSuccessAt)
		v.LastErrorAt = cloneTimePtr(rec.LastErrorAt)
		v.NextAttemptAt = cloneTimePtr(rec.NextAttemptAt)
		v.UnboundAt = cloneTimePtr(rec.UnboundAt)
		return &v
	case *ExternalFieldStateExport:
		v := *rec
		v.Baseline = append(json.RawMessage(nil), rec.Baseline...)
		v.ConflictKata = append(json.RawMessage(nil), rec.ConflictKata...)
		v.ConflictExternal = append(json.RawMessage(nil), rec.ConflictExternal...)
		v.ConflictAt = cloneTimePtr(rec.ConflictAt)
		return &v
	case *FederationBindingExport:
		v := *rec
		return &v
	case *FederationSyncStatusExport:
		v := *rec
		return &v
	case *FederationQuarantineExport:
		v := *rec
		return &v
	case *FederationEnrollmentExport:
		v := *rec
		v.ProjectID = cloneInt64Ptr(rec.ProjectID)
		return &v
	case *IssueClaimExport:
		v := *rec
		return &v
	case *PendingClaimRequestExport:
		v := *rec
		return &v
	case *EventExport:
		v := *rec
		v.IssueID = cloneInt64Ptr(rec.IssueID)
		v.RelatedIssueID = cloneInt64Ptr(rec.RelatedIssueID)
		return &v
	case *PurgeLogExport:
		v := *rec
		v.EventsDeletedMinID = cloneInt64Ptr(rec.EventsDeletedMinID)
		v.EventsDeletedMaxID = cloneInt64Ptr(rec.EventsDeletedMaxID)
		v.PurgeResetAfterEventID = cloneInt64Ptr(rec.PurgeResetAfterEventID)
		return &v
	case *ProjectPurgeLogExport:
		v := *rec
		return &v
	case *SequenceExport:
		v := *rec
		return &v
	default:
		return rec
	}
}

type externalFieldMappingIdentity struct {
	connectorInstance string
	kataField         string
	externalFieldID   string
	schemaRevision    string
	createdAt         time.Time
}

func externalFieldMappingExportIdentity(mapping *ExternalFieldMappingExport) externalFieldMappingIdentity {
	return externalFieldMappingIdentity{
		connectorInstance: mapping.ConnectorInstance,
		kataField:         mapping.KataField,
		externalFieldID:   mapping.ExternalFieldID,
		schemaRevision:    mapping.SchemaRevision,
		createdAt:         mapping.CreatedAt,
	}
}

func externalFieldStateMappingIdentity(state *ExternalFieldStateExport) externalFieldMappingIdentity {
	return externalFieldMappingIdentity{
		connectorInstance: state.MappingConnectorInstance,
		kataField:         state.MappingKataField,
		externalFieldID:   state.MappingExternalFieldID,
		schemaRevision:    state.MappingSchemaRevision,
		createdAt:         state.MappingCreatedAt,
	}
}

func cloneTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func mergeStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
