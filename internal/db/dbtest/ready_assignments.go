package dbtest

import (
	"context"
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkReadyAssignmentExpiry(t *testing.T, store db.Storage) error {
	ctx := context.Background()
	// Deliberately differs from wall time: all ownership decisions use At.
	at := time.Date(2035, 1, 2, 12, 0, 0, 0, time.UTC)
	fixture, err := createIssueFixture(ctx, store, "ready-assignment-project", "unassigned", "author", nil)
	require.NoError(t, err)
	owner := "worker"
	permanent, err := createFixtureIssue(ctx, store, fixture.Project.ID, "permanent", "author", &owner)
	require.NoError(t, err)
	timed := func(title string, expires time.Time) db.Issue {
		issue, err := createFixtureIssue(ctx, store, fixture.Project.ID, title, "author", nil)
		require.NoError(t, err)
		result, err := store.ClaimOwner(ctx, db.ClaimOwnerParams{IssueID: issue.ID, Actor: owner, TTL: time.Hour, Now: expires.Add(-time.Hour)})
		require.NoError(t, err)
		return result.Issue
	}
	expired := timed("expired", at.Add(-time.Second))
	boundary := timed("exact boundary", at)
	active := timed("active", at.Add(time.Second))
	scheduled := timed("scheduled", at.Add(-time.Second))
	_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: scheduled.ID, Actor: "author", Patch: map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2035-01-03T12:00:00Z"`)}})
	require.NoError(t, err)
	_, err = store.AddLabel(ctx, expired.ID, "selected", "author")
	require.NoError(t, err)
	blocked := timed("blocked", at.Add(-time.Second))
	_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: permanent.ID, ToIssueID: blocked.ID, Type: "blocks", Author: "author"})
	require.NoError(t, err)
	other, err := createIssueFixture(ctx, store, "other-ready-assignment-project", "other expired", "author", nil)
	require.NoError(t, err)
	_, err = store.ClaimOwner(ctx, db.ClaimOwnerParams{IssueID: other.Issue.ID, Actor: owner, TTL: time.Hour, Now: at.Add(-2 * time.Hour)})
	require.NoError(t, err)
	before, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	stored, err := store.IssueByID(ctx, expired.ID)
	require.NoError(t, err)
	for _, global := range []bool{false, true} {
		name := "project"
		if global {
			name = "global"
		}
		t.Run(name, func(t *testing.T) {
			read := func(filter db.ReadyIssuesFilter, limit int) []db.Issue {
				filter.At = at
				if global {
					rows, err := store.ReadyIssuesGlobal(ctx, limit, filter)
					require.NoError(t, err)
					issues := make([]db.Issue, len(rows))
					for i, row := range rows {
						issues[i] = row.Issue
					}
					return issues
				}
				rows, err := store.ReadyIssues(ctx, fixture.Project.ID, limit, filter)
				require.NoError(t, err)
				return rows
			}
			rows := read(db.ReadyIssuesFilter{Unowned: true}, 0)
			expected := []int64{fixture.Issue.ID, expired.ID, boundary.ID}
			if global {
				expected = append(expected, other.Issue.ID)
			}
			require.ElementsMatch(t, expected, issueIDs(rows))
			for _, row := range rows {
				require.Nil(t, row.Owner)
				require.Nil(t, row.AssignmentExpiresOn)
			}
			rows = read(db.ReadyIssuesFilter{Owner: owner}, 0)
			require.ElementsMatch(t, []int64{permanent.ID, active.ID}, issueIDs(rows))
			rows = read(db.ReadyIssuesFilter{}, 0)
			require.Len(t, rows, len(expected)+2)
			for _, row := range rows {
				if row.ID == expired.ID || row.ID == boundary.ID {
					require.Nil(t, row.Owner)
					require.Nil(t, row.AssignmentExpiresOn)
				}
				if row.ID == active.ID {
					require.Equal(t, owner, *row.Owner)
					require.Equal(t, active.AssignmentExpiresOn, row.AssignmentExpiresOn)
				}
			}
			rows = read(db.ReadyIssuesFilter{Unowned: true, Labels: []string{"selected"}}, 1)
			require.Equal(t, []int64{expired.ID}, issueIDs(rows))
			rows = read(db.ReadyIssuesFilter{Unowned: true, ExcludeLabels: []string{"selected"}, AllowedIssueIDs: []int64{expired.ID, boundary.ID}}, 1)
			require.Equal(t, []int64{boundary.ID}, issueIDs(rows))
		})
	}
	after, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after, "ready reads must not emit events")
	unchanged, err := store.IssueByID(ctx, expired.ID)
	require.NoError(t, err)
	require.Equal(t, stored, unchanged, "effective ownership must not mutate stored state")
	return nil
}
