package dbtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

const (
	replayProjectUID    = "01HZZZZZZZZZZZZZZZZZZZZZ11"
	replayIssueUID      = "01HZZZZZZZZZZZZZZZZZZZZZ12"
	replayEventUID      = "01HZZZZZZZZZZZZZZZZZZZZZ13"
	replayInstanceUID   = "01HZZZZZZZZZZZZZZZZZZZZZ14"
	replayRecurrenceUID = "01HZZZZZZZZZZZZZZZZZZZZZ15"
	replayCommentUID    = "01HZZZZZZZZZZZZZZZZZZZZZ16"
	replayClaimUID      = "01HZZZZZZZZZZZZZZZZZZZZZ17"
	replayPendingUID    = "01HZZZZZZZZZZZZZZZZZZZZZ18"
	replayPurgeUID      = "01HZZZZZZZZZZZZZZZZZZZZZ19"
	replayProjectPurge  = "01HZZZZZZZZZZZZZZZZZZZZZ1A"
	replaySpokeUID      = "01HZZZZZZZZZZZZZZZZZZZZZ1B"
	replayHubProjectUID = "01HZZZZZZZZZZZZZZZZZZZZZ1C"
	replayBindingUID    = "01HZZZZZZZZZZZZZZZZZZZZZ1D"
)

func checkSnapshotReplayExtendedState(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	records := extendedReplayRecords()
	if err := store.ImportReplay(ctx, records, db.ImportOptions{}); err != nil {
		return fmt.Errorf("import extended replay fixture: %w", err)
	}

	project, err := store.ProjectByUID(ctx, replayProjectUID)
	if err != nil {
		return fmt.Errorf("read extended replay project: %w", err)
	}
	assert.Equal(t, int64(41), project.ID)
	assert.JSONEq(t, `{"team":"example"}`, string(project.Metadata))
	aliases, err := store.ProjectAliases(ctx, project.ID)
	if err != nil {
		return err
	}
	require.Len(t, aliases, 1)
	assert.Equal(t, int64(141), aliases[0].ID)

	recurrence, err := store.GetRecurrenceByUID(ctx, replayRecurrenceUID)
	if err != nil {
		return fmt.Errorf("read replay recurrence: %w", err)
	}
	assert.Equal(t, int64(51), recurrence.ID)
	assert.Equal(t, "FREQ=WEEKLY;BYDAY=MO", recurrence.RRule)
	issue, err := store.IssueByUID(ctx, replayIssueUID, db.IncludeDeletedNo)
	if err != nil {
		return fmt.Errorf("read extended replay issue: %w", err)
	}
	require.NotNil(t, issue.RecurrenceID)
	assert.Equal(t, recurrence.ID, *issue.RecurrenceID)
	comments, err := store.CommentsByIssue(ctx, issue.ID)
	if err != nil {
		return err
	}
	require.Len(t, comments, 1)
	assert.Equal(t, replayCommentUID, comments[0].UID)
	externalBinding, err := store.ExternalRootBindingByIssue(ctx, issue.ID)
	if err != nil {
		return fmt.Errorf("read replay external root binding: %w", err)
	}
	assert.Equal(t, replayBindingUID, externalBinding.UID)
	assert.False(t, externalBinding.Enabled)
	assert.Equal(t, "restore_reconfirmation_required", externalBinding.PausedReason)
	assert.Empty(t, externalBinding.ClaimToken)
	assert.Nil(t, externalBinding.ClaimStartedAt)
	assert.Equal(t, replayCommentUID, externalBinding.PendingCommentUID)
	externalMappings, err := store.ListExternalFieldMappings(ctx, "connector-one")
	if err != nil {
		return fmt.Errorf("read replay external field mappings: %w", err)
	}
	require.Len(t, externalMappings, 1)
	externalStates, err := store.ExternalFieldStates(ctx, externalBinding.ID)
	if err != nil {
		return fmt.Errorf("read replay external field states: %w", err)
	}
	require.Len(t, externalStates, 1)
	assert.Equal(t, externalMappings[0].ID, externalStates[0].MappingID)
	assert.True(t, externalStates[0].Conflicted)
	assert.JSONEq(t, `"2026-08-20"`, string(externalStates[0].Baseline))
	labels, err := store.LabelsByIssue(ctx, issue.ID)
	if err != nil {
		return err
	}
	require.Len(t, labels, 1)
	assert.Equal(t, "restored", labels[0].Label)

	binding, err := store.IssueSyncBindingByProject(ctx, project.ID)
	if err != nil {
		return fmt.Errorf("read replay issue-sync binding: %w", err)
	}
	assert.Equal(t, int64(71), binding.ID)
	assert.False(t, binding.Enabled, "ordinary restores disable external sync")
	status, err := store.IssueSyncStatusByProject(ctx, project.ID)
	if err != nil {
		return fmt.Errorf("read replay issue-sync status: %w", err)
	}
	assert.Equal(t, 2, status.LastCreated)
	assert.Equal(t, "temporary failure", status.LastError)

	federation, err := store.FederationBindingByProject(ctx, project.ID)
	if err != nil {
		return fmt.Errorf("read replay federation binding: %w", err)
	}
	assert.Equal(t, db.FederationRoleSpoke, federation.Role)
	assert.True(t, federation.PushEnabled)
	federationStatus, err := store.FederationSyncStatusByProject(ctx, project.ID)
	if err != nil {
		return fmt.Errorf("read replay federation status: %w", err)
	}
	assert.Equal(t, "connection reset", pointerString(federationStatus.LastError))
	quarantine, err := store.ActiveFederationQuarantine(
		ctx, project.ID, db.FederationQuarantineDirectionPull,
	)
	if err != nil {
		return fmt.Errorf("read replay quarantine: %w", err)
	}
	assert.Equal(t, int64(91), quarantine.ID)
	assert.Equal(t, []string{"event-one", "event-two"}, quarantine.EventUIDs)
	enrollments, err := store.ListFederationEnrollments(ctx)
	if err != nil {
		return err
	}
	require.Len(t, enrollments, 1)
	assert.Equal(t, int64(81), enrollments[0].ID)
	assert.Equal(t, "pull,push", enrollments[0].Capabilities)

	pending, err := store.ListPendingClaimRequests(ctx, project.ID, 10)
	if err != nil {
		return err
	}
	require.Len(t, pending, 1)
	assert.Equal(t, replayPendingUID, pending[0].RequestUID)

	mappings, err := collectExport(store.ExportImportMappings(ctx, db.ExportFilter{}))
	if err != nil {
		return err
	}
	require.Len(t, mappings, 1)
	assert.Equal(t, int64(131), mappings[0].ID)
	assert.Equal(t, "external-42", mappings[0].ExternalID)
	externalMappingExports, err := collectExport(store.ExportExternalFieldMappings(ctx, db.ExportFilter{}))
	if err != nil {
		return err
	}
	require.Len(t, externalMappingExports, 1)
	assert.Equal(t, "schedule-one", externalMappingExports[0].ExternalFieldID)
	assert.Equal(t, time.Date(2026, 7, 15, 12, 0, 0, 123456789, time.UTC), externalMappingExports[0].CreatedAt)
	externalBindingExports, err := collectExport(store.ExportExternalRootBindings(ctx, db.ExportFilter{}))
	if err != nil {
		return err
	}
	require.Len(t, externalBindingExports, 1)
	assert.Equal(t, replayBindingUID, externalBindingExports[0].UID)
	assert.Equal(t, replayProjectUID, externalBindingExports[0].ProjectUID)
	assert.Equal(t, replayIssueUID, externalBindingExports[0].IssueUID)
	require.NotNil(t, externalBindingExports[0].ReceiveCommentsAfter)
	assert.Equal(t, time.Date(2026, 7, 15, 12, 1, 0, 123900000, time.UTC), *externalBindingExports[0].ReceiveCommentsAfter)
	require.NotNil(t, externalBindingExports[0].PublishCommentsAfter)
	assert.Equal(t, time.Date(2026, 7, 15, 12, 2, 0, 456700000, time.UTC), *externalBindingExports[0].PublishCommentsAfter)
	externalStateExports, err := collectExport(store.ExportExternalFieldStates(ctx, db.ExportFilter{}))
	if err != nil {
		return err
	}
	require.Len(t, externalStateExports, 1)
	assert.Equal(t, replayBindingUID, externalStateExports[0].BindingUID)
	assert.Equal(t, externalMappingExports[0].CreatedAt, externalStateExports[0].MappingCreatedAt)
	assert.JSONEq(t, `"2026-08-20"`, string(externalStateExports[0].Baseline))
	purges, err := collectExport(store.ExportPurgeLog(ctx, db.ExportFilter{}))
	if err != nil {
		return err
	}
	require.Len(t, purges, 1)
	assert.Equal(t, replayPurgeUID, purges[0].UID)
	projectPurges, err := collectExport(store.ExportProjectPurgeLog(ctx, db.ExportFilter{}))
	if err != nil {
		return err
	}
	require.Len(t, projectPurges, 1)
	assert.Equal(t, replayProjectPurge, projectPurges[0].UID)

	sequences, err := collectExport(store.ExportSequences(ctx))
	if err != nil {
		return err
	}
	sequenceByName := make(map[string]int64, len(sequences))
	for _, sequence := range sequences {
		sequenceByName[sequence.Name] = sequence.Seq
	}
	for name, floor := range replaySequenceFloors() {
		assert.GreaterOrEqualf(t, sequenceByName[name], floor, "identity floor for %s", name)
	}
	return nil
}

