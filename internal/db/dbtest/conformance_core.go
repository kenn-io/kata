package dbtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkProjectAliases(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	first, err := store.CreateProject(ctx, "first-alias-project")
	if err != nil {
		return fmt.Errorf("create first alias project: %w", err)
	}
	second, err := store.CreateProject(ctx, "second-alias-project")
	if err != nil {
		return fmt.Errorf("create second alias project: %w", err)
	}
	alias, err := store.AttachAlias(ctx, first.ID, "example-workspace", "local")
	if err != nil {
		return fmt.Errorf("attach alias: %w", err)
	}
	assert.Equal(t, first.ID, alias.ProjectID)
	assert.Equal(t, "example-workspace", alias.AliasIdentity)
	assert.Equal(t, "local", alias.AliasKind)
	_, err = store.AttachAlias(ctx, second.ID, alias.AliasIdentity, alias.AliasKind)
	assert.ErrorIs(t, err, db.ErrAliasExists)

	byID, err := store.AliasByID(ctx, alias.ID)
	if err != nil {
		return fmt.Errorf("alias by ID: %w", err)
	}
	byIdentity, err := store.AliasByIdentity(ctx, alias.AliasIdentity)
	if err != nil {
		return fmt.Errorf("alias by identity: %w", err)
	}
	assert.Equal(t, alias, byID)
	assert.Equal(t, alias, byIdentity)

	latest, ok, err := store.LatestAliasForProject(ctx, first.ID)
	if err != nil {
		return fmt.Errorf("latest alias: %w", err)
	}
	assert.True(t, ok)
	assert.Equal(t, db.AliasRow{Identity: alias.AliasIdentity, Kind: alias.AliasKind}, latest)

	if err := store.ReassignAlias(ctx, alias.ID, second.ID); err != nil {
		return fmt.Errorf("reassign alias: %w", err)
	}
	firstAliases, err := store.ProjectAliases(ctx, first.ID)
	if err != nil {
		return fmt.Errorf("list first aliases: %w", err)
	}
	assert.Empty(t, firstAliases)
	secondAliases, err := store.ProjectAliases(ctx, second.ID)
	if err != nil {
		return fmt.Errorf("list second aliases: %w", err)
	}
	require.Len(t, secondAliases, 1)
	assert.Equal(t, second.ID, secondAliases[0].ProjectID)

	_, err = store.AliasByID(ctx, alias.ID+100000)
	assert.ErrorIs(t, err, db.ErrNotFound)
	if err := store.HardDeleteProject(ctx, first.ID); err != nil {
		return fmt.Errorf("hard delete empty project: %w", err)
	}
	_, err = store.ProjectByID(ctx, first.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
	return nil
}

func checkLifecycle(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()

	instanceUID := store.InstanceUID()
	if !uid.Valid(instanceUID) {
		return fmt.Errorf("invalid instance UID %q", instanceUID)
	}
	if store.Path() == "" {
		return errors.New("empty storage path")
	}
	version, err := store.SchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("schema version: %w", err)
	}
	assert.Equal(t, db.CurrentSchemaVersion(), version)

	if err := store.RefreshInstanceUID(ctx); err != nil {
		return fmt.Errorf("refresh instance UID: %w", err)
	}
	assert.Equal(t, instanceUID, store.InstanceUID())

	var calls int
	marker := errors.New("operation stopped")
	err = store.RetryTransient(ctx, func() error {
		calls++
		return marker
	})
	assert.ErrorIs(t, err, marker)
	assert.Equal(t, 1, calls)
	return nil
}

