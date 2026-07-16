package dbtest

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkSnapshotReplayCore(t *testing.T, target db.Storage, backend Backend) error {
	t.Helper()
	ctx := context.Background()
	source := backend.Open(t)
	require.NotNil(t, source)
	t.Cleanup(func() {
		require.NoError(t, source.Close())
	})

	project, err := source.CreateProject(ctx, "replay-project")
	if err != nil {
		return err
	}
	alias, err := source.AttachAlias(ctx, project.ID, "example/replay", "git")
	if err != nil {
		return err
	}
	first, _, err := source.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "Replay the snapshot",
		Body:      "preserve every durable projection",
		Author:    "fixture-author",
	})
	if err != nil {
		return err
	}
	second, _, err := source.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "Verify the replay",
		Author:    "fixture-author",
	})
	if err != nil {
		return err
	}
	comment, _, err := source.CreateComment(ctx, db.CreateCommentParams{
		IssueID: first.ID,
		Author:  "reviewer",
		Body:    "observable replay state",
	})
	if err != nil {
		return err
	}
	if _, err := source.AddLabel(ctx, first.ID, "portable", "fixture-author"); err != nil {
		return err
	}
	link, err := source.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: first.ID,
		ToIssueID:   second.ID,
		Type:        "blocks",
		Author:      "fixture-author",
	})
	if err != nil {
		return err
	}
	if _, err := source.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source:     "example-tracker",
		ExternalID: "issue-42",
		ObjectType: "issue",
		ProjectID:  project.ID,
		IssueID:    &first.ID,
	}); err != nil {
		return err
	}
	tokenName := "replay client"
	token, _, err := source.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "replay-token-secret",
		Actor:          "automation",
		Name:           &tokenName,
		AdminActor:     db.BootstrapActor,
	})
	if err != nil {
		return err
	}
	revokedToken, _, err := source.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "revoked-replay-token",
		Actor:          "retired-automation",
		AdminActor:     db.BootstrapActor,
	})
	if err != nil {
		return err
	}
	if _, _, err := source.RevokeAPIToken(ctx, revokedToken.ID, db.BootstrapActor); err != nil {
		return err
	}

	records, err := CollectImportRecords(ctx, source, db.ExportFilter{IncludeDeleted: true})
	if err != nil {
		return err
	}
	replacedProject, err := target.CreateProject(ctx, "replaced-by-replay")
	if err != nil {
		return err
	}
	if _, _, err := target.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: replacedProject.ID,
		Title:     "This state must not survive",
		Author:    "fixture-author",
	}); err != nil {
		return err
	}
	sourceInstanceUID := source.InstanceUID()
	targetInstanceUID := target.InstanceUID()
	require.NotEqual(t, sourceInstanceUID, targetInstanceUID)

	if err := target.ImportReplay(ctx, records, db.ImportOptions{}); err != nil {
		return fmt.Errorf("import replay fixture: %w", err)
	}
	assert.Equal(t, sourceInstanceUID, target.InstanceUID())
	_, err = target.ProjectByUID(ctx, replacedProject.UID)
	assert.ErrorIs(t, err, db.ErrNotFound,
		"snapshot replay must replace target state rather than merge into it")
	version, err := target.SchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("read replay schema version: %w", err)
	}
	assert.Equal(t, db.CurrentSchemaVersion(), version)

	gotProject, err := target.ProjectByUID(ctx, project.UID)
	if err != nil {
		return fmt.Errorf("read replay project: %w", err)
	}
	assert.Equal(t, project.ID, gotProject.ID)
	assert.Equal(t, "replay-project", gotProject.Name)
	aliases, err := target.ProjectAliases(ctx, project.ID)
	if err != nil {
		return fmt.Errorf("read replay aliases: %w", err)
	}
	require.Len(t, aliases, 1)
	assert.Equal(t, alias.ID, aliases[0].ID)
	assert.Equal(t, "example/replay", aliases[0].AliasIdentity)

	gotFirst, err := target.IssueByUID(ctx, first.UID, db.IncludeDeletedNo)
	if err != nil {
		return fmt.Errorf("read first replay issue: %w", err)
	}
	assert.Equal(t, first.ID, gotFirst.ID)
	assert.Equal(t, "Replay the snapshot", gotFirst.Title)
	comments, err := target.CommentsByIssue(ctx, gotFirst.ID)
	if err != nil {
		return fmt.Errorf("read replay comments: %w", err)
	}
	require.Len(t, comments, 1)
	assert.Equal(t, comment.UID, comments[0].UID)
	assert.Equal(t, "observable replay state", comments[0].Body)
	labels, err := target.LabelsByIssue(ctx, gotFirst.ID)
	if err != nil {
		return fmt.Errorf("read replay labels: %w", err)
	}
	require.Len(t, labels, 1)
	assert.Equal(t, "portable", labels[0].Label)
	gotSecond, err := target.IssueByUID(ctx, second.UID, db.IncludeDeletedNo)
	if err != nil {
		return fmt.Errorf("read second replay issue: %w", err)
	}
	gotLink, err := target.LinkByEndpoints(ctx, gotFirst.ID, gotSecond.ID, "blocks")
	if err != nil {
		return fmt.Errorf("read replay link: %w", err)
	}
	assert.Equal(t, link.ID, gotLink.ID)

	mapping, err := target.ImportMappingBySource(ctx, project.ID, "example-tracker", "issue", "issue-42")
	if err != nil {
		return fmt.Errorf("read replay import mapping: %w", err)
	}
	require.NotNil(t, mapping.IssueID)
	assert.Equal(t, first.ID, *mapping.IssueID)

	projectEventUIDs := replayEventUIDs(records, project.ID)
	events, err := target.EventsByUIDs(ctx, project.ID, projectEventUIDs)
	if err != nil {
		return fmt.Errorf("read replay events: %w", err)
	}
	assert.Len(t, events, len(projectEventUIDs))
	resolved, err := target.ResolveAPIToken(ctx, "replay-token-secret")
	if err != nil {
		return fmt.Errorf("resolve replay token: %w", err)
	}
	assert.Equal(t, token.ID, resolved.ID)
	assert.Equal(t, "automation", resolved.Actor)
	_, err = target.ResolveAPIToken(ctx, "revoked-replay-token")
	assert.ErrorIs(t, err, db.ErrNotFound)
	tokens, err := target.ListAPITokens(ctx)
	if err != nil {
		return fmt.Errorf("list replay tokens: %w", err)
	}
	require.Len(t, tokens, 2)
	assert.Equal(t, token.ID, tokens[0].ID)
	assert.Empty(t, tokens[0].TokenHash)
	assert.Equal(t, revokedToken.ID, tokens[1].ID)
	require.NotNil(t, tokens[1].RevokedAt)
	if _, err := target.SystemProject(ctx); err != nil {
		return fmt.Errorf("read replay system project: %w", err)
	}

	createdAfterReplay, err := target.CreateProject(ctx, "after-replay")
	if err != nil {
		return fmt.Errorf("create project after replay: %w", err)
	}
	assert.Greater(t, createdAfterReplay.ID, project.ID,
		"identity sequence must advance past imported project IDs")
	return nil
}