// checkSnapshotReplayRejectsInvalidExternalRootFrontiers verifies that replay
// preserves the create-time synchronization boundary: receive is always
// fronted and publish is fronted whenever publishing is enabled. A malformed
// imported binding must roll back with the rest of the envelope rather than
// leave a row that can later be resumed.
func checkSnapshotReplayRejectsInvalidExternalRootFrontiers(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()

	cases := []struct {
		name    string
		mutate  func([]db.ImportRecord)
		wantErr error
	}{
		{
			name: "missing receive frontier",
			mutate: func(records []db.ImportRecord) {
				externalRootReplayBinding(records).ReceiveCommentsAfter = nil
			},
			wantErr: db.ErrExternalRootValidation,
		},
		{
			name: "missing publish frontier while publishing enabled",
			mutate: func(records []db.ImportRecord) {
				binding := externalRootReplayBinding(records)
				binding.PublishComments = true
				binding.PublishCommentsAfter = nil
			},
			wantErr: db.ErrExternalRootValidation,
		},
		{
			name: "root mapping source does not match connector instance",
			mutate: func(records []db.ImportRecord) {
				externalRootReplayBinding(records).RootMappingSource = "connector:other-connector"
			},
			wantErr: db.ErrExternalRootValidation,
		},
		{
			name: "root mapping external ID does not match root key",
			mutate: func(records []db.ImportRecord) {
				externalRootReplayBinding(records).RootMappingExternalID = "other-root"
			},
			wantErr: db.ErrExternalRootValidation,
		},
		{
			name: "pending comment is missing its start time",
			mutate: func(records []db.ImportRecord) {
				externalRootReplayBinding(records).PendingCommentStartedAt = nil
			},
			wantErr: db.ErrExternalRootValidation,
		},
		{
			name: "pending comment start time has no comment",
			mutate: func(records []db.ImportRecord) {
				externalRootReplayBinding(records).PendingCommentUID = ""
			},
			wantErr: db.ErrExternalRootValidation,
		},
		{
			name: "binding is missing its durable UID",
			mutate: func(records []db.ImportRecord) {
				externalRootReplayBinding(records).UID = ""
			},
			wantErr: db.ErrExternalRootValidation,
		},
		{
			name: "binding has an invalid durable UID",
			mutate: func(records []db.ImportRecord) {
				externalRootReplayBinding(records).UID = "not-a-uid"
			},
			wantErr: db.ErrExternalRootValidation,
		},
		{
			name: "binding is missing its created timestamp",
			mutate: func(records []db.ImportRecord) {
				externalRootReplayBinding(records).CreatedAt = time.Time{}
			},
			wantErr: db.ErrExternalRootValidation,
		},
		{
			name: "binding is missing its updated timestamp",
			mutate: func(records []db.ImportRecord) {
				externalRootReplayBinding(records).UpdatedAt = time.Time{}
			},
			wantErr: db.ErrExternalRootValidation,
		},
		{
			name: "active binding belongs to a read-only federated spoke",
			mutate: func(records []db.ImportRecord) {
				for index := range records {
					if records[index].FederationBinding != nil {
						records[index].FederationBinding.PushEnabled = false
						return
					}
				}
				panic("external root replay fixture has no federation binding")
			},
			wantErr: db.ErrExternalRootFederationConflict,
		},
		{
			name: "active binding targets issue-sync-managed content",
			mutate: func(records []db.ImportRecord) {
				for index := range records {
					if records[index].IssueSyncBinding != nil {
						records[index].IssueSyncBinding.SourceKey = "connector:connector-one"
						return
					}
				}
				panic("external root replay fixture has no issue-sync binding")
			},
			wantErr: db.ErrExternalRootIssueSyncConflict,
		},
		{
			name: "active field mapping is not writable",
			mutate: func(records []db.ImportRecord) {
				externalRootReplayMapping(records).Writable = false
			},
			wantErr: db.ErrExternalFieldMappingValidation,
		},
		{
			name: "field mapping is missing its created timestamp",
			mutate: func(records []db.ImportRecord) {
				externalRootReplayMapping(records).CreatedAt = time.Time{}
			},
			wantErr: db.ErrExternalFieldMappingValidation,
		},
		{
			name: "field mapping is missing its updated timestamp",
			mutate: func(records []db.ImportRecord) {
				externalRootReplayMapping(records).UpdatedAt = time.Time{}
			},
			wantErr: db.ErrExternalFieldMappingValidation,
		},
		{
			name: "field mapping repeats an accepted kind",
			mutate: func(records []db.ImportRecord) {
				externalRootReplayMapping(records).AcceptedKinds = []string{"date", "date"}
			},
			wantErr: db.ErrExternalFieldMappingValidation,
		},
		{
			name: "non-conflicted field state carries candidates",
			mutate: func(records []db.ImportRecord) {
				state := externalRootReplayState(records)
				state.Conflicted = false
				state.ConflictAt = nil
			},
			wantErr: db.ErrExternalRootValidation,
		},
		{
			name: "conflicted field state is missing conflict time",
			mutate: func(records []db.ImportRecord) {
				externalRootReplayState(records).ConflictAt = nil
			},
			wantErr: db.ErrExternalRootValidation,
		},
		{
			name: "field state mapping belongs to another connector",
			mutate: func(records []db.ImportRecord) {
				externalRootReplayMapping(records).ConnectorInstance = "connector-two"
				externalRootReplayState(records).MappingConnectorInstance = "connector-two"
			},
			wantErr: db.ErrExternalRootValidation,
		},
		{
			name: "comment mapping points to another issue's comment",
			mutate: func(records []db.ImportRecord) {
				projectID, issueID, otherIssueID, otherCommentID := int64(41), int64(61), int64(62), int64(122)
				replacements := map[string]db.ImportRecord{
					db.ImportKindProjectAlias: {Kind: db.ImportKindIssue, Issue: &db.IssueExport{
						ID: otherIssueID, UID: "01HZZZZZZZZZZZZZZZZZZZZZ2A", ProjectID: projectID,
						ShortID: "zz2a", Title: "Other replay issue", Status: "open",
						Author: "fixture-author", CreatedAt: "2026-07-15T12:00:00.000Z",
						UpdatedAt: "2026-07-15T12:00:00.000Z", Metadata: json.RawMessage(`{}`), Revision: 1,
					}},
					db.ImportKindIssueEmbedding: {Kind: db.ImportKindComment, Comment: &db.CommentExport{
						ID: otherCommentID, UID: "01HZZZZZZZZZZZZZZZZZZZZZ2B", IssueID: otherIssueID,
						Author: "fixture-author", Body: "Other replay comment", CreatedAt: "2026-07-15T12:00:00.000Z",
					}},
					db.ImportKindIssueSyncStatus: {Kind: db.ImportKindImportMapping, ImportMapping: &db.ImportMappingExport{
						ID: 132, Source: "connector:connector-one:comments:external-42",
						ExternalID: "cross-issue-comment", ObjectType: "comment", ProjectID: projectID,
						IssueID: &issueID, CommentID: &otherCommentID, ImportedAt: "2026-07-15T12:00:00.000Z",
					}},
				}
				for index := range records {
					originalKind := records[index].Kind
					if replacement, ok := replacements[originalKind]; ok {
						records[index] = replacement
						delete(replacements, originalKind)
					}
				}
			},
			wantErr: db.ErrExternalRootValidation,
		},
		{
			name: "binding-scoped comment mapping belongs to another issue",
			mutate: func(records []db.ImportRecord) {
				projectID, otherIssueID, otherCommentID := int64(41), int64(62), int64(122)
				replacements := map[string]db.ImportRecord{
					db.ImportKindProjectAlias: {Kind: db.ImportKindIssue, Issue: &db.IssueExport{
						ID: otherIssueID, UID: "01HZZZZZZZZZZZZZZZZZZZZZ2A", ProjectID: projectID,
						ShortID: "zz2a", Title: "Other replay issue", Status: "open",
						Author: "fixture-author", CreatedAt: "2026-07-15T12:00:00.000Z",
						UpdatedAt: "2026-07-15T12:00:00.000Z", Metadata: json.RawMessage(`{}`), Revision: 1,
					}},
					db.ImportKindIssueEmbedding: {Kind: db.ImportKindComment, Comment: &db.CommentExport{
						ID: otherCommentID, UID: "01HZZZZZZZZZZZZZZZZZZZZZ2B", IssueID: otherIssueID,
						Author: "fixture-author", Body: "Other replay comment", CreatedAt: "2026-07-15T12:00:00.000Z",
					}},
					db.ImportKindIssueSyncStatus: {Kind: db.ImportKindImportMapping, ImportMapping: &db.ImportMappingExport{
						ID: 132, Source: "connector:connector-one:binding:" + replayBindingUID,
						ExternalID: "cross-binding-comment", ObjectType: "comment", ProjectID: projectID,
						IssueID: &otherIssueID, CommentID: &otherCommentID, ImportedAt: "2026-07-15T12:00:00.000Z",
					}},
				}
				for index := range records {
					originalKind := records[index].Kind
					if replacement, ok := replacements[originalKind]; ok {
						records[index] = replacement
						delete(replacements, originalKind)
					}
				}
			},
			wantErr: db.ErrExternalRootValidation,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			records := extendedReplayRecords()
			test.mutate(records)

			err := store.ImportReplay(ctx, records, db.ImportOptions{})
			require.ErrorIs(t, err, test.wantErr)
			_, err = store.ProjectByUID(ctx, replayProjectUID)
			require.True(t, errors.Is(err, db.ErrNotFound), "invalid replay must roll back the envelope")
		})
	}
	return nil
}

