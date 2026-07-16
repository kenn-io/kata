package dbtest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

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

func extendedReplayRecords() []db.ImportRecord {
	projectID := int64(41)
	issueID := int64(61)
	commentID := int64(121)
	recurrenceID := int64(51)
	created := "2026-07-15T12:00:00.000Z"
	lastError := "temporary failure"
	lastErrorAt := "2026-07-15T12:05:00.000Z"
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
			BindingID: 71, ProjectID: projectID, LastErrorAt: &lastErrorAt, LastError: &lastError,
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
			RecurrenceID: &recurrenceID, RecurrenceUID: stringPointer(replayRecurrenceUID),
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
			ID: 131, Source: "example-tracker", ExternalID: "external-42", ObjectType: "issue",
			ProjectID: projectID, IssueID: &issueID, ImportedAt: created,
		}},
		{Kind: db.ImportKindFederationBinding, FederationBinding: &db.FederationBindingExport{
			ProjectID: projectID, Role: "spoke", HubURL: "https://hub.example",
			HubProjectID: 7, HubProjectUID: replayHubProjectUID, ReplayHorizonEventID: 10,
			PullCursorEventID: 11, PushEnabled: true, PushCursorEventID: 12,
			Actor: "sync-agent", Enabled: true, CreatedAt: created, UpdatedAt: created,
		}},
		{Kind: db.ImportKindFederationSyncStatus, FederationSyncStatus: &db.FederationSyncStatusExport{
			ProjectID: projectID, LastErrorAt: &lastErrorAt, LastError: stringPointer("connection reset"),
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
			ProjectID: projectID, PurgedIssueID: 160, IssueUID: stringPointer("01HZZZZZZZZZZZZZZZZZZZZZ1E"),
			ProjectUID: &projectUID, ProjectName: "extended-replay", ShortID: &purgeShortID,
			IssueTitle: "Purged issue", IssueAuthor: "fixture-author", EventCount: 2,
			PurgeResetAfterEventID: int64Pointer(100), Actor: "operator", Reason: &reason, PurgedAt: created,
		}},
		{Kind: db.ImportKindProjectPurgeLog, ProjectPurgeLog: &db.ProjectPurgeLogExport{
			ID: 151, UID: replayProjectPurge, OriginInstanceUID: replayInstanceUID,
			ProjectID: projectPurgeID, ProjectUID: &projectPurgeUID, ProjectName: "retired-project",
			IssueCount: 2, EventCount: 3, AliasCount: 1, CommentCount: 1, LinkCount: 1,
			LabelCount: 1, ClaimCount: 1, PendingClaimRequestCount: 1,
			PurgeResetAfterEventID: int64Pointer(101), Actor: "operator", Reason: &reason, PurgedAt: created,
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
		"projects":               500,
		"project_aliases":        600,
		"issue_sync_bindings":    700,
		"issues":                 800,
		"comments":               900,
		"links":                  1000,
		"import_mappings":        1100,
		"events":                 1200,
		"purge_log":              1300,
		"project_purge_log":      1400,
		"api_tokens":             1500,
		"federation_quarantine":  1600,
		"federation_enrollments": 1700,
		"issue_claims":           1800,
		"pending_claim_requests": 1900,
	}
}

func checkSnapshotReplayCompatibilityOptions(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	localInstanceUID := store.InstanceUID()
	created := "2026-07-15T12:00:00.000Z"
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
		NewInstance:                     true,
		DedupeLegacyActivePendingClaims: true,
		RecomputeEventContentHash:       true,
		PreserveIssueSyncBindingEnabled: true,
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
			IssueID: &issueID, IssueUID: stringPointer(replayIssueUID), Type: "issue.created",
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

func stringPointer(value string) *string { return &value }

func int64Pointer(value int64) *int64 { return &value }

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