func checkProjects(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()

	_, err := store.CreateProject(ctx, db.SystemProjectName)
	assert.Error(t, err)
	_, err = store.CreateProjectWithUID(ctx, "invalid-uid-project", "not-a-ulid")
	assert.Error(t, err)
	_, err = store.CreateProjectWithUID(ctx, "reserved-uid-project", db.SystemProjectUID)
	assert.Error(t, err)

	projects, err := store.ListProjects(ctx)
	if err != nil {
		return fmt.Errorf("list fresh projects: %w", err)
	}
	assert.Empty(t, projects)

	alpha, err := store.CreateProjectWithUID(ctx, "alpha-project", "01HZNQ7VFPK1XGD8R5MABCD4EX")
	if err != nil {
		return fmt.Errorf("create project with UID: %w", err)
	}
	assert.Equal(t, "alpha-project", alpha.Name)
	assert.Equal(t, "01HZNQ7VFPK1XGD8R5MABCD4EX", alpha.UID)

	beta, err := store.CreateProject(ctx, "beta-project")
	if err != nil {
		return fmt.Errorf("create project: %w", err)
	}
	if !uid.Valid(beta.UID) {
		return fmt.Errorf("invalid generated project UID %q", beta.UID)
	}

	byID, err := store.ProjectByID(ctx, alpha.ID)
	if err != nil {
		return fmt.Errorf("project by ID: %w", err)
	}
	byName, err := store.ProjectByName(ctx, alpha.Name)
	if err != nil {
		return fmt.Errorf("project by name: %w", err)
	}
	byUID, err := store.ProjectByUID(ctx, alpha.UID)
	if err != nil {
		return fmt.Errorf("project by UID: %w", err)
	}
	for _, got := range []db.Project{byID, byName, byUID} {
		assert.Equal(t, alpha.ID, got.ID)
		assert.Equal(t, alpha.Name, got.Name)
		assert.Equal(t, alpha.UID, got.UID)
	}

	renamed, err := store.RenameProject(ctx, beta.ID, "renamed-project")
	if err != nil {
		return fmt.Errorf("rename project: %w", err)
	}
	assert.Equal(t, beta.ID, renamed.ID)
	assert.Equal(t, "renamed-project", renamed.Name)

	includingArchived, err := store.ProjectByNameIncludingArchived(ctx, alpha.Name)
	if err != nil {
		return fmt.Errorf("project by name including archived: %w", err)
	}
	assert.Equal(t, alpha.ID, includingArchived.ID)

	projects, err = store.ListProjectsIncludingArchived(ctx)
	if err != nil {
		return fmt.Errorf("list projects including archived: %w", err)
	}
	require.Len(t, projects, 2)
	assert.Equal(t, []string{"alpha-project", "renamed-project"}, []string{projects[0].Name, projects[1].Name})

	_, err = store.ProjectByUID(ctx, "01ARZ3NDEKTSV4RRFFQ69G5FAV")
	assert.ErrorIs(t, err, db.ErrNotFound)
	_, err = store.ProjectByID(ctx, alpha.ID+100000)
	assert.ErrorIs(t, err, db.ErrNotFound)
	_, err = store.ProjectByName(ctx, "missing-project")
	assert.ErrorIs(t, err, db.ErrNotFound)
	_, err = store.RenameProject(ctx, alpha.ID+100000, "missing-project")
	assert.ErrorIs(t, err, db.ErrNotFound)
	return nil
}

func checkIssueEventAtomicity(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "event-project")
	if err != nil {
		return fmt.Errorf("create project: %w", err)
	}
	before, err := store.MaxEventID(ctx)
	if err != nil {
		return fmt.Errorf("max event before create: %w", err)
	}

	issue, event, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "persist issue and event",
		Body:      "both rows commit together",
		Author:    "conformance-agent",
	})
	if err != nil {
		return fmt.Errorf("create issue: %w", err)
	}
	assert.Equal(t, "issue.created", event.Type)
	require.NotNil(t, event.IssueID)
	assert.Equal(t, issue.ID, *event.IssueID)
	require.NotNil(t, event.IssueUID)
	assert.Equal(t, issue.UID, *event.IssueUID)

	byID, err := store.IssueByID(ctx, issue.ID)
	if err != nil {
		return fmt.Errorf("issue by ID: %w", err)
	}
	byUID, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	if err != nil {
		return fmt.Errorf("issue by UID: %w", err)
	}
	byShortID, err := store.IssueByShortID(ctx, project.ID, issue.ShortID, db.IncludeDeletedNo)
	if err != nil {
		return fmt.Errorf("issue by short ID: %w", err)
	}
	for _, got := range []db.Issue{byID, byUID, byShortID} {
		assert.Equal(t, issue.ID, got.ID)
		assert.Equal(t, issue.UID, got.UID)
		assert.Equal(t, issue.ShortID, got.ShortID)
		assert.Equal(t, issue.Title, got.Title)
	}

	events, err := store.EventsAfter(ctx, db.EventsAfterParams{AfterID: before, ProjectID: project.ID, Limit: 10})
	if err != nil {
		return fmt.Errorf("events after create: %w", err)
	}
	require.Len(t, events, 1)
	assert.Equal(t, event.UID, events[0].UID)

	afterCreate, err := store.MaxEventID(ctx)
	if err != nil {
		return fmt.Errorf("max event after create: %w", err)
	}
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID + 100000,
		Title:     "must roll back",
		Author:    "conformance-agent",
	})
	assert.Error(t, err)
	afterFailure, maxErr := store.MaxEventID(ctx)
	if maxErr != nil {
		return fmt.Errorf("max event after failed create: %w", maxErr)
	}
	assert.Equal(t, afterCreate, afterFailure)

	issues, err := store.ListIssues(ctx, db.ListIssuesParams{ProjectID: project.ID})
	if err != nil {
		return fmt.Errorf("list issues: %w", err)
	}
	require.Len(t, issues, 1)
	assert.Equal(t, issue.UID, issues[0].UID)
	return nil
}