// checkSnapshotReplayRejectsInvalidBindingMappings verifies that every mapping
// namespace derived from a binding UID remains attached to that binding's
// issue. These records are portable replay input, so the envelope must be
// rejected before a malformed namespace can poison revision or ownership
// lookups.
func checkSnapshotReplayRejectsInvalidBindingMappings(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()

	cases := []struct {
		name       string
		source     string
		objectType string
		comment    bool
		crossIssue bool
	}{
		{
			name:       "inbound comment",
			source:     "connector:connector-one:binding:" + replayBindingUID,
			objectType: "comment",
			comment:    true,
			crossIssue: true,
		},
		{
			name:       "lifecycle comment",
			source:     "connector:connector-one:binding:" + replayBindingUID + ":lifecycle",
			objectType: "comment",
			comment:    true,
			crossIssue: true,
		},
		{
			name:       "published comment",
			source:     "connector:connector-one:binding:" + replayBindingUID + ":published",
			objectType: "comment",
			comment:    true,
			crossIssue: true,
		},
		{
			name:       "comment revision",
			source:     "connector:connector-one:binding:" + replayBindingUID + ":revisions",
			objectType: "issue",
			crossIssue: true,
		},
		{
			name:       "root revision",
			source:     "connector:connector-one:binding:" + replayBindingUID + ":root-revisions",
			objectType: "issue",
			crossIssue: true,
		},
		{
			name:       "lifecycle comment has issue shape",
			source:     "connector:connector-one:binding:" + replayBindingUID + ":lifecycle",
			objectType: "issue",
		},
		{
			name:       "published comment has issue shape",
			source:     "connector:connector-one:binding:" + replayBindingUID + ":published",
			objectType: "issue",
		},
		{
			name:       "comment revision has comment shape",
			source:     "connector:connector-one:binding:" + replayBindingUID + ":revisions",
			objectType: "comment",
			comment:    true,
		},
		{
			name:       "root revision has comment shape",
			source:     "connector:connector-one:binding:" + replayBindingUID + ":root-revisions",
			objectType: "comment",
			comment:    true,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			records := extendedReplayRecords()
			installBindingMapping(records, test.source, test.objectType, test.comment, test.crossIssue)

			err := store.ImportReplay(ctx, records, db.ImportOptions{})
			require.ErrorIs(t, err, db.ErrExternalRootValidation)
			_, err = store.ProjectByUID(ctx, replayProjectUID)
			require.True(t, errors.Is(err, db.ErrNotFound), "invalid replay must not mutate the target")
		})
	}
	return nil
}

func checkSnapshotReplayRejectsDuplicateFieldMappingIdentities(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	records := extendedReplayRecords()
	original := *externalRootReplayMapping(records)
	duplicate := original
	duplicate.Active = false

	for index := range records {
		if records[index].Kind == db.ImportKindProjectAlias {
			records[index] = db.ImportRecord{
				Kind:                 db.ImportKindExternalFieldMapping,
				ExternalFieldMapping: &duplicate,
			}
			break
		}
	}

	err := store.ImportReplay(ctx, records, db.ImportOptions{})
	require.ErrorIs(t, err, db.ErrExternalFieldMappingValidation)
	_, err = store.ProjectByUID(ctx, replayProjectUID)
	require.True(t, errors.Is(err, db.ErrNotFound), "ambiguous replay must not mutate the target")
	return nil
}

func checkSnapshotReplayPreservesSubmicrosecondFieldMappingIdentities(
	t *testing.T,
	store db.Storage,
) error {
	t.Helper()
	ctx := context.Background()
	records := extendedReplayRecords()
	original := externalRootReplayMapping(records)
	adjacent := *original
	adjacent.Active = false
	adjacent.CreatedAt = original.CreatedAt.Add(time.Nanosecond)

	for index := range records {
		if records[index].Kind == db.ImportKindProjectAlias {
			records[index] = db.ImportRecord{
				Kind:                 db.ImportKindExternalFieldMapping,
				ExternalFieldMapping: &adjacent,
			}
			break
		}
	}
	externalRootReplayState(records).MappingCreatedAt = original.CreatedAt

	if err := store.ImportReplay(ctx, records, db.ImportOptions{}); err != nil {
		return fmt.Errorf("import submicrosecond field mapping identities: %w", err)
	}
	issue, err := store.IssueByUID(ctx, replayIssueUID, db.IncludeDeletedNo)
	if err != nil {
		return err
	}
	binding, err := store.ExternalRootBindingByIssue(ctx, issue.ID)
	if err != nil {
		return err
	}
	mappings, err := store.ListExternalFieldMappings(ctx, original.ConnectorInstance)
	if err != nil {
		return err
	}
	require.Len(t, mappings, 2)
	var originalMappingID int64
	for _, mapping := range mappings {
		if mapping.CreatedAt.Equal(original.CreatedAt) {
			originalMappingID = mapping.ID
		}
	}
	require.NotZero(t, originalMappingID)
	states, err := store.ExternalFieldStates(ctx, binding.ID)
	if err != nil {
		return err
	}
	require.Len(t, states, 1)
	assert.Equal(t, originalMappingID, states[0].MappingID)
	return nil
}

func installBindingMapping(
	records []db.ImportRecord,
	source string,
	objectType string,
	withComment bool,
	crossIssue bool,
) {
	const (
		projectID      = int64(41)
		bindingIssue   = int64(61)
		bindingComment = int64(121)
		otherIssueID   = int64(62)
		otherComment   = int64(122)
	)
	issueID := bindingIssue
	commentID := bindingComment
	replacements := map[string]db.ImportRecord{
		db.ImportKindIssueSyncStatus: {Kind: db.ImportKindImportMapping, ImportMapping: &db.ImportMappingExport{
			ID: 132, Source: source, ExternalID: "cross-binding-object", ObjectType: objectType,
			ProjectID: projectID, IssueID: &issueID, ImportedAt: "2026-07-15T12:00:00.000Z",
		}},
	}
	if crossIssue {
		issueID = otherIssueID
		commentID = otherComment
		replacements[db.ImportKindProjectAlias] = db.ImportRecord{Kind: db.ImportKindIssue, Issue: &db.IssueExport{
			ID: otherIssueID, UID: "01HZZZZZZZZZZZZZZZZZZZZZ2A", ProjectID: projectID,
			ShortID: "zz2a", Title: "Other replay issue", Status: "open",
			Author: "fixture-author", CreatedAt: "2026-07-15T12:00:00.000Z",
			UpdatedAt: "2026-07-15T12:00:00.000Z", Metadata: json.RawMessage(`{}`), Revision: 1,
		}}
	}
	if withComment {
		if crossIssue {
			replacements[db.ImportKindIssueEmbedding] = db.ImportRecord{Kind: db.ImportKindComment, Comment: &db.CommentExport{
				ID: otherComment, UID: "01HZZZZZZZZZZZZZZZZZZZZZ2B", IssueID: otherIssueID,
				Author: "fixture-author", Body: "Other replay comment", CreatedAt: "2026-07-15T12:00:00.000Z",
			}}
		}
		replacements[db.ImportKindIssueSyncStatus].ImportMapping.CommentID = &commentID
	}
	for index := range records {
		originalKind := records[index].Kind
		if replacement, ok := replacements[originalKind]; ok {
			records[index] = replacement
			delete(replacements, originalKind)
		}
	}
}

func externalRootReplayBinding(records []db.ImportRecord) *db.ExternalRootBindingExport {
	for _, record := range records {
		if record.Kind == db.ImportKindExternalRootBinding {
			return record.ExternalRootBinding
		}
	}
	panic("external root replay fixture has no binding")
}

func externalRootReplayMapping(records []db.ImportRecord) *db.ExternalFieldMappingExport {
	for _, record := range records {
		if record.Kind == db.ImportKindExternalFieldMapping {
			return record.ExternalFieldMapping
		}
	}
	panic("external root replay fixture has no field mapping")
}

func externalRootReplayState(records []db.ImportRecord) *db.ExternalFieldStateExport {
	for _, record := range records {
		if record.Kind == db.ImportKindExternalFieldState {
			return record.ExternalFieldState
		}
	}
	panic("external root replay fixture has no field state")
}

