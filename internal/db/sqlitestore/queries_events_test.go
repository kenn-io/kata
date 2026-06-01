package sqlitestore_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestMaxEventID_EmptyTable(t *testing.T) {
	d := openTestDB(t)
	got, err := d.MaxEventID(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(0), got)
}

func TestMaxEventID_AfterInserts(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createProject(ctx, t, d, "a")
	createTesterIssues(ctx, t, d, p.ID, 3)
	got, err := d.MaxEventID(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(3), got, "three issue.created events → highest event id is 3")
}

func TestEventsAfter_CrossProject(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	pa := createProject(ctx, t, d, "a")
	pb := createProject(ctx, t, d, "b")
	createTesterIssue(ctx, t, d, pa.ID, "a1")
	createTesterIssue(ctx, t, d, pb.ID, "b1")

	all, err := d.EventsAfter(ctx, db.EventsAfterParams{AfterID: 0, Limit: 100})
	require.NoError(t, err)
	assert.Len(t, all, 2)
	assert.Equal(t, int64(1), all[0].ID)
	assert.Equal(t, int64(2), all[1].ID)
}

func TestEventsAfter_ExcludesSystemProjectFromCrossProjectFeed(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createProject(ctx, t, d, "a")
	_, normal := createTesterIssue(ctx, t, d, p.ID, "a1")
	_, _, err := d.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "secret-token",
		Actor:          "wesm",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)
	sys, err := d.SystemProject(ctx)
	require.NoError(t, err)

	all, err := d.EventsAfter(ctx, db.EventsAfterParams{AfterID: 0, Limit: 100})
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, normal.ID, all[0].ID)
	assert.Equal(t, "issue.created", all[0].Type)

	systemRows, err := d.EventsAfter(ctx, db.EventsAfterParams{
		AfterID: 0, ProjectID: sys.ID, Limit: 100,
	})
	require.NoError(t, err)
	assert.Empty(t, systemRows)
}

func TestEventsAfter_PerProjectFilter(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	pa := createProject(ctx, t, d, "a")
	pb := createProject(ctx, t, d, "b")
	createTesterIssue(ctx, t, d, pa.ID, "a1")
	createTesterIssue(ctx, t, d, pb.ID, "b1")

	onlyA, err := d.EventsAfter(ctx, db.EventsAfterParams{AfterID: 0, ProjectID: pa.ID, Limit: 100})
	require.NoError(t, err)
	require.Len(t, onlyA, 1)
	assert.Equal(t, pa.ID, onlyA[0].ProjectID)
}

func TestEventsAfter_RespectsThroughID(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createProject(ctx, t, d, "a")
	createTesterIssues(ctx, t, d, p.ID, 5)
	got, err := d.EventsAfter(ctx, db.EventsAfterParams{AfterID: 0, ThroughID: 3, Limit: 100})
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, int64(3), got[2].ID)
}

func TestEventsAfter_RespectsLimit(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createProject(ctx, t, d, "a")
	createTesterIssues(ctx, t, d, p.ID, 5)
	got, err := d.EventsAfter(ctx, db.EventsAfterParams{AfterID: 0, Limit: 2})
	require.NoError(t, err)
	assert.Len(t, got, 2)
}

func TestEventsAfter_StrictlyAfterNonZeroID(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createProject(ctx, t, d, "a")
	createTesterIssues(ctx, t, d, p.ID, 5)
	// Five issue.created events with ids 1..5. AfterID=3 must return ids 4, 5
	// (strict >); AfterID=5 must return zero rows.
	got, err := d.EventsAfter(ctx, db.EventsAfterParams{AfterID: 3, Limit: 100})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, int64(4), got[0].ID)
	assert.Equal(t, int64(5), got[1].ID)

	none, err := d.EventsAfter(ctx, db.EventsAfterParams{AfterID: 5, Limit: 100})
	require.NoError(t, err)
	assert.Len(t, none, 0, "AfterID at the highest event id must return no rows (strict >)")
}

func TestEventsInWindow_ExcludesSystemProjectFromCrossProjectFeed(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createProject(ctx, t, d, "a")
	_, normal := createTesterIssue(ctx, t, d, p.ID, "a1")
	_, _, err := d.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "secret-token",
		Actor:          "wesm",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)

	all, err := d.EventsInWindow(ctx, db.EventsInWindowParams{
		Since: "1970-01-01T00:00:00.000Z",
		Until: "2999-01-01T00:00:00.000Z",
	})
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, normal.ID, all[0].ID)
	assert.Equal(t, "issue.created", all[0].Type)
}

func TestPurgeResetCheck_NoPurges(t *testing.T) {
	d := openTestDB(t)
	got, err := d.PurgeResetCheck(context.Background(), 0, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(0), got)
}

func TestPurgeResetCheck_AfterPurgeWithEvents(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createProject(ctx, t, d, "a")
	is, _ := createTesterIssue(ctx, t, d, p.ID, "doomed")
	_, err := d.PurgeIssue(ctx, is.ID, "tester", nil)
	require.NoError(t, err)

	// cursor below the reset → returns the reset cursor
	got, err := d.PurgeResetCheck(ctx, 0, 0)
	require.NoError(t, err)
	assert.Greater(t, got, int64(0), "purge of an issue with events reserves a synthetic cursor")

	// cursor at-or-above the reset → returns 0
	zero, err := d.PurgeResetCheck(ctx, got, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(0), zero, "PurgeResetCheck uses strict > so cursor==reset is unaffected")
}

func TestMaxLocalOriginEventID(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createProject(ctx, t, d, "p")

	got, err := d.MaxLocalOriginEventID(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), got, "no events yet → MAX over empty set surfaces as 0")

	createTesterIssue(ctx, t, d, p.ID, "t")

	got, err = d.MaxLocalOriginEventID(ctx, p.ID)
	require.NoError(t, err)
	assert.Greater(t, got, int64(0), "issue creation produced a local-origin event")
}

func TestMaxFederationBaselineEventID(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createProject(ctx, t, d, "p")

	// No baseline snapshots yet — MAX() over an empty set is NULL, surfaced as 0.
	got, err := d.MaxFederationBaselineEventID(ctx, p.ID, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(0), got)
}

func TestPurgeResetCheck_PerProject(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	pa := createProject(ctx, t, d, "a")
	pb := createProject(ctx, t, d, "b")
	is, _ := createTesterIssue(ctx, t, d, pa.ID, "doomed")
	_, err := d.PurgeIssue(ctx, is.ID, "tester", nil)
	require.NoError(t, err)

	// per-project filter: a purge in A is invisible to a B-scoped subscriber
	got, err := d.PurgeResetCheck(ctx, 0, pb.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), got, "per-project filter excludes other-project purges")

	// per-project filter: visible to A-scoped subscriber
	gotA, err := d.PurgeResetCheck(ctx, 0, pa.ID)
	require.NoError(t, err)
	assert.Greater(t, gotA, int64(0))
}
