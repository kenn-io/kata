package sqlitestore_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/uid"
)

// TestOpenSeedsInstanceUID covers the §8.2 invariant: a fresh db.Open writes
// meta.instance_uid as a valid 26-char ULID and exposes it via InstanceUID().
func TestOpenSeedsInstanceUID(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	got := d.InstanceUID()
	require.NotEmpty(t, got)
	assert.True(t, uid.Valid(got), "InstanceUID %q is not a valid ULID", got)
	var stored string
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&stored))
	assert.Equal(t, got, stored)
}

// TestInstanceUIDStableAcrossReopen covers the spec's "set once at first init,
// never changes" rule: a second db.Open on the same path returns the same UID.
func TestInstanceUIDStableAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	first, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	original := first.InstanceUID()
	require.NoError(t, first.Close())

	second, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = second.Close() })
	assert.Equal(t, original, second.InstanceUID())
}

// TestInstanceUIDCachedAcrossDirectSQLMutation covers §8.3's set-once
// contract: mutating meta.instance_uid out from under an open handle (via
// direct SQL — never something the daemon does, but a guard against future
// code that might) does not change the cached value used by subsequent insert
// paths. The daemon reads meta.instance_uid once at Open and treats it as
// fixed for the process lifetime.
func TestInstanceUIDCachedAcrossDirectSQLMutation(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	original := d.InstanceUID()

	// Stomp the row directly. RefreshInstanceUID exists for the importer's
	// post-commit sync and is intentionally not called here.
	bogus, err := uid.New()
	require.NoError(t, err)
	require.NotEqual(t, original, bogus)
	_, err = d.ExecContext(ctx,
		`UPDATE meta SET value=? WHERE key='instance_uid'`, bogus)
	require.NoError(t, err)

	assert.Equal(t, original, d.InstanceUID(),
		"cached InstanceUID must not move when meta.instance_uid is mutated mid-run")

	p := createKataProject(ctx, t, d)
	_, evt := createTesterIssue(ctx, t, d, p.ID, "post-mutation")
	assert.Equal(t, original, evt.OriginInstanceUID,
		"insert path must stamp the cached origin, not the freshly-mutated row value")
}

// TestEventInsertCarriesUIDAndOrigin covers §8.3: a new event written through
// the daemon's mutation path has a valid UID and origin_instance_uid matching
// the local meta.instance_uid.
func TestEventInsertCarriesUIDAndOrigin(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	p := createKataProject(ctx, t, d)
	_, evt := createTesterIssue(ctx, t, d, p.ID, "uid-stamping")
	assert.True(t, uid.Valid(evt.UID), "event UID %q invalid", evt.UID)
	assert.Equal(t, d.InstanceUID(), evt.OriginInstanceUID)
}

// TestPurgeInsertCarriesUIDAndOrigin covers §8.3 for purge_log: a purge writes
// a row with valid uid + origin_instance_uid matching the local instance.
func TestPurgeInsertCarriesUIDAndOrigin(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	p := createKataProject(ctx, t, d)
	issue, _ := createTesterIssue(ctx, t, d, p.ID, "purge me")
	pl, err := d.PurgeIssue(ctx, issue.ID, "tester", nil)
	require.NoError(t, err)
	assert.True(t, uid.Valid(pl.UID), "purge UID %q invalid", pl.UID)
	assert.Equal(t, d.InstanceUID(), pl.OriginInstanceUID)
}

// TestEventUIDNotNullRejected guards §8.2's NOT NULL contract at runtime: a
// raw INSERT bypassing the daemon's uid generation is rejected by SQLite.
func TestEventUIDNotNullRejected(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	p := createKataProject(ctx, t, d)

	// uid omitted entirely.
	_, err := d.ExecContext(ctx, `
		INSERT INTO events(origin_instance_uid, project_id, project_name, type, actor, payload, created_at)
		VALUES (?, ?, 'github.com/wesm/kata', 'issue.created', 'tester', '{}', strftime('%Y-%m-%dT%H:%M:%fZ','now'))`,
		d.InstanceUID(), p.ID)
	require.Error(t, err, "events.uid NOT NULL must reject INSERT without uid")

	// origin_instance_uid omitted entirely.
	freshUID, err := uid.New()
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `
		INSERT INTO events(uid, project_id, project_name, type, actor, payload, created_at)
		VALUES (?, ?, 'github.com/wesm/kata', 'issue.created', 'tester', '{}', strftime('%Y-%m-%dT%H:%M:%fZ','now'))`,
		freshUID, p.ID)
	require.Error(t, err, "events.origin_instance_uid NOT NULL must reject INSERT without origin")
}

// TestPurgeLogUIDNotNullRejected guards §8.2 for purge_log: NULL uid or
// origin_instance_uid must be rejected at runtime.
func TestPurgeLogUIDNotNullRejected(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)

	// uid omitted entirely.
	_, err := d.ExecContext(ctx, `
		INSERT INTO purge_log(origin_instance_uid, project_id, purged_issue_id, project_name,
		                     issue_title, issue_author,
		                     comment_count, link_count, label_count, event_count, actor, purged_at)
		VALUES (?, 1, 99, 'p', 'title', 'tester', 0, 0, 0, 0, 'tester',
		        strftime('%Y-%m-%dT%H:%M:%fZ','now'))`,
		d.InstanceUID())
	require.Error(t, err, "purge_log.uid NOT NULL must reject INSERT without uid")

	// origin_instance_uid omitted entirely.
	freshUID, err := uid.New()
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `
		INSERT INTO purge_log(uid, project_id, purged_issue_id, project_name,
		                     issue_title, issue_author,
		                     comment_count, link_count, label_count, event_count, actor, purged_at)
		VALUES (?, 1, 99, 'p', 'title', 'tester', 0, 0, 0, 0, 'tester',
		        strftime('%Y-%m-%dT%H:%M:%fZ','now'))`,
		freshUID)
	require.Error(t, err, "purge_log.origin_instance_uid NOT NULL must reject INSERT without origin")
}
