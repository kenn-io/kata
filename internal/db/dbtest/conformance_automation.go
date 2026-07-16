package dbtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func issueIDs(issues []db.Issue) []int64 {
	ids := make([]int64, 0, len(issues))
	for _, issue := range issues {
		ids = append(ids, issue.ID)
	}
	return ids
}

func readyGlobalIssueIDs(issues []db.ReadyGlobalIssue) []int64 {
	ids := make([]int64, 0, len(issues))
	for _, issue := range issues {
		ids = append(ids, issue.ID)
	}
	return ids
}

func searchCandidateIDs(candidates []db.SearchCandidate) []int64 {
	ids := make([]int64, 0, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, candidate.Issue.ID)
	}
	return ids
}

func eventTypeNames(events []db.Event) []string {
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	return types
}

func collectExport[T any](sequence iter.Seq2[T, error]) ([]T, error) {
	var values []T
	for value, err := range sequence {
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func checkAPITokensAndSystemProject(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	systemProject, err := store.SystemProject(ctx)
	if err != nil {
		return fmt.Errorf("system project: %w", err)
	}
	assert.Equal(t, db.SystemProjectUID, systemProject.UID)
	assert.Equal(t, db.SystemProjectName, systemProject.Name)
	if err := store.EnsureSystemProject(ctx); err != nil {
		return fmt.Errorf("ensure system project: %w", err)
	}
	_, err = store.ProjectByID(ctx, systemProject.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return fmt.Errorf("list projects with system project: %w", err)
	}
	assert.Empty(t, projects)

	name := "  laptop  "
	token, created, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "secret-token",
		Actor:          "  automation  ",
		Name:           &name,
		AdminActor:     db.BootstrapActor,
	})
	if err != nil {
		return fmt.Errorf("create api token: %w", err)
	}
	assert.Equal(t, "automation", token.Actor)
	require.NotNil(t, token.Name)
	assert.Equal(t, "laptop", *token.Name)
	assert.Nil(t, token.LastUsedAt)
	assert.Nil(t, token.RevokedAt)
	assert.Len(t, token.TokenHash, 64)
	assert.NotEqual(t, "secret-token", token.TokenHash)
	assert.Equal(t, "token.created", created.Type)
	assert.Equal(t, db.BootstrapActor, created.Actor)
	assert.Equal(t, systemProject.ID, created.ProjectID)
	assert.Equal(t, db.SystemProjectName, created.ProjectName)
	var createdPayload struct {
		TokenID     int64   `json:"token_id"`
		TokenHash   string  `json:"token_hash"`
		TargetActor string  `json:"target_actor"`
		Name        *string `json:"name"`
	}
	require.NoError(t, json.Unmarshal([]byte(created.Payload), &createdPayload))
	assert.Equal(t, token.ID, createdPayload.TokenID)
	assert.Equal(t, token.TokenHash, createdPayload.TokenHash)
	assert.Equal(t, token.Actor, createdPayload.TargetActor)
	require.NotNil(t, createdPayload.Name)
	assert.Equal(t, "laptop", *createdPayload.Name)
	assert.NotContains(t, created.Payload, "secret-token")

	resolved, err := store.ResolveAPIToken(ctx, "secret-token")
	if err != nil {
		return fmt.Errorf("resolve api token: %w", err)
	}
	assert.Equal(t, token.ID, resolved.ID)
	require.NotNil(t, resolved.LastUsedAt)
	firstUsed := *resolved.LastUsedAt
	resolved, err = store.ResolveAPIToken(ctx, "secret-token")
	if err != nil {
		return fmt.Errorf("resolve api token again: %w", err)
	}
	require.NotNil(t, resolved.LastUsedAt)
	assert.Equal(t, firstUsed, *resolved.LastUsedAt)

	tokens, err := store.ListAPITokens(ctx)
	if err != nil {
		return fmt.Errorf("list api tokens: %w", err)
	}
	require.Len(t, tokens, 1)
	assert.Empty(t, tokens[0].TokenHash)
	assert.Equal(t, token.ID, tokens[0].ID)
	require.NotNil(t, tokens[0].LastUsedAt)

	revoked, revokedEvent, err := store.RevokeAPIToken(ctx, token.ID, " token-admin ")
	if err != nil {
		return fmt.Errorf("revoke api token: %w", err)
	}
	require.NotNil(t, revoked.RevokedAt)
	assert.Equal(t, "token.revoked", revokedEvent.Type)
	assert.Equal(t, "token-admin", revokedEvent.Actor)
	assert.NotContains(t, revokedEvent.Payload, token.TokenHash)
	var revokedPayload struct {
		TokenID     int64   `json:"token_id"`
		TargetActor string  `json:"target_actor"`
		Name        *string `json:"name"`
	}
	require.NoError(t, json.Unmarshal([]byte(revokedEvent.Payload), &revokedPayload))
	assert.Equal(t, token.ID, revokedPayload.TokenID)
	assert.Equal(t, token.Actor, revokedPayload.TargetActor)
	_, err = store.ResolveAPIToken(ctx, "secret-token")
	assert.ErrorIs(t, err, db.ErrNotFound)
	_, _, err = store.RevokeAPIToken(ctx, token.ID, db.BootstrapActor)
	assert.ErrorIs(t, err, db.ErrNotFound)

	blankName := "  "
	invalid := []db.CreateAPITokenParams{
		{Actor: "actor", AdminActor: db.BootstrapActor},
		{PlaintextToken: "another-token", Actor: "bootstrap", AdminActor: db.BootstrapActor},
		{PlaintextToken: "another-token", Actor: "actor"},
		{PlaintextToken: "another-token", Actor: "actor", AdminActor: db.BootstrapActor, Name: &blankName},
	}
	for _, params := range invalid {
		_, _, err := store.CreateAPIToken(ctx, params)
		assert.Error(t, err)
	}
	return nil
}