func extendedReplayRecords() []db.ImportRecord {
	projectID := int64(41)
	issueID := int64(61)
	commentID := int64(121)
	recurrenceID := int64(51)
	created := "2026-07-15T12:00:00.000Z"
	createdTime := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	mappingCreatedAt := createdTime.Add(123456789 * time.Nanosecond)
	receiveAfter := createdTime.Add(time.Minute + 123900*time.Microsecond)
	publishAfter := createdTime.Add(2*time.Minute + 456700*time.Microsecond)
	pendingStarted := createdTime.Add(3 * time.Minute)
	lastAttempt := createdTime.Add(4 * time.Minute)
	lastSuccess := createdTime.Add(5 * time.Minute)
	externalLastErrorAt := createdTime.Add(6 * time.Minute)
	nextAttempt := createdTime.Add(10 * time.Minute)
	conflictAt := createdTime.Add(7 * time.Minute)
	lastError := "temporary failure"
	issueSyncLastErrorAt := "2026-07-15T12:05:00.000Z"
	projectUID := replayProjectUID
	issueUID := replayIssueUID
	shortID := "zz12"
	purgeShortID := "zz1e"
	reason := "retention"
	projectPurgeUID := "01HZZZZZZZZZZZZZZZZZZZZZ1D"
	projectPurgeID := int64(90)
	records := []db.ImportRecord{
		{Kind: db.ImportKindMeta, Meta: &db.MetaKV{Key: "instance_uid", Value: replayInstanceUID}},
		{Kind: db.ImportKindProject, Project: &db.ProjectExport{
			ID: projectID, UID: projectUID, Name: "extended-replay", CreatedAt: created,
			Metadata: json.RawMessage(`{"team":"example"}`), Revision: 3,
		}},
		{Kind: db.ImportKindProjectAlias, Alias: &db.AliasExport{
			ID: 141, ProjectID: projectID, AliasIdentity: "example/extended", AliasKind: "git", CreatedAt: created,
		}},
		{Kind: db.ImportKindIssueSyncBinding, IssueSyncBinding: &db.IssueSyncBindingExport{
			ID: 71, ProjectID: projectID, Provider: "example", SourceKey: "example:42",
			RemoteID: "42", DisplayName: "Example tracker", Config: json.RawMessage(`{"mode":"mirror"}`),
			Enabled: true, IntervalSeconds: 300, CreatedAt: created, UpdatedAt: created,
		}},
		{Kind: db.ImportKindIssueSyncStatus, IssueSyncStatus: &db.IssueSyncStatusExport{
			BindingID: 71, ProjectID: projectID, LastErrorAt: &issueSyncLastErrorAt, LastError: &lastError,
			LastCreated: 2, LastUpdated: 3, LastUnchanged: 4, LastComments: 5,
		}},
		{Kind: db.ImportKindRecurrence, Recurrence: &db.RecurrenceExport{
			ID: recurrenceID, UID: replayRecurrenceUID, ProjectID: projectID,
			RRule: "FREQ=WEEKLY;BYDAY=MO", DTStart: "2026-07-20", Timezone: "UTC",
			TemplateTitle: "Weekly review", TemplateBody: "Review progress",
			TemplateLabels: json.RawMessage(`["weekly"]`), TemplateMetadata: json.RawMessage(`{"cadence":"weekly"}`),
			Author: "scheduler", Revision: 1, CreatedAt: created, UpdatedAt: created,
		}},
		{Kind: db.ImportKindIssue, Issue: &db.IssueExport{
			ID: issueID, UID: issueUID, ProjectID: projectID, ShortID: shortID,
			Title: "Restored recurring issue", Body: "durable state", Status: "open",
			Author: "fixture-author", CreatedAt: created, UpdatedAt: created,
			Metadata: json.RawMessage(`{"source":"snapshot"}`), Revision: 2, ContentRevision: 1,
			RecurrenceID: &recurrenceID, RecurrenceUID: new(replayRecurrenceUID),
		}},
		{Kind: db.ImportKindIssueEmbedding, IssueEmbedding: &db.IssueEmbeddingExport{
			IssueUID: issueUID, EmbeddedContentRevision: 1, Fingerprint: "legacy-vector",
			Dims: 2, VectorB64: "AAAAAAAAgD8=",
		}},
		{Kind: db.ImportKindComment, Comment: &db.CommentExport{
			ID: commentID, UID: replayCommentUID, IssueID: issueID, Author: "reviewer",
			Body: "restored comment", CreatedAt: created,
		}},
		{Kind: db.ImportKindIssueLabel, Label: &db.IssueLabelExport{
			IssueID: issueID, Label: "restored", Author: "fixture-author", CreatedAt: created,
		}},
		{Kind: db.ImportKindImportMapping, ImportMapping: &db.ImportMappingExport{
			ID: 131, Source: "connector:connector-one", ExternalID: "external-42", ObjectType: "issue",
			ProjectID: projectID, IssueID: &issueID, ImportedAt: created,
		}},
		{Kind: db.ImportKindExternalFieldMapping, ExternalFieldMapping: &db.ExternalFieldMappingExport{
			ConnectorInstance: "connector-one", KataField: "scheduled_on",
			ExternalFieldID: "schedule-one", ExternalFieldName: "Schedule",
			AcceptedKinds: []string{"date"}, Nullable: true, Writable: true,
			SchemaRevision: "schema-one", Active: true,
			CreatedAt: mappingCreatedAt, UpdatedAt: mappingCreatedAt,
		}},
		{Kind: db.ImportKindExternalRootBinding, ExternalRootBinding: &db.ExternalRootBindingExport{
			UID: replayBindingUID, ProjectUID: projectUID, IssueUID: issueUID,
			RootMappingSource: "connector:connector-one", RootMappingExternalID: "external-42",
			ConnectorInstance: "connector-one", ExternalRootKey: "external-42",
			ExternalAccountKey: "opaque-account", Active: true, Enabled: true,
			ReceiveComments: true, PublishComments: true, CompleteExternal: true,
			ReceiveCommentsAfter: &receiveAfter, PublishCommentsAfter: &publishAfter,
			PendingCommentUID: replayCommentUID, PendingCommentStartedAt: &pendingStarted,
			LastAttemptAt: &lastAttempt, LastSuccessAt: &lastSuccess, LastErrorAt: &externalLastErrorAt,
			LastError: "temporary connector failure", ConsecutiveFailures: 1,
			NextAttemptAt: &nextAttempt, CreatedAt: createdTime, UpdatedAt: externalLastErrorAt,
		}},
		{Kind: db.ImportKindExternalFieldState, ExternalFieldState: &db.ExternalFieldStateExport{
			BindingUID: replayBindingUID, MappingConnectorInstance: "connector-one",
			MappingKataField: "scheduled_on", MappingExternalFieldID: "schedule-one",
			MappingSchemaRevision: "schema-one", MappingCreatedAt: mappingCreatedAt,
			Baseline:         json.RawMessage(`"2026-08-20"`),
			ConflictKata:     json.RawMessage(`"2026-08-21"`),
			ConflictExternal: json.RawMessage(`"2026-08-22"`),
			Conflicted:       true, ConflictAt: &conflictAt, UpdatedAt: conflictAt,
		}},
		{Kind: db.ImportKindFederationBinding, FederationBinding: &db.FederationBindingExport{
			ProjectID: projectID, Role: "spoke", HubURL: "https://hub.example",
			HubProjectID: 7, HubProjectUID: replayHubProjectUID, ReplayHorizonEventID: 10,
			PullCursorEventID: 11, PushEnabled: true, PushCursorEventID: 12,
			Actor: "sync-agent", Enabled: true, CreatedAt: created, UpdatedAt: created,
		}},
		{Kind: db.ImportKindFederationSyncStatus, FederationSyncStatus: &db.FederationSyncStatusExport{
			ProjectID: projectID, LastErrorAt: &issueSyncLastErrorAt, LastError: new("connection reset"),
		}},
		{Kind: db.ImportKindFederationQuarantine, FederationQuarantine: &db.FederationQuarantineExport{
			ID: 91, ProjectID: projectID, Direction: "pull", FirstEventID: 20, LastEventID: 21,
			EventUIDs: json.RawMessage(`["event-one","event-two"]`), Error: "invalid remote event", CreatedAt: created,
		}},
		{Kind: db.ImportKindFederationEnrollment, FederationEnrollment: &db.FederationEnrollmentExport{
			ID: 81, TokenHash: strings.Repeat("a", 64), SpokeInstanceUID: replaySpokeUID,
			ProjectID: &projectID, Capabilities: "pull,push", Actor: "sync-agent",
			CreatedAt: created, UpdatedAt: created,
		}},
		{Kind: db.ImportKindIssueClaim, IssueClaim: &db.IssueClaimExport{
			ID: 101, ClaimUID: replayClaimUID, ProjectID: projectID, IssueID: issueID,
			IssueUID: issueUID, Holder: "worker", HolderInstanceUID: replaySpokeUID,
			ClientKind: "agent", Purpose: "implementation", ClaimKind: "hard",
			AcquiredAt: created, Revision: 1, UpdatedAt: created,
		}},
		{Kind: db.ImportKindPendingClaimRequest, PendingClaimRequest: &db.PendingClaimRequestExport{
			ID: 111, RequestUID: replayPendingUID, ProjectID: projectID, IssueID: issueID,
			IssueUID: issueUID, Holder: "reviewer", HolderInstanceUID: replayInstanceUID,
			ClientKind: "cli", ClaimKind: "hard", Purpose: "review", RequestedAt: created,
		}},
		{Kind: db.ImportKindPurgeLog, PurgeLog: &db.PurgeLogExport{
			ID: 161, UID: replayPurgeUID, OriginInstanceUID: replayInstanceUID,
			ProjectID: projectID, PurgedIssueID: 160, IssueUID: new("01HZZZZZZZZZZZZZZZZZZZZZ1E"),
			ProjectUID: &projectUID, ProjectName: "extended-replay", ShortID: &purgeShortID,
			IssueTitle: "Purged issue", IssueAuthor: "fixture-author", EventCount: 2,
			PurgeResetAfterEventID: new(int64(100)), Actor: "operator", Reason: &reason, PurgedAt: created,
		}},
		{Kind: db.ImportKindProjectPurgeLog, ProjectPurgeLog: &db.ProjectPurgeLogExport{
			ID: 151, UID: replayProjectPurge, OriginInstanceUID: replayInstanceUID,
			ProjectID: projectPurgeID, ProjectUID: &projectPurgeUID, ProjectName: "retired-project",
			IssueCount: 2, EventCount: 3, AliasCount: 1, CommentCount: 1, LinkCount: 1,
			LabelCount: 1, ClaimCount: 1, PendingClaimRequestCount: 1,
			PurgeResetAfterEventID: new(int64(101)), Actor: "operator", Reason: &reason, PurgedAt: created,
		}},
	}
	for name, floor := range replaySequenceFloors() {
		sequence := db.SequenceExport{Name: name, Seq: floor}
		records = append(records, db.ImportRecord{Kind: db.ImportKindSQLiteSequence, Sequence: &sequence})
	}
	return records
}