func checkIdempotency(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "idempotency-project")
	if err != nil {
		return fmt.Errorf("create project: %w", err)
	}
	since := time.Now().Add(-time.Minute)
	issue, event, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID:              project.ID,
		Title:                  "idempotent issue",
		Author:                 "conformance-agent",
		IdempotencyKey:         "request-1",
		IdempotencyFingerprint: "fingerprint-1",
	})
	if err != nil {
		return fmt.Errorf("create issue: %w", err)
	}

	match, err := store.LookupIdempotency(ctx, project.ID, "request-1", since)
	if err != nil {
		return fmt.Errorf("lookup idempotency: %w", err)
	}
	require.NotNil(t, match)
	assert.Equal(t, issue.ID, match.IssueID)
	assert.Equal(t, issue.ShortID, match.IssueShortID)
	assert.Equal(t, event.UID, match.Event.UID)
	assert.Equal(t, "fingerprint-1", match.Fingerprint)

	missing, err := store.LookupIdempotency(ctx, project.ID, "request-2", since)
	if err != nil {
		return fmt.Errorf("lookup missing idempotency key: %w", err)
	}
	assert.Nil(t, missing)

	releaseFirst, err := store.AcquireIdempotencyLock(ctx, project.ID, "serialized-request")
	if err != nil {
		return fmt.Errorf("acquire first idempotency lock: %w", err)
	}
	firstReleased := false
	defer func() {
		if !firstReleased {
			_ = releaseFirst()
		}
	}()
	type lockResult struct {
		release func() error
		err     error
	}
	second := make(chan lockResult, 1)
	lockCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	go func() {
		release, err := store.AcquireIdempotencyLock(lockCtx, project.ID, "serialized-request")
		second <- lockResult{release: release, err: err}
	}()
	select {
	case result := <-second:
		if result.release != nil {
			_ = result.release()
		}
		return errors.New("second idempotency lock acquired before first release")
	case <-time.After(100 * time.Millisecond):
	}
	if err := releaseFirst(); err != nil {
		return fmt.Errorf("release first idempotency lock: %w", err)
	}
	firstReleased = true
	select {
	case result := <-second:
		if result.err != nil {
			return fmt.Errorf("acquire second idempotency lock: %w", result.err)
		}
		if err := result.release(); err != nil {
			return fmt.Errorf("release second idempotency lock: %w", err)
		}
	case <-lockCtx.Done():
		return errors.New("second idempotency lock did not acquire after first release")
	}
	return nil
}

func checkConcurrentUniqueness(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	start := make(chan struct{})
	results := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			_, err := store.CreateProject(ctx, "one-name")
			results <- err
		}()
	}
	ready.Wait()
	close(start)

	var successes int
	var firstErr error
	for range 2 {
		if err := <-results; err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		successes++
	}
	if successes == 0 {
		return fmt.Errorf("both concurrent creates failed: %w", firstErr)
	}
	assert.Equal(t, 1, successes)

	projects, err := store.ListProjects(ctx)
	if err != nil {
		return fmt.Errorf("list projects: %w", err)
	}
	require.Len(t, projects, 1)
	assert.Equal(t, "one-name", projects[0].Name)
	return nil
}