func replayEventUIDs(records []db.ImportRecord, projectID int64) []string {
	uids := make([]string, 0)
	for _, record := range records {
		if record.Event != nil && record.Event.ProjectID == projectID {
			uids = append(uids, record.Event.UID)
		}
	}
	return uids
}

// CollectImportRecords drains the storage export contract into the normalized
// current-schema records consumed by ImportReplay. Keeping this in dbtest lets
// every backend exercise the same snapshot fixture and ordering.
func CollectImportRecords(
	ctx context.Context,
	store db.Storage,
	filter db.ExportFilter,
) ([]db.ImportRecord, error) {
	var records []db.ImportRecord
	appendRecord := func(record db.ImportRecord, err error) error {
		if err != nil {
			return err
		}
		records = append(records, record)
		return nil
	}
	for value, err := range store.ExportMeta(ctx) {
		v := value
		if err := appendRecord(db.ImportRecord{Kind: db.ImportKindMeta, Meta: &v}, err); err != nil {
			return nil, fmt.Errorf("export meta for replay: %w", err)
		}
	}
	for value, err := range store.ExportProjects(ctx, filter) {
		v := value
		if err := appendRecord(db.ImportRecord{Kind: db.ImportKindProject, Project: &v}, err); err != nil {
			return nil, fmt.Errorf("export projects for replay: %w", err)
		}
	}
	for value, err := range store.ExportProjectAliases(ctx, filter) {
		v := value
		if err := appendRecord(db.ImportRecord{Kind: db.ImportKindProjectAlias, Alias: &v}, err); err != nil {
			return nil, fmt.Errorf("export project aliases for replay: %w", err)
		}
	}
	for value, err := range store.ExportIssueSyncBindings(ctx, filter) {
		v := value
		if err := appendRecord(db.ImportRecord{Kind: db.ImportKindIssueSyncBinding, IssueSyncBinding: &v}, err); err != nil {
			return nil, fmt.Errorf("export issue sync bindings for replay: %w", err)
		}
	}
	for value, err := range store.ExportIssueSyncStatus(ctx, filter) {
		v := value
		if err := appendRecord(db.ImportRecord{Kind: db.ImportKindIssueSyncStatus, IssueSyncStatus: &v}, err); err != nil {
			return nil, fmt.Errorf("export issue sync status for replay: %w", err)
		}
	}
	for value, err := range store.ExportRecurrences(ctx, filter) {
		v := value
		if err := appendRecord(db.ImportRecord{Kind: db.ImportKindRecurrence, Recurrence: &v}, err); err != nil {
			return nil, fmt.Errorf("export recurrences for replay: %w", err)
		}
	}
	for value, err := range store.ExportIssues(ctx, filter) {
		v := value
		if err := appendRecord(db.ImportRecord{Kind: db.ImportKindIssue, Issue: &v}, err); err != nil {
			return nil, fmt.Errorf("export issues for replay: %w", err)
		}
	}
	for value, err := range store.ExportComments(ctx, filter) {
		v := value
		if err := appendRecord(db.ImportRecord{Kind: db.ImportKindComment, Comment: &v}, err); err != nil {
			return nil, fmt.Errorf("export comments for replay: %w", err)
		}
	}
	for value, err := range store.ExportIssueLabels(ctx, filter) {
		v := value
		if err := appendRecord(db.ImportRecord{Kind: db.ImportKindIssueLabel, Label: &v}, err); err != nil {
			return nil, fmt.Errorf("export labels for replay: %w", err)
		}
	}
	for value, err := range store.ExportLinks(ctx, filter) {
		v := value
		if err := appendRecord(db.ImportRecord{Kind: db.ImportKindLink, Link: &v}, err); err != nil {
			return nil, fmt.Errorf("export links for replay: %w", err)
		}
	}
	for value, err := range store.ExportImportMappings(ctx, filter) {
		v := value
		if err := appendRecord(db.ImportRecord{Kind: db.ImportKindImportMapping, ImportMapping: &v}, err); err != nil {
			return nil, fmt.Errorf("export mappings for replay: %w", err)
		}
	}
	for value, err := range store.ExportFederationBindings(ctx, filter) {
		v := value
		if err := appendRecord(db.ImportRecord{Kind: db.ImportKindFederationBinding, FederationBinding: &v}, err); err != nil {
			return nil, fmt.Errorf("export federation bindings for replay: %w", err)
		}
	}
	for value, err := range store.ExportFederationSyncStatus(ctx, filter) {
		v := value
		if err := appendRecord(db.ImportRecord{Kind: db.ImportKindFederationSyncStatus, FederationSyncStatus: &v}, err); err != nil {
			return nil, fmt.Errorf("export federation sync status for replay: %w", err)
		}
	}
	for value, err := range store.ExportFederationQuarantine(ctx, filter) {
		v := value
		if err := appendRecord(db.ImportRecord{Kind: db.ImportKindFederationQuarantine, FederationQuarantine: &v}, err); err != nil {
			return nil, fmt.Errorf("export federation quarantine for replay: %w", err)
		}
	}
	for value, err := range store.ExportFederationEnrollments(ctx, filter) {
		v := value
		if err := appendRecord(db.ImportRecord{Kind: db.ImportKindFederationEnrollment, FederationEnrollment: &v}, err); err != nil {
			return nil, fmt.Errorf("export federation enrollments for replay: %w", err)
		}
	}
	for value, err := range store.ExportIssueClaims(ctx, filter) {
		v := value
		if err := appendRecord(db.ImportRecord{Kind: db.ImportKindIssueClaim, IssueClaim: &v}, err); err != nil {
			return nil, fmt.Errorf("export issue claims for replay: %w", err)
		}
	}
	for value, err := range store.ExportPendingClaimRequests(ctx, filter) {
		v := value
		if err := appendRecord(db.ImportRecord{Kind: db.ImportKindPendingClaimRequest, PendingClaimRequest: &v}, err); err != nil {
			return nil, fmt.Errorf("export pending claims for replay: %w", err)
		}
	}
	for value, err := range store.ExportEvents(ctx, filter) {
		v := value
		if err := appendRecord(db.ImportRecord{Kind: db.ImportKindEvent, Event: &v}, err); err != nil {
			return nil, fmt.Errorf("export events for replay: %w", err)
		}
	}
	for value, err := range store.ExportPurgeLog(ctx, filter) {
		v := value
		if err := appendRecord(db.ImportRecord{Kind: db.ImportKindPurgeLog, PurgeLog: &v}, err); err != nil {
			return nil, fmt.Errorf("export purge log for replay: %w", err)
		}
	}
	for value, err := range store.ExportProjectPurgeLog(ctx, filter) {
		v := value
		if err := appendRecord(db.ImportRecord{Kind: db.ImportKindProjectPurgeLog, ProjectPurgeLog: &v}, err); err != nil {
			return nil, fmt.Errorf("export project purge log for replay: %w", err)
		}
	}
	for value, err := range store.ExportSequences(ctx) {
		v := value
		if err := appendRecord(db.ImportRecord{Kind: db.ImportKindSQLiteSequence, Sequence: &v}, err); err != nil {
			return nil, fmt.Errorf("export sequences for replay: %w", err)
		}
	}
	return records, nil
}