func replaySequenceFloors() map[string]int64 {
	return map[string]int64{
		"projects":                500,
		"project_aliases":         600,
		"issue_sync_bindings":     700,
		"issues":                  800,
		"comments":                900,
		"links":                   1000,
		"import_mappings":         1100,
		"events":                  1200,
		"purge_log":               1300,
		"project_purge_log":       1400,
		"api_tokens":              1500,
		"federation_quarantine":   1600,
		"federation_enrollments":  1700,
		"issue_claims":            1800,
		"pending_claim_requests":  1900,
		"external_root_bindings":  2000,
		"external_field_mappings": 2100,
	}
}

func checkSnapshotReplayCompatibilityOptions(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	localInstanceUID := store.InstanceUID()
	created := "2026-07-15T12:00:00.000Z"
	createdTime := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	receiveAfter := createdTime.Add(time.Minute)
	pendingStarted := createdTime.Add(2 * time.Minute)
	projectID := int64(5)
	issueID := int64(6)
	recurrenceRecords := []db.ImportRecord{
		{Kind: db.ImportKindMeta, Meta: &db.MetaKV{Key: "instance_uid", Value: replayInstanceUID}},
		{Kind: db.ImportKindProject, Project: &db.ProjectExport{
			ID: projectID, UID: replayProjectUID, Name: "compatibility-replay", CreatedAt: created,
			Metadata: json.RawMessage(`{}`), Revision: 1,
		}},
		{Kind: db.ImportKindIssueSyncBinding, IssueSyncBinding: &db.IssueSyncBindingExport{
			ID: 7, ProjectID: projectID, Provider: "example", SourceKey: "example:compat",
			RemoteID: "compat", DisplayName: "Compatibility source", Config: json.RawMessage(`{}`),
			Enabled: true, IntervalSeconds: 60, CreatedAt: created, UpdatedAt: created,
		}},
		{Kind: db.ImportKindIssue, Issue: &db.IssueExport{
			ID: issueID, UID: replayIssueUID, ProjectID: projectID, ShortID: "zz12",
			Title: "Legacy replay issue", Status: "open", Author: "fixture-author",
			CreatedAt: created, UpdatedAt: created, Metadata: json.RawMessage(`{}`), Revision: 1,
		}},
		{Kind: db.ImportKindComment, Comment: &db.CommentExport{
			ID: 10, UID: replayCommentUID, IssueID: issueID, Author: "fixture-author",
			Body: "pending external comment", CreatedAt: created,
		}},
		{Kind: db.ImportKindImportMapping, ImportMapping: &db.ImportMappingExport{
			ID: 11, Source: "connector:connector-cutover", ExternalID: "root-cutover",
			ObjectType: "issue", ProjectID: projectID, IssueID: &issueID, ImportedAt: created,
		}},
		{Kind: db.ImportKindExternalRootBinding, ExternalRootBinding: &db.ExternalRootBindingExport{
			UID: replayBindingUID, ProjectUID: replayProjectUID, IssueUID: replayIssueUID,
			RootMappingSource: "connector:connector-cutover", RootMappingExternalID: "root-cutover",
			ConnectorInstance: "connector-cutover", ExternalRootKey: "root-cutover",
			ExternalAccountKey: "opaque-account", Active: true, Enabled: true,
			ReceiveComments: true, CompleteExternal: true, ReceiveCommentsAfter: &receiveAfter,
			PendingCommentUID: replayCommentUID, PendingCommentStartedAt: &pendingStarted,
			CreatedAt: createdTime, UpdatedAt: pendingStarted,
		}},
		{Kind: db.ImportKindPendingClaimRequest, PendingClaimRequest: &db.PendingClaimRequestExport{
			ID: 8, RequestUID: replayPendingUID, ProjectID: projectID, IssueID: issueID,
			IssueUID: replayIssueUID, Holder: "worker", HolderInstanceUID: replaySpokeUID,
			ClientKind: "agent", ClaimKind: "hard", RequestedAt: created,
		}},
		{Kind: db.ImportKindPendingClaimRequest, PendingClaimRequest: &db.PendingClaimRequestExport{
			ID: 9, RequestUID: "01HZZZZZZZZZZZZZZZZZZZZZ1F", ProjectID: projectID, IssueID: issueID,
			IssueUID: replayIssueUID, Holder: "worker", HolderInstanceUID: replaySpokeUID,
			ClientKind: "agent", ClaimKind: "hard", RequestedAt: "2026-07-15T12:01:00.000Z",
		}},
		{Kind: db.ImportKindEvent, Event: &db.EventExport{
			ID: 10, UID: replayEventUID, OriginInstanceUID: replayInstanceUID,
			ProjectID: projectID, ProjectUID: replayProjectUID, ProjectName: "compatibility-replay",
			IssueID: &issueID, Type: "issue.created", Actor: "fixture-author",
			Payload:       json.RawMessage(`{"title":"Imported replay event"}`),
			HLCPhysicalMS: 1784102400000, HLCCounter: 0, ContentHash: "legacy-hash",
			CreatedAt: created,
		}},
	}
	if err := store.ImportReplay(ctx, recurrenceRecords, db.ImportOptions{
		NewInstance:                         true,
		DedupeLegacyActivePendingClaims:     true,
		RecomputeEventContentHash:           true,
		PreserveIssueSyncBindingEnabled:     true,
		PreserveExternalRootBindingsEnabled: true,
	}); err != nil {
		return fmt.Errorf("import compatibility replay fixture: %w", err)
	}
	assert.Equal(t, localInstanceUID, store.InstanceUID())
	project, err := store.ProjectByUID(ctx, replayProjectUID)
	if err != nil {
		return err
	}
	binding, err := store.IssueSyncBindingByProject(ctx, project.ID)
	if err != nil {
		return err
	}
	assert.True(t, binding.Enabled)
	externalBinding, err := store.ExternalRootBindingByIssue(ctx, issueID)
	if err != nil {
		return fmt.Errorf("read preserved external root binding: %w", err)
	}
	assert.True(t, externalBinding.Enabled)
	assert.Empty(t, externalBinding.PausedReason)
	assert.Empty(t, externalBinding.ClaimToken)
	assert.Nil(t, externalBinding.ClaimStartedAt)
	assert.Equal(t, replayCommentUID, externalBinding.PendingCommentUID)
	assert.Equal(t, &pendingStarted, externalBinding.PendingCommentStartedAt)
	pending, err := store.ListPendingClaimRequests(ctx, project.ID, 10)
	if err != nil {
		return err
	}
	require.Len(t, pending, 1)
	assert.Equal(t, replayPendingUID, pending[0].RequestUID)
	events, err := store.EventsByUIDs(ctx, project.ID, []string{replayEventUID})
	if err != nil {
		return err
	}
	require.Len(t, events, 1)
	assert.Equal(t, replayIssueUID, pointerString(events[0].IssueUID))
	assert.Equal(t, "cdf3c90ed43f0325e610c2db3d0ec8666b168bc74e51f6429a324cf711c95ffa",
		events[0].ContentHash)
	return nil
}

func checkSnapshotReplayHistoricalProjectName(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	const (
		currentName    = "renamed-project"
		historicalName = "original-project"
		created        = "2026-07-15T12:00:00.000Z"
	)
	payload := json.RawMessage(`{"name":"original-project"}`)
	hash, err := db.EventContentHash(db.EventHashInput{
		UID: replayEventUID, OriginInstanceUID: replayInstanceUID,
		ProjectUID: replayProjectUID, ProjectName: historicalName,
		Type: "project.created", Actor: "fixture-author",
		HLCPhysicalMS: 1784102400000, CreatedAt: created, Payload: payload,
	})
	if err != nil {
		return err
	}
	err = store.ImportReplay(ctx, []db.ImportRecord{
		{Kind: db.ImportKindProject, Project: &db.ProjectExport{
			ID: 5, UID: replayProjectUID, Name: currentName, CreatedAt: created,
			Metadata: json.RawMessage(`{}`), Revision: 1,
		}},
		{Kind: db.ImportKindEvent, Event: &db.EventExport{
			ID: 10, UID: replayEventUID, OriginInstanceUID: replayInstanceUID,
			ProjectID: 5, ProjectUID: replayProjectUID, ProjectName: historicalName,
			Type: "project.created", Actor: "fixture-author", Payload: payload,
			HLCPhysicalMS: 1784102400000, ContentHash: hash, CreatedAt: created,
		}},
	}, db.ImportOptions{})
	if err != nil {
		return err
	}
	project, err := store.ProjectByUID(ctx, replayProjectUID)
	if err != nil {
		return err
	}
	assert.Equal(t, currentName, project.Name)
	events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 10})
	if err != nil {
		return err
	}
	require.Len(t, events, 1)
	assert.Equal(t, historicalName, events[0].ProjectName)
	assert.Equal(t, hash, events[0].ContentHash)
	return nil
}