func checkPurgeReset(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "purge-project")
	if err != nil {
		return fmt.Errorf("create project: %w", err)
	}
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "purge me",
		Author:    "conformance-agent",
	})
	if err != nil {
		return fmt.Errorf("create issue: %w", err)
	}
	if _, err := store.PurgeIssue(ctx, issue.ID, "conformance-agent", nil); err != nil {
		return fmt.Errorf("purge issue: %w", err)
	}

	reset, err := store.PurgeResetCheck(ctx, 0, project.ID)
	if err != nil {
		return fmt.Errorf("purge reset check: %w", err)
	}
	assert.Positive(t, reset)
	afterReset, err := store.PurgeResetCheck(ctx, reset, project.ID)
	if err != nil {
		return fmt.Errorf("purge reset check at cursor: %w", err)
	}
	assert.Zero(t, afterReset)

	_, err = store.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	assert.ErrorIs(t, err, db.ErrNotFound)
	purgeRecords, err := collectExport(store.ExportPurgeLog(ctx, db.ExportFilter{ProjectID: &project.ID}))
	if err != nil {
		return err
	}
	require.Len(t, purgeRecords, 1)
	assert.Equal(t, issue.UID, *purgeRecords[0].IssueUID)
	assert.Equal(t, project.UID, *purgeRecords[0].ProjectUID)
	assert.Equal(t, "purge me", purgeRecords[0].IssueTitle)
	assert.Equal(t, "conformance-agent", purgeRecords[0].Actor)
	if _, err := collectExport(store.ExportSequences(ctx)); err != nil {
		return err
	}
	return nil
}

func checkProjectLifecycle(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "lifecycle-project")
	if err != nil {
		return fmt.Errorf("create lifecycle project: %w", err)
	}
	first, err := store.AttachAlias(ctx, project.ID, "first-workspace", "local")
	if err != nil {
		return fmt.Errorf("attach first alias: %w", err)
	}
	second, err := store.AttachAlias(ctx, project.ID, "second-workspace", "local")
	if err != nil {
		return fmt.Errorf("attach second alias: %w", err)
	}
	detached, event, err := store.DetachProjectAlias(ctx, db.DetachAliasParams{
		ProjectID: project.ID, AliasID: first.ID, Actor: "conformance-agent",
	})
	if err != nil {
		return fmt.Errorf("detach alias: %w", err)
	}
	assert.Equal(t, first.ID, detached.ID)
	require.NotNil(t, event)
	assert.Equal(t, "project.alias_removed", event.Type)
	_, _, err = store.DetachProjectAlias(ctx, db.DetachAliasParams{
		ProjectID: project.ID, AliasID: second.ID, Actor: "conformance-agent",
	})
	assert.ErrorIs(t, err, db.ErrAliasIsLast)

	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "open work", Author: "conformance-agent",
	})
	if err != nil {
		return fmt.Errorf("create open issue: %w", err)
	}
	open, err := store.CountOpenIssues(ctx, project.ID)
	if err != nil {
		return fmt.Errorf("count open issues: %w", err)
	}
	assert.Equal(t, int64(1), open)
	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: project.ID, Actor: "conformance-agent",
	})
	var openErr *db.ProjectHasOpenIssuesError
	assert.ErrorAs(t, err, &openErr)

	archived, removedEvent, err := store.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: project.ID, Actor: "conformance-agent", Force: true,
	})
	if err != nil {
		return fmt.Errorf("force archive project: %w", err)
	}
	require.NotNil(t, archived.DeletedAt)
	require.NotNil(t, removedEvent)
	assert.Equal(t, "project.removed", removedEvent.Type)
	aliases, err := store.ProjectAliases(ctx, project.ID)
	if err != nil {
		return fmt.Errorf("list aliases after archive: %w", err)
	}
	assert.Empty(t, aliases)

	restored, restoredEvent, changed, err := store.RestoreProject(ctx, project.ID, "conformance-agent")
	if err != nil {
		return fmt.Errorf("restore project: %w", err)
	}
	assert.True(t, changed)
	assert.Nil(t, restored.DeletedAt)
	require.NotNil(t, restoredEvent)
	assert.Equal(t, "project.restored", restoredEvent.Type)
	_, noEvent, changed, err := store.RestoreProject(ctx, project.ID, "conformance-agent")
	if err != nil {
		return fmt.Errorf("repeat project restore: %w", err)
	}
	assert.False(t, changed)
	assert.Nil(t, noEvent)

	archived, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: project.ID, Actor: "conformance-agent", Force: true,
	})
	if err != nil {
		return fmt.Errorf("archive before purge: %w", err)
	}
	log, err := store.PurgeProject(ctx, db.PurgeProjectParams{
		ProjectID: archived.ID, Actor: "conformance-agent",
	})
	if err != nil {
		return fmt.Errorf("purge project: %w", err)
	}
	assert.Equal(t, project.ID, log.ProjectID)
	require.NotNil(t, log.EventsDeletedMaxID)
	require.NotNil(t, log.PurgeResetAfterEventID)
	assert.Greater(t, *log.PurgeResetAfterEventID, *log.EventsDeletedMaxID)
	reset, err := store.PurgeResetCheck(ctx, 0, project.ID)
	if err != nil {
		return fmt.Errorf("project purge reset: %w", err)
	}
	assert.Positive(t, reset)
	_, err = store.ProjectByID(ctx, project.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
	projectPurgeRecords, err := collectExport(store.ExportProjectPurgeLog(ctx, db.ExportFilter{ProjectID: &project.ID}))
	if err != nil {
		return err
	}
	require.Len(t, projectPurgeRecords, 1)
	assert.Equal(t, project.UID, *projectPurgeRecords[0].ProjectUID)
	assert.Equal(t, project.Name, projectPurgeRecords[0].ProjectName)
	assert.Equal(t, "conformance-agent", projectPurgeRecords[0].Actor)
	return nil
}