func checkRecurrences(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "recurrence-project")
	if err != nil {
		return fmt.Errorf("create recurrence project: %w", err)
	}
	owner := "alice"
	priority := int64(2)
	rec, err := store.CreateRecurrence(ctx, db.CreateRecurrenceIn{
		ProjectID: project.ID,
		Actor:     "scheduler",
		Rule:      "FREQ=WEEKLY;BYDAY=MO",
		DTStart:   "2026-05-11",
		Timezone:  "America/New_York",
		Template: db.RecurrenceTemplate{
			Title: "Weekly review", Body: "What got done?", Owner: &owner, Priority: &priority,
			Labels: []string{"Recurring", " recurring ", "p2"}, Metadata: json.RawMessage(`{"kind":"weekly"}`),
		},
	})
	if err != nil {
		return fmt.Errorf("create recurrence: %w", err)
	}
	if !uid.Valid(rec.UID) {
		return fmt.Errorf("invalid recurrence UID %q", rec.UID)
	}
	assert.Equal(t, int64(1), rec.Revision)
	assert.JSONEq(t, `["p2","recurring"]`, string(rec.TemplateLabels))
	assert.JSONEq(t, `{"kind":"weekly"}`, string(rec.TemplateMetadata))
	require.NotNil(t, rec.NextOccurrenceKey)
	assert.Equal(t, "2026-05-11", *rec.NextOccurrenceKey)
	byID, err := store.GetRecurrenceByID(ctx, rec.ID)
	if err != nil {
		return fmt.Errorf("get recurrence by ID: %w", err)
	}
	assert.Equal(t, rec.UID, byID.UID)
	byUID, err := store.GetRecurrenceByUID(ctx, rec.UID)
	if err != nil {
		return fmt.Errorf("get recurrence by UID: %w", err)
	}
	assert.Equal(t, rec.ID, byUID.ID)
	_, err = store.GetRecurrenceByID(ctx, rec.ID+10000)
	assert.ErrorIs(t, err, db.ErrNotFound)
	listed, err := store.ListRecurrencesByProject(ctx, project.ID)
	if err != nil {
		return fmt.Errorf("list recurrences: %w", err)
	}
	require.Len(t, listed, 1)
	assert.Equal(t, rec.ID, listed[0].ID)

	noChange, err := store.PatchRecurrence(ctx, db.PatchRecurrenceIn{
		RecurrenceID: rec.ID, IfMatchRev: rec.Revision, Actor: "scheduler",
		Update: db.RecurrenceUpdate{TemplateTitle: &rec.TemplateTitle},
	})
	if err != nil {
		return fmt.Errorf("no-op recurrence patch: %w", err)
	}
	assert.False(t, noChange.Changed)
	assert.Equal(t, rec.Revision, noChange.NewRevision)
	newTitle := "Weekly retrospective"
	newLabels := []string{"planning", "Planning", "p2"}
	patched, err := store.PatchRecurrence(ctx, db.PatchRecurrenceIn{
		RecurrenceID: rec.ID, IfMatchRev: rec.Revision, Actor: "scheduler",
		Update: db.RecurrenceUpdate{TemplateTitle: &newTitle, TemplateLabels: &newLabels},
	})
	if err != nil {
		return fmt.Errorf("patch recurrence: %w", err)
	}
	assert.True(t, patched.Changed)
	assert.Equal(t, int64(2), patched.NewRevision)
	assert.Equal(t, newTitle, patched.Recurrence.TemplateTitle)
	assert.JSONEq(t, `["p2","planning"]`, string(patched.Recurrence.TemplateLabels))
	_, err = store.PatchRecurrence(ctx, db.PatchRecurrenceIn{
		RecurrenceID: rec.ID, IfMatchRev: 1, Actor: "scheduler",
		Update: db.RecurrenceUpdate{TemplateTitle: &rec.TemplateTitle},
	})
	var conflict *db.RevisionConflictError
	assert.ErrorAs(t, err, &conflict)
	if conflict != nil {
		assert.Equal(t, int64(2), conflict.CurrentRevision)
	}

	materialized, err := store.MaterializeNext(ctx, rec.ID, "2026-05-11", "scheduler")
	if err != nil {
		return fmt.Errorf("materialize recurrence: %w", err)
	}
	assert.False(t, materialized.Skipped)
	assert.Equal(t, "2026-05-18", materialized.OccurrenceKey)
	assert.NotZero(t, materialized.NewIssueID)
	if !uid.Valid(materialized.NewIssueUID) {
		return fmt.Errorf("invalid materialized issue UID %q", materialized.NewIssueUID)
	}
	issue, err := store.IssueByID(ctx, materialized.NewIssueID)
	if err != nil {
		return fmt.Errorf("get materialized issue: %w", err)
	}
	assert.Equal(t, newTitle, issue.Title)
	assert.Equal(t, "What got done?", issue.Body)
	assert.Equal(t, &owner, issue.Owner)
	assert.Equal(t, &priority, issue.Priority)
	assert.Equal(t, &rec.ID, issue.RecurrenceID)
	assert.Equal(t, &materialized.OccurrenceKey, issue.OccurrenceKey)
	var issueMetadata map[string]any
	require.NoError(t, json.Unmarshal([]byte(issue.Metadata), &issueMetadata))
	assert.Equal(t, "weekly", issueMetadata["kind"])
	assert.Equal(t, materialized.OccurrenceKey, issueMetadata["scheduled_on"])
	labels, err := store.LabelsForIssue(ctx, issue.ID)
	if err != nil {
		return fmt.Errorf("labels for materialized issue: %w", err)
	}
	assert.Equal(t, []string{"p2", "planning"}, labels)

	skipped, err := store.MaterializeNext(ctx, rec.ID, "2026-05-11", "scheduler")
	if err != nil {
		return fmt.Errorf("repeat recurrence materialization: %w", err)
	}
	assert.True(t, skipped.Skipped)
	assert.Equal(t, materialized.NewIssueUID, skipped.NewIssueUID)
	assert.Equal(t, materialized.OccurrenceKey, skipped.OccurrenceKey)
	closedIssue, closeEvents, changed, err := store.CloseIssueWithEvents(
		ctx, materialized.NewIssueID, "done", "scheduler", "completed", nil,
	)
	if err != nil {
		return fmt.Errorf("close recurrence occurrence: %w", err)
	}
	assert.True(t, changed)
	assert.Equal(t, "closed", closedIssue.Status)
	require.NotEmpty(t, closeEvents)
	assert.Equal(t, []string{"issue.closed", "issue.created", "recurrence.materialized"},
		eventTypeNames(closeEvents))
	issues, err := store.ListIssues(ctx, db.ListIssuesParams{ProjectID: project.ID})
	if err != nil {
		return fmt.Errorf("list recurrence issues after close: %w", err)
	}
	var nextIssue *db.Issue
	for index := range issues {
		if issues[index].OccurrenceKey != nil && *issues[index].OccurrenceKey == "2026-05-25" {
			nextIssue = &issues[index]
			break
		}
	}
	require.NotNil(t, nextIssue, "closing a done occurrence must materialize the next occurrence")
	assert.Equal(t, "open", nextIssue.Status)

	if err := store.SoftDeleteRecurrence(ctx, rec.ID, "scheduler"); err != nil {
		return fmt.Errorf("soft-delete recurrence: %w", err)
	}
	listed, err = store.ListRecurrencesByProject(ctx, project.ID)
	if err != nil {
		return fmt.Errorf("list after recurrence delete: %w", err)
	}
	assert.Empty(t, listed)
	deleted, err := store.GetRecurrenceByID(ctx, rec.ID)
	if err != nil {
		return fmt.Errorf("get deleted recurrence: %w", err)
	}
	require.NotNil(t, deleted.DeletedAt)
	assert.Equal(t, int64(6), deleted.Revision)
	if err := store.SoftDeleteRecurrence(ctx, rec.ID, "scheduler"); err == nil {
		return errors.New("deleting recurrence twice unexpectedly succeeded")
	}

	events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
	if err != nil {
		return fmt.Errorf("list recurrence events: %w", err)
	}
	eventTypes := make([]string, 0, len(events))
	for _, event := range events {
		eventTypes = append(eventTypes, event.Type)
	}
	assert.Contains(t, eventTypes, "recurrence.created")
	assert.Contains(t, eventTypes, "recurrence.updated")
	assert.Contains(t, eventTypes, "issue.created")
	assert.Contains(t, eventTypes, "recurrence.materialized")
	assert.Contains(t, eventTypes, "recurrence.materialization_skipped")
	assert.Contains(t, eventTypes, "recurrence.deleted")

	_, err = store.CreateRecurrence(ctx, db.CreateRecurrenceIn{
		ProjectID: project.ID, Actor: "scheduler", Rule: "FREQ=WEEKLY", DTStart: "2026-05-11",
		Timezone: "Mars/Phobos", Template: db.RecurrenceTemplate{Title: "invalid timezone"},
	})
	assert.ErrorIs(t, err, db.ErrInvalidRecurrence)
	_, err = store.CreateRecurrence(ctx, db.CreateRecurrenceIn{
		ProjectID: project.ID, Actor: "scheduler", Rule: "FREQ=WEEKLY", DTStart: "2026-05-11",
		Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "invalid label", Labels: []string{"two words"}},
	})
	assert.ErrorIs(t, err, db.ErrLabelInvalid)
	return nil
}