func checkSnapshotReplayUnsafeHistoricalProjectName(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	const (
		unsafeName = "original\nproject"
		created    = "2026-07-15T12:00:00.000Z"
	)
	payload := json.RawMessage(`{"name":"original project"}`)
	hash, err := db.EventContentHash(db.EventHashInput{
		UID: replayEventUID, OriginInstanceUID: replayInstanceUID,
		ProjectUID: replayProjectUID, ProjectName: unsafeName,
		Type: "project.created", Actor: "fixture-author",
		HLCPhysicalMS: 1784102400000, CreatedAt: created, Payload: payload,
	})
	if err != nil {
		return err
	}
	err = store.ImportReplay(ctx, []db.ImportRecord{
		{Kind: db.ImportKindProject, Project: &db.ProjectExport{
			ID: 5, UID: replayProjectUID, Name: "safe-current-name", CreatedAt: created,
			Metadata: json.RawMessage(`{}`), Revision: 1,
		}},
		{Kind: db.ImportKindEvent, Event: &db.EventExport{
			ID: 10, UID: replayEventUID, OriginInstanceUID: replayInstanceUID,
			ProjectID: 5, ProjectUID: replayProjectUID, ProjectName: unsafeName,
			Type: "project.created", Actor: "fixture-author", Payload: payload,
			HLCPhysicalMS: 1784102400000, ContentHash: hash, CreatedAt: created,
		}},
	}, db.ImportOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-printable")
	projects, listErr := store.ListProjects(ctx)
	if listErr != nil {
		return listErr
	}
	assert.Empty(t, projects, "unsafe durable event name must roll back replay")
	return nil
}

func checkSnapshotReplayAtomicRejection(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	localInstanceUID := store.InstanceUID()
	created := "2026-07-15T12:00:00.000Z"
	records := []db.ImportRecord{
		{Kind: db.ImportKindMeta, Meta: &db.MetaKV{Key: "instance_uid", Value: replayInstanceUID}},
		{Kind: db.ImportKindProject, Project: &db.ProjectExport{
			ID: 2, UID: replayProjectUID, Name: "first-project", CreatedAt: created,
			Metadata: json.RawMessage(`{}`), Revision: 1,
		}},
		{Kind: db.ImportKindProject, Project: &db.ProjectExport{
			ID: 3, UID: replayProjectUID, Name: "duplicate-project", CreatedAt: created,
			Metadata: json.RawMessage(`{}`), Revision: 1,
		}},
	}
	err := store.ImportReplay(ctx, records, db.ImportOptions{})
	require.Error(t, err)
	assert.Equal(t, localInstanceUID, store.InstanceUID())
	projects, listErr := store.ListProjects(ctx)
	if listErr != nil {
		return listErr
	}
	assert.Empty(t, projects, "failed replay must roll back every user project")
	if _, systemErr := store.SystemProject(ctx); systemErr != nil {
		return fmt.Errorf("failed replay removed target system project: %w", systemErr)
	}

	err = store.ImportReplay(ctx, []db.ImportRecord{
		{Kind: db.ImportKindMeta, Meta: &db.MetaKV{Key: "instance_uid", Value: replayInstanceUID}},
		{Kind: db.ImportKindProject},
	}, db.ImportOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "import record 1")
	assert.Contains(t, err.Error(), "no payload set")
	projects, listErr = store.ListProjects(ctx)
	if listErr != nil {
		return listErr
	}
	assert.Empty(t, projects)

	err = store.ImportReplay(ctx, []db.ImportRecord{{
		Kind: db.ImportKindProject,
		Project: &db.ProjectExport{
			ID: 4, UID: replayProjectUID, Name: "unsafe\nproject", CreatedAt: created,
			Metadata: json.RawMessage(`{}`), Revision: 1,
		},
	}}, db.ImportOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-printable")
	projects, listErr = store.ListProjects(ctx)
	if listErr != nil {
		return listErr
	}
	assert.Empty(t, projects)

	issueID := int64(6)
	err = store.ImportReplay(ctx, []db.ImportRecord{
		{Kind: db.ImportKindProject, Project: &db.ProjectExport{
			ID: 5, UID: replayProjectUID, Name: "hash-rejection", CreatedAt: created,
			Metadata: json.RawMessage(`{}`), Revision: 1,
		}},
		{Kind: db.ImportKindIssue, Issue: &db.IssueExport{
			ID: issueID, UID: replayIssueUID, ProjectID: 5, ShortID: "zz12",
			Title: "Hash rejection", Status: "open", Author: "fixture-author",
			CreatedAt: created, UpdatedAt: created, Metadata: json.RawMessage(`{}`), Revision: 1,
		}},
		{Kind: db.ImportKindEvent, Event: &db.EventExport{
			ID: 10, UID: replayEventUID, OriginInstanceUID: replayInstanceUID,
			ProjectID: 5, ProjectUID: replayProjectUID, ProjectName: "hash-rejection",
			IssueID: &issueID, IssueUID: new(replayIssueUID), Type: "issue.created",
			Actor: "fixture-author", Payload: json.RawMessage(`{"title":"Hash rejection"}`),
			HLCPhysicalMS: 1784102400000, ContentHash: strings.Repeat("0", 64), CreatedAt: created,
		}},
	}, db.ImportOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "content_hash mismatch")
	projects, listErr = store.ListProjects(ctx)
	if listErr != nil {
		return listErr
	}
	assert.Empty(t, projects, "hash rejection must roll back projected rows")

	existing, createErr := store.CreateProject(ctx, "appeared-before-replay")
	if createErr != nil {
		return createErr
	}
	err = store.ImportReplay(ctx, []db.ImportRecord{{
		Kind: db.ImportKindProject,
		Project: &db.ProjectExport{
			ID: 8, UID: replayProjectUID, Name: "fresh-only", CreatedAt: created,
			Metadata: json.RawMessage(`{}`), Revision: 1,
		},
	}}, db.ImportOptions{RequireFreshTarget: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fresh")
	preserved, preserveErr := store.ProjectByUID(ctx, existing.UID)
	if preserveErr != nil {
		return fmt.Errorf("fresh-target rejection lost existing project: %w", preserveErr)
	}
	assert.Equal(t, existing.Name, preserved.Name)
	return nil
}

func checkSnapshotReplayProjectEnvelopes(
	t *testing.T,
	target db.Storage,
	backend Backend,
) error {
	t.Helper()
	ctx := context.Background()
	source := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	peerTarget := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, peerTarget.Close()) })

	firstProject, err := source.CreateProject(ctx, "first-envelope")
	if err != nil {
		return err
	}
	secondProject, err := source.CreateProject(ctx, "second-envelope")
	if err != nil {
		return err
	}
	firstIssue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: firstProject.ID, Title: "first side", Author: "fixture-author",
	})
	if err != nil {
		return err
	}
	secondIssue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: secondProject.ID, Title: "second side", Author: "fixture-author",
	})
	if err != nil {
		return err
	}
	link, err := source.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: firstIssue.ID, ToIssueID: secondIssue.ID, Type: "blocks", Author: "fixture-author",
	})
	if err != nil {
		return err
	}
	if _, err := source.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source: "example-tracker", ExternalID: "cross-link", ObjectType: "link",
		ProjectID: firstProject.ID, IssueID: &firstIssue.ID, LinkID: &link.ID,
	}); err != nil {
		return err
	}

	filtered, err := CollectImportRecords(ctx, source, db.ExportFilter{
		ProjectID: &firstProject.ID, IncludeDeleted: true,
	})
	if err != nil {
		return err
	}
	if err := target.ImportReplay(ctx, filtered, db.ImportOptions{}); err != nil {
		return fmt.Errorf("import project-scoped replay envelope: %w", err)
	}
	_, err = target.LinkByID(ctx, link.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
	_, err = target.ImportMappingBySource(
		ctx, firstProject.ID, "example-tracker", "link", "cross-link",
	)
	assert.ErrorIs(t, err, db.ErrNotFound)

	full, err := CollectImportRecords(ctx, source, db.ExportFilter{IncludeDeleted: true})
	if err != nil {
		return err
	}
	for _, record := range full {
		if record.Link != nil && record.Link.ID == link.ID {
			duplicate := *record.Link
			full = append(full, db.ImportRecord{Kind: db.ImportKindLink, Link: &duplicate})
			break
		}
	}
	if err := peerTarget.ImportReplay(ctx, full, db.ImportOptions{}); err != nil {
		return fmt.Errorf("import complete replay envelope with duplicate link: %w", err)
	}
	gotLink, err := peerTarget.LinkByEndpoints(ctx, firstIssue.ID, secondIssue.ID, "blocks")
	if err != nil {
		return err
	}
	assert.Equal(t, link.ID, gotLink.ID)
	mapping, err := peerTarget.ImportMappingBySource(
		ctx, firstProject.ID, "example-tracker", "link", "cross-link",
	)
	if err != nil {
		return err
	}
	require.NotNil(t, mapping.LinkID)
	assert.Equal(t, link.ID, *mapping.LinkID)
	return nil
}