func checkIssueLifecycle(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "issue-lifecycle-project")
	if err != nil {
		return fmt.Errorf("create project: %w", err)
	}
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "original title", Body: "original body", Author: "conformance-agent",
	})
	if err != nil {
		return fmt.Errorf("create issue: %w", err)
	}
	newTitle, newBody := "edited title", "edited body"
	edited, editEvent, changed, err := store.EditIssue(ctx, db.EditIssueParams{
		IssueID: issue.ID, Title: &newTitle, Body: &newBody, Actor: "conformance-agent",
	})
	if err != nil {
		return fmt.Errorf("edit issue: %w", err)
	}
	assert.True(t, changed)
	assert.Equal(t, newTitle, edited.Title)
	require.NotNil(t, editEvent)
	assert.Equal(t, "issue.updated", editEvent.Type)

	priority := int64(2)
	prioritized, priorityEvent, changed, err := store.UpdatePriority(ctx, issue.ID, &priority, "conformance-agent")
	if err != nil {
		return fmt.Errorf("set priority: %w", err)
	}
	assert.True(t, changed)
	require.NotNil(t, prioritized.Priority)
	assert.Equal(t, priority, *prioritized.Priority)
	require.NotNil(t, priorityEvent)
	_, noEvent, changed, err := store.UpdatePriority(ctx, issue.ID, &priority, "conformance-agent")
	if err != nil {
		return fmt.Errorf("repeat priority: %w", err)
	}
	assert.False(t, changed)
	assert.Nil(t, noEvent)

	owner := "owner-one"
	owned, ownerEvent, changed, err := store.UpdateOwner(ctx, issue.ID, &owner, "conformance-agent")
	if err != nil {
		return fmt.Errorf("set owner: %w", err)
	}
	assert.True(t, changed)
	require.NotNil(t, owned.Owner)
	assert.Equal(t, owner, *owned.Owner)
	require.NotNil(t, ownerEvent)
	claim, err := store.ClaimOwner(ctx, issue.ID, "owner-two", false)
	assert.ErrorIs(t, err, db.ErrAlreadyClaimed)
	require.NotNil(t, claim.CurrentOwner)
	claim, err = store.ClaimOwner(ctx, issue.ID, "owner-two", true)
	if err != nil {
		return fmt.Errorf("force claim owner: %w", err)
	}
	assert.True(t, claim.Changed)

	closed, closeEvents, changed, err := store.CloseIssueWithEvents(
		ctx, issue.ID, "done", "conformance-agent", "complete", nil,
	)
	if err != nil {
		return fmt.Errorf("close issue: %w", err)
	}
	assert.True(t, changed)
	assert.Equal(t, "closed", closed.Status)
	require.Len(t, closeEvents, 1)
	assert.Equal(t, "issue.closed", closeEvents[0].Type)
	var closePayload struct {
		ParentUID     *string `json:"parent_uid"`
		ParentShortID *string `json:"parent_short_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(closeEvents[0].Payload), &closePayload))
	require.NotNil(t, closePayload.ParentUID)
	require.NotNil(t, closePayload.ParentShortID)
	assert.Empty(t, *closePayload.ParentUID)
	assert.Empty(t, *closePayload.ParentShortID)
	_, noEvent, changed, err = store.CloseIssue(ctx, issue.ID, "done", "conformance-agent", "complete", nil)
	if err != nil {
		return fmt.Errorf("repeat close: %w", err)
	}
	assert.False(t, changed)
	assert.Nil(t, noEvent)

	stats, err := store.BatchProjectStats(ctx)
	if err != nil {
		return fmt.Errorf("project stats: %w", err)
	}
	assert.Equal(t, 1, stats[project.ID].Closed)
	assert.Equal(t, 0, stats[project.ID].Open)

	reopened, reopenEvent, changed, err := store.ReopenIssue(ctx, issue.ID, "conformance-agent")
	if err != nil {
		return fmt.Errorf("reopen issue: %w", err)
	}
	assert.True(t, changed)
	assert.Equal(t, "open", reopened.Status)
	require.NotNil(t, reopenEvent)

	deleted, deleteEvent, changed, err := store.SoftDeleteIssue(ctx, issue.ID, "conformance-agent")
	if err != nil {
		return fmt.Errorf("soft delete issue: %w", err)
	}
	assert.True(t, changed)
	require.NotNil(t, deleted.DeletedAt)
	require.NotNil(t, deleteEvent)
	_, err = store.IssueByShortID(ctx, project.ID, issue.ShortID, db.IncludeDeletedNo)
	assert.ErrorIs(t, err, db.ErrNotFound)
	restored, restoreEvent, changed, err := store.RestoreIssue(ctx, issue.ID, "conformance-agent")
	if err != nil {
		return fmt.Errorf("restore issue: %w", err)
	}
	assert.True(t, changed)
	assert.Nil(t, restored.DeletedAt)
	require.NotNil(t, restoreEvent)

	prefixMatches, err := store.IssueUIDPrefixMatch(ctx, issue.UID[:8], 10, db.IncludeDeletedNo)
	if err != nil {
		return fmt.Errorf("issue uid prefix: %w", err)
	}
	require.Len(t, prefixMatches, 1)
	all, err := store.ListAllIssues(ctx, db.ListAllIssuesParams{ProjectID: project.ID})
	if err != nil {
		return fmt.Errorf("list all issues: %w", err)
	}
	require.Len(t, all, 1)

	start := make(chan struct{})
	results := make(chan struct {
		changed bool
		err     error
	}, 2)
	for range 2 {
		go func() {
			<-start
			_, _, changed, err := store.CloseIssue(
				ctx, issue.ID, "done", "conformance-agent", "concurrent close", nil,
			)
			results <- struct {
				changed bool
				err     error
			}{changed: changed, err: err}
		}()
	}
	close(start)
	changedCount := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			return fmt.Errorf("concurrent close: %w", result.err)
		}
		if result.changed {
			changedCount++
		}
	}
	assert.Equal(t, 1, changedCount)
	return nil
}

func checkEventQueries(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "event-query-project")
	if err != nil {
		return fmt.Errorf("create project: %w", err)
	}
	since := time.Now().Add(-time.Minute)
	_, event, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "event query", Author: "conformance-agent",
	})
	if err != nil {
		return fmt.Errorf("create issue: %w", err)
	}
	byUID, err := store.EventsByUIDs(ctx, project.ID, []string{event.UID})
	if err != nil {
		return fmt.Errorf("events by uid: %w", err)
	}
	require.Len(t, byUID, 1)
	assert.Equal(t, event.ID, byUID[0].ID)
	window, err := store.EventsInWindow(ctx, db.EventsInWindowParams{
		ProjectID: project.ID, Since: since.UTC().Format(time.RFC3339Nano),
		Until: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return fmt.Errorf("events in window: %w", err)
	}
	require.Len(t, window, 1)
	localMax, err := store.MaxLocalOriginEventID(ctx, project.ID)
	if err != nil {
		return fmt.Errorf("max local event id: %w", err)
	}
	assert.Equal(t, event.ID, localMax)
	baselineMax, err := store.MaxFederationBaselineEventID(ctx, project.ID, 0)
	if err != nil {
		return fmt.Errorf("max baseline event id: %w", err)
	}
	assert.Zero(t, baselineMax)
	return nil
}

func checkIssueCreateEnvelope(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "create-envelope-project")
	if err != nil {
		return fmt.Errorf("create project: %w", err)
	}
	target, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "link target", Author: "conformance-agent",
	})
	if err != nil {
		return fmt.Errorf("create target: %w", err)
	}
	priority := int64(1)
	owner := "initial-owner"
	issue, event, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "complete initial state",
		Body:      "replay must reproduce this row",
		Author:    "conformance-agent",
		Owner:     &owner,
		Priority:  &priority,
		Labels:    []string{"backend", "backend"},
		Links: []db.InitialLink{
			{Type: "related", ToNumber: target.ID},
			{Type: "related", ToNumber: target.ID, Incoming: true},
		},
	})
	if err != nil {
		return fmt.Errorf("create complete issue: %w", err)
	}
	require.NotNil(t, issue.Owner)
	assert.Equal(t, owner, *issue.Owner)
	require.NotNil(t, issue.Priority)
	assert.Equal(t, priority, *issue.Priority)
	var payload struct {
		UID      string `json:"uid"`
		ShortID  string `json:"short_id"`
		Title    string `json:"title"`
		Body     string `json:"body"`
		Owner    string `json:"owner"`
		Priority int64  `json:"priority"`
		Status   string `json:"status"`
		Metadata any    `json:"metadata"`
		Labels   []string
		Links    []struct {
			Type       string `json:"type"`
			ToIssueUID string `json:"to_issue_uid"`
		} `json:"links"`
	}
	require.NoError(t, json.Unmarshal([]byte(event.Payload), &payload))
	assert.Equal(t, issue.UID, payload.UID)
	assert.Equal(t, issue.ShortID, payload.ShortID)
	assert.Equal(t, issue.Title, payload.Title)
	assert.Equal(t, issue.Body, payload.Body)
	assert.Equal(t, owner, payload.Owner)
	assert.Equal(t, priority, payload.Priority)
	assert.Equal(t, "open", payload.Status)
	assert.Equal(t, []string{"backend"}, payload.Labels)
	require.Len(t, payload.Links, 1)
	assert.Equal(t, target.UID, payload.Links[0].ToIssueUID)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "self link", Author: "conformance-agent",
		Links: []db.InitialLink{{Type: "related", ToNumber: issue.ID + 1}},
	})
	assert.ErrorIs(t, err, db.ErrSelfLink)
	secondTarget, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "second parent target", Author: "conformance-agent",
	})
	if err != nil {
		return fmt.Errorf("create second parent target: %w", err)
	}
	stringMetadataIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "string metadata", Author: "conformance-agent",
		Metadata: map[string]json.RawMessage{"estimate": json.RawMessage(`"3"`)},
	})
	if err != nil {
		return fmt.Errorf("create string metadata issue: %w", err)
	}
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "numeric metadata", Author: "conformance-agent",
		Metadata: map[string]json.RawMessage{"estimate": json.RawMessage(`3`)},
	})
	if err != nil {
		return fmt.Errorf("create numeric metadata issue: %w", err)
	}
	metadataMatches, err := store.ListIssues(ctx, db.ListIssuesParams{
		ProjectID: project.ID,
		Meta:      []db.MetaFilter{{Key: "estimate", Value: "3", HasValue: true}},
	})
	if err != nil {
		return fmt.Errorf("filter string metadata: %w", err)
	}
	require.Len(t, metadataMatches, 1)
	assert.Equal(t, stringMetadataIssue.ID, metadataMatches[0].ID)

	before, err := store.MaxEventID(ctx)
	if err != nil {
		return fmt.Errorf("max event before failed creates: %w", err)
	}
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "bad link", Author: "conformance-agent",
		Links: []db.InitialLink{{Type: "unknown", ToNumber: target.ID}},
	})
	assert.ErrorIs(t, err, db.ErrInitialLinkInvalidType)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "missing link", Author: "conformance-agent",
		Links: []db.InitialLink{{Type: "related", ToNumber: target.ID + 100000}},
	})
	assert.ErrorIs(t, err, db.ErrInitialLinkTargetNotFound)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "bad label", Author: "conformance-agent", Labels: []string{"Bad Label"},
	})
	assert.ErrorIs(t, err, db.ErrLabelInvalid)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "two parents", Author: "conformance-agent",
		Links: []db.InitialLink{
			{Type: "parent", ToNumber: target.ID},
			{Type: "parent", ToNumber: secondTarget.ID},
		},
	})
	assert.ErrorIs(t, err, db.ErrParentAlreadySet)
	after, err := store.MaxEventID(ctx)
	if err != nil {
		return fmt.Errorf("max event after failed creates: %w", err)
	}
	assert.Equal(t, before, after)
	return nil
}

func checkConcurrentIssueIdentity(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "concurrent-identity-project")
	if err != nil {
		return fmt.Errorf("create project: %w", err)
	}
	firstUID, err := uid.New()
	if err != nil {
		return fmt.Errorf("generate first uid: %w", err)
	}
	secondBytes := []byte(firstUID)
	if secondBytes[1] == '0' {
		secondBytes[1] = '1'
	} else {
		secondBytes[1] = '0'
	}
	secondUID := string(secondBytes)
	require.True(t, uid.Valid(secondUID))
	start := make(chan struct{})
	results := make(chan struct {
		issue db.Issue
		event db.Event
		err   error
	}, 2)
	for index, issueUID := range []string{firstUID, secondUID} {
		go func(index int, issueUID string) {
			<-start
			issue, event, err := store.CreateIssue(ctx, db.CreateIssueParams{
				ProjectID: project.ID, UID: issueUID,
				Title: fmt.Sprintf("concurrent issue %d", index), Author: "conformance-agent",
			})
			results <- struct {
				issue db.Issue
				event db.Event
				err   error
			}{issue: issue, event: event, err: err}
		}(index, issueUID)
	}
	close(start)
	shortIDs := make(map[string]struct{}, 2)
	hlcValues := make(map[db.EventHLCTimestamp]struct{}, 2)
	for range 2 {
		result := <-results
		if result.err != nil {
			return fmt.Errorf("concurrent issue create: %w", result.err)
		}
		shortIDs[result.issue.ShortID] = struct{}{}
		hlcValues[db.EventHLCTimestamp{
			PhysicalMS: result.event.HLCPhysicalMS,
			Counter:    result.event.HLCCounter,
		}] = struct{}{}
	}
	assert.Len(t, shortIDs, 2)
	assert.Len(t, hlcValues, 2)
	return nil
}

func checkConcurrentOwnerClaim(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "concurrent-owner-project")
	if err != nil {
		return fmt.Errorf("create project: %w", err)
	}
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "claim once", Author: "conformance-agent",
	})
	if err != nil {
		return fmt.Errorf("create issue: %w", err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, actor := range []string{"claimant-one", "claimant-two"} {
		go func(actor string) {
			<-start
			_, err := store.ClaimOwner(ctx, issue.ID, actor, false)
			results <- err
		}(actor)
	}
	close(start)
	var success, conflicts int
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			success++
		case errors.Is(err, db.ErrAlreadyClaimed):
			conflicts++
		default:
			return fmt.Errorf("concurrent owner claim: %w", err)
		}
	}
	assert.Equal(t, 1, success)
	assert.Equal(t, 1, conflicts)
	return nil
}