func checkCloseAuditQueries(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	primary, err := store.CreateProject(ctx, "close-audit-primary")
	if err != nil {
		return fmt.Errorf("create close audit project: %w", err)
	}
	parent, err := createFixtureIssue(ctx, store, primary.ID, "parent", "author", nil)
	if err != nil {
		return err
	}
	first, err := createFixtureIssue(ctx, store, primary.ID, "first sibling", "agent", nil)
	if err != nil {
		return err
	}
	second, err := createFixtureIssue(ctx, store, primary.ID, "second sibling", "agent", nil)
	if err != nil {
		return err
	}
	refused, err := createFixtureIssue(ctx, store, primary.ID, "refused sibling", "agent", nil)
	if err != nil {
		return err
	}
	for _, child := range []db.Issue{first, second, refused} {
		if _, err := store.CreateLink(ctx, db.CreateLinkParams{
			FromIssueID: child.ID, ToIssueID: parent.ID, Type: "parent", Author: "agent",
		}); err != nil {
			return fmt.Errorf("create parent link: %w", err)
		}
	}
	if _, _, _, err := store.CloseIssue(
		ctx, first.ID, "done", "agent", "  Shared   message. ", nil,
	); err != nil {
		return fmt.Errorf("close first sibling: %w", err)
	}
	if _, _, _, err := store.CloseIssue(
		ctx, second.ID, "wontfix", "agent", "shared message", nil,
	); err != nil {
		return fmt.Errorf("close second sibling: %w", err)
	}

	archivedProject, err := store.CreateProject(ctx, "close-audit-archived")
	if err != nil {
		return fmt.Errorf("create archived sibling project: %w", err)
	}
	archivedSibling, err := createFixtureIssue(ctx, store, archivedProject.ID, "archived sibling", "agent", nil)
	if err != nil {
		return err
	}
	if _, err := store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: archivedSibling.ID, ToIssueID: parent.ID, Type: "parent", Author: "agent",
	}); err != nil {
		return fmt.Errorf("create archived sibling parent link: %w", err)
	}
	if _, _, _, err := store.CloseIssue(
		ctx, archivedSibling.ID, "done", "agent", "archived close", nil,
	); err != nil {
		return fmt.Errorf("close archived sibling: %w", err)
	}
	if _, _, err := store.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: archivedProject.ID, Actor: "admin", Force: true,
	}); err != nil {
		return fmt.Errorf("archive sibling project: %w", err)
	}

	siblings, err := store.RecentSiblingCloses(ctx, parent.ID, refused.ID, "agent", time.Time{})
	if err != nil {
		return fmt.Errorf("recent sibling closes: %w", err)
	}
	require.Len(t, siblings, 2)
	siblingIDs := make([]int64, 0, len(siblings))
	for _, event := range siblings {
		require.NotNil(t, event.IssueID)
		siblingIDs = append(siblingIDs, *event.IssueID)
		assert.NotEmpty(t, event.IssueShortID)
	}
	assert.ElementsMatch(t, []int64{first.ID, second.ID}, siblingIDs)
	future, err := store.RecentSiblingCloses(ctx, parent.ID, refused.ID, "agent", time.Now().Add(time.Hour))
	if err != nil {
		return fmt.Errorf("future sibling closes: %w", err)
	}
	assert.Empty(t, future)

	matching, err := store.RecentSameMessageClose(
		ctx, parent.ID, refused.ID, "agent", db.NormalizeCloseMessage("shared message"), time.Time{},
	)
	if err != nil {
		return fmt.Errorf("recent same-message close: %w", err)
	}
	require.NotNil(t, matching)
	require.NotNil(t, matching.IssueID)
	assert.Equal(t, first.ID, *matching.IssueID)
	missing, err := store.RecentSameMessageClose(
		ctx, parent.ID, refused.ID, "agent", "different", time.Time{},
	)
	if err != nil {
		return fmt.Errorf("missing same-message close: %w", err)
	}
	assert.Nil(t, missing)

	prior := first.ShortID
	throttled, err := store.InsertCloseThrottledEvent(ctx, refused.ID, "agent", db.CloseThrottledPayload{
		Reason: db.CloseThrottleReasonDuplicateMessage,
		Parent: parent.ShortID,
		Prior:  &prior,
	})
	if err != nil {
		return fmt.Errorf("insert close throttled event: %w", err)
	}
	assert.Equal(t, "close.throttled", throttled.Type)
	assert.Equal(t, refused.ID, *throttled.IssueID)
	assert.Equal(t, refused.UID, *throttled.IssueUID)
	assert.Equal(t, primary.UID, throttled.ProjectUID)
	var payload db.CloseThrottledPayload
	require.NoError(t, json.Unmarshal([]byte(throttled.Payload), &payload))
	assert.Equal(t, db.CloseThrottleReasonDuplicateMessage, payload.Reason)
	assert.Equal(t, parent.ShortID, payload.Parent)
	assert.Equal(t, &prior, payload.Prior)
	return nil
}