func checkSnapshotMergeExternalRoots(
	t *testing.T,
	target db.Storage,
	backend Backend,
) error {
	t.Helper()
	ctx := context.Background()
	source := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, source.Close()) })

	if err := source.ImportReplay(ctx, extendedReplayRecords(), db.ImportOptions{
		PreserveExternalRootBindingsEnabled: true,
	}); err != nil {
		return fmt.Errorf("seed external-root merge source: %w", err)
	}
	sourceProject, err := source.ProjectByUID(ctx, replayProjectUID)
	if err != nil {
		return err
	}
	pausedIssue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: sourceProject.ID, Title: "Operator-paused root", Author: "fixture-author",
	})
	if err != nil {
		return err
	}
	pausedBinding, _, err := source.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
		ProjectID: sourceProject.ID, IssueID: pausedIssue.ID,
		ConnectorInstance: "connector-one", ExternalRootKey: "operator-paused-root",
		ExternalAccountKey: "opaque-account", Actor: "fixture-author",
		ReceiveCommentsAfter: time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC),
	})
	if err != nil {
		return err
	}
	if _, _, err := source.PauseExternalRootBinding(ctx, db.ExternalRootActionParams{
		BindingID: pausedBinding.ID, Actor: "operator", Reason: "operator_pause",
	}); err != nil {
		return err
	}
	records, err := CollectImportRecords(ctx, source, db.ExportFilter{
		ProjectID: &sourceProject.ID, IncludeDeleted: true,
	})
	if err != nil {
		return err
	}
	targetMapping, err := target.UpsertExternalFieldMapping(ctx, db.ExternalFieldMappingParams{
		ConnectorInstance: "connector-one", KataField: "scheduled_on",
		ExternalFieldID: "schedule-one", ExternalFieldName: "Schedule",
		AcceptedKinds: []string{"date"}, Nullable: true, Writable: true,
		SchemaRevision: "schema-one",
	})
	if err != nil {
		return err
	}
	for i := range records {
		if records[i].ExternalFieldMapping != nil {
			records[i].ExternalFieldMapping.CreatedAt = targetMapping.CreatedAt
			records[i].ExternalFieldMapping.UpdatedAt = targetMapping.UpdatedAt
		}
		if records[i].ExternalFieldState != nil {
			records[i].ExternalFieldState.MappingCreatedAt = targetMapping.CreatedAt
		}
	}
	existing, err := target.CreateProject(ctx, "existing-project")
	if err != nil {
		return err
	}
	if err := target.ImportReplay(ctx, records, db.ImportOptions{MergeProject: true}); err != nil {
		return fmt.Errorf("merge external-root project snapshot: %w", err)
	}
	if _, err := target.ProjectByUID(ctx, existing.UID); err != nil {
		return fmt.Errorf("read project retained across merge: %w", err)
	}
	mergedProject, err := target.ProjectByUID(ctx, replayProjectUID)
	if err != nil {
		return fmt.Errorf("read merged project: %w", err)
	}
	assert.NotEqual(t, sourceProject.ID, mergedProject.ID)
	mergedIssue, err := target.IssueByUID(ctx, replayIssueUID, db.IncludeDeletedNo)
	if err != nil {
		return fmt.Errorf("read merged external-root issue: %w", err)
	}
	binding, err := target.ExternalRootBindingByIssue(ctx, mergedIssue.ID)
	if err != nil {
		return fmt.Errorf("read merged external-root binding: %w", err)
	}
	assert.Equal(t, replayBindingUID, binding.UID)
	assert.False(t, binding.Enabled)
	assert.Equal(t, "restore_reconfirmation_required", binding.PausedReason)
	mergedPausedIssue, err := target.IssueByUID(ctx, pausedIssue.UID, db.IncludeDeletedNo)
	if err != nil {
		return fmt.Errorf("read merged operator-paused issue: %w", err)
	}
	mergedPausedBinding, err := target.ExternalRootBindingByIssue(ctx, mergedPausedIssue.ID)
	if err != nil {
		return fmt.Errorf("read merged operator-paused binding: %w", err)
	}
	assert.False(t, mergedPausedBinding.Enabled)
	assert.Equal(t, "operator_pause", mergedPausedBinding.PausedReason)
	mappings, err := target.ListExternalFieldMappings(ctx, "connector-one")
	if err != nil {
		return fmt.Errorf("read merged external field mappings: %w", err)
	}
	require.Len(t, mappings, 1)
	assert.Equal(t, targetMapping.ID, mappings[0].ID)
	assert.True(t, mappings[0].Active, "an exact local mapping keeps its locally approved activation")
	states, err := target.ExternalFieldStates(ctx, binding.ID)
	if err != nil {
		return fmt.Errorf("read merged external field states: %w", err)
	}
	require.Len(t, states, 1)
	assert.Equal(t, mappings[0].ID, states[0].MappingID)
	assert.JSONEq(t, `"2026-08-20"`, string(states[0].Baseline))

	precisionTarget := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, precisionTarget.Close()) })
	precisionMapping, err := precisionTarget.UpsertExternalFieldMapping(ctx, db.ExternalFieldMappingParams{
		ConnectorInstance: "connector-one", KataField: "scheduled_on",
		ExternalFieldID: "schedule-one", ExternalFieldName: "Schedule",
		AcceptedKinds: []string{"date"}, Nullable: true, Writable: true,
		SchemaRevision: "schema-one",
	})
	if err != nil {
		return err
	}
	precisionRecords, err := CollectImportRecords(ctx, source, db.ExportFilter{
		ProjectID: &sourceProject.ID, IncludeDeleted: true,
	})
	if err != nil {
		return err
	}
	incomingCreatedAt := precisionMapping.CreatedAt.Add(time.Nanosecond)
	for i := range precisionRecords {
		if precisionRecords[i].ExternalFieldMapping != nil {
			precisionRecords[i].ExternalFieldMapping.CreatedAt = incomingCreatedAt
			precisionRecords[i].ExternalFieldMapping.UpdatedAt = incomingCreatedAt
		}
		if precisionRecords[i].ExternalFieldState != nil {
			precisionRecords[i].ExternalFieldState.MappingCreatedAt = incomingCreatedAt
		}
	}
	if err := precisionTarget.ImportReplay(ctx, precisionRecords, db.ImportOptions{MergeProject: true}); err != nil {
		return fmt.Errorf("merge adjacent submicrosecond field mapping identity: %w", err)
	}
	precisionMappings, err := precisionTarget.ListExternalFieldMappings(ctx, "connector-one")
	if err != nil {
		return err
	}
	require.Len(t, precisionMappings, 2)
	var incomingMappingID int64
	for _, mapping := range precisionMappings {
		if mapping.CreatedAt.Equal(incomingCreatedAt) {
			incomingMappingID = mapping.ID
		}
	}
	require.NotZero(t, incomingMappingID)
	precisionIssue, err := precisionTarget.IssueByUID(ctx, replayIssueUID, db.IncludeDeletedNo)
	if err != nil {
		return err
	}
	precisionBinding, err := precisionTarget.ExternalRootBindingByIssue(ctx, precisionIssue.ID)
	if err != nil {
		return err
	}
	precisionStates, err := precisionTarget.ExternalFieldStates(ctx, precisionBinding.ID)
	if err != nil {
		return err
	}
	require.Len(t, precisionStates, 1)
	assert.Equal(t, incomingMappingID, precisionStates[0].MappingID)

	deactivatedTarget := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, deactivatedTarget.Close()) })
	deactivatedMapping, err := deactivatedTarget.UpsertExternalFieldMapping(ctx, db.ExternalFieldMappingParams{
		ConnectorInstance: "connector-one", KataField: "scheduled_on",
		ExternalFieldID: "schedule-one", ExternalFieldName: "Schedule",
		AcceptedKinds: []string{"date"}, Nullable: true, Writable: true,
		SchemaRevision: "schema-one",
	})
	if err != nil {
		return err
	}
	beforeDeactivation, err := CollectImportRecords(ctx, source, db.ExportFilter{
		ProjectID: &sourceProject.ID, IncludeDeleted: true,
	})
	if err != nil {
		return err
	}
	for i := range beforeDeactivation {
		if beforeDeactivation[i].ExternalFieldMapping != nil {
			beforeDeactivation[i].ExternalFieldMapping.CreatedAt = deactivatedMapping.CreatedAt
			beforeDeactivation[i].ExternalFieldMapping.UpdatedAt = deactivatedMapping.UpdatedAt
		}
		if beforeDeactivation[i].ExternalFieldState != nil {
			beforeDeactivation[i].ExternalFieldState.MappingCreatedAt = deactivatedMapping.CreatedAt
		}
	}
	time.Sleep(2 * time.Millisecond)
	deactivatedMapping, err = deactivatedTarget.UnmapExternalField(ctx, "connector-one", "scheduled_on")
	if err != nil {
		return err
	}
	require.True(t, deactivatedMapping.UpdatedAt.After(deactivatedMapping.CreatedAt))
	if err := deactivatedTarget.ImportReplay(ctx, beforeDeactivation, db.ImportOptions{MergeProject: true}); err != nil {
		return fmt.Errorf("merge snapshot captured before mapping deactivation: %w", err)
	}
	deactivatedMappings, err := deactivatedTarget.ListExternalFieldMappings(ctx, "connector-one")
	if err != nil {
		return err
	}
	require.Len(t, deactivatedMappings, 1)
	assert.Equal(t, deactivatedMapping.ID, deactivatedMappings[0].ID)
	assert.False(t, deactivatedMappings[0].Active)
	assert.Equal(t, deactivatedMapping.UpdatedAt, deactivatedMappings[0].UpdatedAt,
		"merging an older snapshot must preserve the deactivation timestamp")

	advancedTarget := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, advancedTarget.Close()) })
	advancedMapping, err := advancedTarget.UpsertExternalFieldMapping(ctx, db.ExternalFieldMappingParams{
		ConnectorInstance: "connector-one", KataField: "scheduled_on",
		ExternalFieldID: "schedule-one", ExternalFieldName: "Schedule",
		AcceptedKinds: []string{"date"}, Nullable: true, Writable: true,
		SchemaRevision: "schema-one",
	})
	if err != nil {
		return err
	}
	afterDeactivation, err := CollectImportRecords(ctx, source, db.ExportFilter{
		ProjectID: &sourceProject.ID, IncludeDeleted: true,
	})
	if err != nil {
		return err
	}
	incomingUpdatedAt := advancedMapping.UpdatedAt.Add(time.Minute)
	for i := range afterDeactivation {
		if afterDeactivation[i].ExternalFieldMapping != nil {
			afterDeactivation[i].ExternalFieldMapping.CreatedAt = advancedMapping.CreatedAt
			afterDeactivation[i].ExternalFieldMapping.UpdatedAt = incomingUpdatedAt
			afterDeactivation[i].ExternalFieldMapping.Active = false
		}
		if afterDeactivation[i].ExternalFieldState != nil {
			afterDeactivation[i].ExternalFieldState.MappingCreatedAt = advancedMapping.CreatedAt
		}
	}
	if err := advancedTarget.ImportReplay(ctx, afterDeactivation, db.ImportOptions{MergeProject: true}); err != nil {
		return fmt.Errorf("merge snapshot captured after mapping deactivation: %w", err)
	}
	advancedMappings, err := advancedTarget.ListExternalFieldMappings(ctx, "connector-one")
	if err != nil {
		return err
	}
	require.Len(t, advancedMappings, 1)
	assert.True(t, advancedMappings[0].Active, "project merge preserves local mapping activation")
	assert.Equal(t, incomingUpdatedAt, advancedMappings[0].UpdatedAt,
		"merging a newer snapshot must advance the descriptor timestamp")

	unmappedTarget := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, unmappedTarget.Close()) })
	if err := unmappedTarget.ImportReplay(ctx, records, db.ImportOptions{MergeProject: true}); err != nil {
		return fmt.Errorf("merge external-root project snapshot without local mapping: %w", err)
	}
	unmapped, err := unmappedTarget.ListExternalFieldMappings(ctx, "connector-one")
	if err != nil {
		return fmt.Errorf("read unapproved merged external field mapping: %w", err)
	}
	require.Len(t, unmapped, 1)
	assert.False(t, unmapped[0].Active, "project merge must not activate a connector mapping")

	conflictTarget := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, conflictTarget.Close()) })
	conflictingMapping, err := conflictTarget.UpsertExternalFieldMapping(ctx, db.ExternalFieldMappingParams{
		ConnectorInstance: "connector-one", KataField: "scheduled_on",
		ExternalFieldID: "schedule-one", ExternalFieldName: "Different schedule",
		AcceptedKinds: []string{"date"}, Nullable: true, Writable: true,
		SchemaRevision: "schema-one",
	})
	if err != nil {
		return err
	}
	conflictingRecords, err := CollectImportRecords(ctx, source, db.ExportFilter{
		ProjectID: &sourceProject.ID, IncludeDeleted: true,
	})
	if err != nil {
		return err
	}
	for i := range conflictingRecords {
		if conflictingRecords[i].ExternalFieldMapping != nil {
			conflictingRecords[i].ExternalFieldMapping.CreatedAt = conflictingMapping.CreatedAt
			conflictingRecords[i].ExternalFieldMapping.UpdatedAt = conflictingMapping.UpdatedAt
		}
		if conflictingRecords[i].ExternalFieldState != nil {
			conflictingRecords[i].ExternalFieldState.MappingCreatedAt = conflictingMapping.CreatedAt
		}
	}
	err = conflictTarget.ImportReplay(ctx, conflictingRecords, db.ImportOptions{MergeProject: true})
	require.ErrorContains(t, err, "mapping identity conflicts with target descriptor")
	_, err = conflictTarget.ProjectByUID(ctx, replayProjectUID)
	assert.ErrorIs(t, err, db.ErrNotFound, "mapping collision must roll back the project merge")

	uidTarget := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, uidTarget.Close()) })
	uidProject, err := uidTarget.CreateProject(ctx, "uid-collision-target")
	if err != nil {
		return err
	}
	uidIssue, _, err := uidTarget.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: uidProject.ID, Title: "Existing bound issue", Author: "fixture-author",
	})
	if err != nil {
		return err
	}
	uidIssueID := uidIssue.ID
	if _, err := uidTarget.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source: "connector:connector-one", ExternalID: "existing-root", ObjectType: "issue",
		ProjectID: uidProject.ID, IssueID: &uidIssueID,
	}); err != nil {
		return err
	}
	existingBinding, _, err := uidTarget.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
		ProjectID: uidProject.ID, IssueID: uidIssue.ID,
		ConnectorInstance: "connector-one", ExternalRootKey: "existing-root",
		ExternalAccountKey: "opaque-account", Actor: "fixture-author",
		ReceiveCommentsAfter: time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	uidCollisionRecords, err := CollectImportRecords(ctx, source, db.ExportFilter{
		ProjectID: &sourceProject.ID, IncludeDeleted: true,
	})
	if err != nil {
		return err
	}
	for i := range uidCollisionRecords {
		if uidCollisionRecords[i].ExternalRootBinding != nil {
			uidCollisionRecords[i].ExternalRootBinding.UID = existingBinding.UID
		}
		if uidCollisionRecords[i].ExternalFieldState != nil {
			uidCollisionRecords[i].ExternalFieldState.BindingUID = existingBinding.UID
		}
	}
	err = uidTarget.ImportReplay(ctx, uidCollisionRecords, db.ImportOptions{MergeProject: true})
	require.ErrorContains(t, err, "external root binding UID")
	_, err = uidTarget.ProjectByUID(ctx, replayProjectUID)
	assert.ErrorIs(t, err, db.ErrNotFound, "binding UID collision must roll back the project merge")

	historyTarget := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, historyTarget.Close()) })
	historyProject, err := historyTarget.CreateProject(ctx, "root-history-target")
	if err != nil {
		return err
	}
	historyIssue, _, err := historyTarget.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: historyProject.ID, Title: "Historical root owner", Author: "fixture-author",
	})
	if err != nil {
		return err
	}
	historyBinding, _, err := historyTarget.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
		ProjectID: historyProject.ID, IssueID: historyIssue.ID,
		ConnectorInstance: "connector-one", ExternalRootKey: "external-42",
		ExternalAccountKey: "opaque-account", Actor: "fixture-author",
		ReceiveCommentsAfter: time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	if _, _, err := historyTarget.UnbindExternalRootBinding(ctx, db.ExternalRootActionParams{
		BindingID: historyBinding.ID, Actor: "fixture-author",
	}); err != nil {
		return err
	}
	err = historyTarget.ImportReplay(ctx, records, db.ImportOptions{MergeProject: true})
	require.ErrorIs(t, err, db.ErrExternalRootAlreadyBound)
	_, err = historyTarget.ProjectByUID(ctx, replayProjectUID)
	assert.ErrorIs(t, err, db.ErrNotFound, "root-history collision must roll back the project merge")

	accountTarget := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, accountTarget.Close()) })
	accountProject, err := accountTarget.CreateProject(ctx, "account-history-target")
	if err != nil {
		return err
	}
	accountIssue, _, err := accountTarget.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: accountProject.ID, Title: "Historical account owner", Author: "fixture-author",
	})
	if err != nil {
		return err
	}
	_, _, err = accountTarget.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
		ProjectID: accountProject.ID, IssueID: accountIssue.ID,
		ConnectorInstance: "connector-one", ExternalRootKey: "different-root",
		ExternalAccountKey: "different-account", Actor: "fixture-author",
		ReceiveCommentsAfter: time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	err = accountTarget.ImportReplay(ctx, records, db.ImportOptions{MergeProject: true})
	require.ErrorIs(t, err, db.ErrExternalRootValidation)
	_, err = accountTarget.ProjectByUID(ctx, replayProjectUID)
	assert.ErrorIs(t, err, db.ErrNotFound, "account-history collision must roll back the project merge")
	return nil
}
