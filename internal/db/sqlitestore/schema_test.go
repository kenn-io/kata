package sqlitestore_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestIssuesMetadataAndRevisionColumns(t *testing.T) {
	d, ctx, _, iss := setupTestIssue(t)
	var meta string
	var rev int64
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT metadata, revision FROM issues WHERE id = ?`, iss.ID,
	).Scan(&meta, &rev))
	assert.Equal(t, "{}", meta, "metadata default must be '{}'")
	assert.Equal(t, int64(1), rev, "revision default must be 1")
}

func TestIssuesMetadataRejectsInvalidJSON(t *testing.T) {
	d, ctx, _, iss := setupTestIssue(t)
	_, err := d.ExecContext(ctx,
		`UPDATE issues SET metadata = 'not json' WHERE id = ?`, iss.ID,
	)
	require.Error(t, err, "json_valid CHECK must reject non-JSON metadata")
}

// TestIssuesMetadataRejectsNonObjectShapes pins the json_type = 'object'
// CHECK on issues.metadata. Arrays, scalars, and nulls are rejected so the
// read path's `json.Unmarshal(meta, &map[string]…)` cannot crash on a row
// corrupted by direct SQL or an upstream bug.
func TestIssuesMetadataRejectsNonObjectShapes(t *testing.T) {
	d, ctx, _, iss := setupTestIssue(t)
	for _, badShape := range []string{`[]`, `[1,2,3]`, `42`, `"hello"`, `null`, `true`} {
		_, err := d.ExecContext(ctx,
			`UPDATE issues SET metadata = ? WHERE id = ?`, badShape, iss.ID,
		)
		require.Errorf(t, err,
			"json_type CHECK must reject non-object metadata: %s", badShape)
	}
}

func TestProjectsMetadataAndRevisionColumns(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	var meta string
	var rev int64
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT metadata, revision FROM projects WHERE id = ?`, p.ID,
	).Scan(&meta, &rev))
	assert.Equal(t, "{}", meta, "metadata default must be '{}'")
	assert.Equal(t, int64(1), rev, "revision default must be 1")
}

func TestProjectsMetadataRejectsInvalidJSON(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	_, err := d.ExecContext(ctx,
		`UPDATE projects SET metadata = 'not json' WHERE id = ?`, p.ID,
	)
	require.Error(t, err, "json_valid CHECK must reject non-JSON metadata")
}

// TestProjectsMetadataRejectsNonObjectShapes mirrors the issues check at the
// projects table — see TestIssuesMetadataRejectsNonObjectShapes for rationale.
func TestProjectsMetadataRejectsNonObjectShapes(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	for _, badShape := range []string{`[]`, `[1,2,3]`, `42`, `"hello"`, `null`, `true`} {
		_, err := d.ExecContext(ctx,
			`UPDATE projects SET metadata = ? WHERE id = ?`, badShape, p.ID,
		)
		require.Errorf(t, err,
			"json_type CHECK must reject non-object metadata: %s", badShape)
	}
}

// TestRecurrencesTemplateMetadataRejectsNonObjectShapes pins the json_type
// CHECK on recurrences.template_metadata — same rationale as the issue /
// project checks.
func TestRecurrencesTemplateMetadataRejectsNonObjectShapes(t *testing.T) {
	d, ctx, p, _ := setupTestIssue(t)
	_, err := d.ExecContext(ctx, `INSERT INTO recurrences
        (uid, project_id, rrule, dtstart, timezone, template_title, author)
        VALUES ('01J0000000000000000000RECC', ?, 'FREQ=WEEKLY', '2026-05-15',
                'UTC', 't', 'tester')`, p.ID)
	require.NoError(t, err)

	for _, badShape := range []string{`[]`, `42`, `"x"`, `null`} {
		_, err := d.ExecContext(ctx,
			`UPDATE recurrences SET template_metadata = ?
			 WHERE uid = '01J0000000000000000000RECC'`, badShape)
		require.Errorf(t, err,
			"json_type CHECK must reject non-object template_metadata: %s", badShape)
	}
}

// TestRecurrencesTemplateLabelsRejectsNonArrayShapes pins the json_type
// CHECK on recurrences.template_labels — labels are always a JSON array of
// strings, so non-array shapes must be rejected at write time.
func TestRecurrencesTemplateLabelsRejectsNonArrayShapes(t *testing.T) {
	d, ctx, p, _ := setupTestIssue(t)
	_, err := d.ExecContext(ctx, `INSERT INTO recurrences
        (uid, project_id, rrule, dtstart, timezone, template_title, author)
        VALUES ('01J0000000000000000000RECL', ?, 'FREQ=WEEKLY', '2026-05-15',
                'UTC', 't', 'tester')`, p.ID)
	require.NoError(t, err)

	for _, badShape := range []string{`{}`, `42`, `"x"`, `null`} {
		_, err := d.ExecContext(ctx,
			`UPDATE recurrences SET template_labels = ?
			 WHERE uid = '01J0000000000000000000RECL'`, badShape)
		require.Errorf(t, err,
			"json_type CHECK must reject non-array template_labels: %s", badShape)
	}
}

func TestEventsCarryOriginInstanceUID(t *testing.T) {
	d, ctx, _, _ := setupTestIssue(t) // creates project + issue + issue.created event

	var id int64
	var origUID string
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT id, origin_instance_uid FROM events
		   ORDER BY id DESC LIMIT 1`,
	).Scan(&id, &origUID))
	assert.Equal(t, d.InstanceUID(), origUID,
		"origin_instance_uid must be this daemon's instance_uid")
}

func TestRecurrencesTableAndIssueLinkage(t *testing.T) {
	d, ctx, p, _ := setupTestIssue(t)

	_, err := d.ExecContext(ctx, `INSERT INTO recurrences
        (uid, project_id, rrule, dtstart, timezone, template_title, author)
        VALUES ('01J0000000000000000000REC1', ?, 'FREQ=WEEKLY', '2026-05-15',
                'America/New_York', 'Pay rent', 'tester')`, p.ID)
	require.NoError(t, err)

	// recurrence_id + occurrence_key columns exist on issues
	var n int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('issues')
           WHERE name IN ('recurrence_id','occurrence_key')`,
	).Scan(&n))
	assert.Equal(t, 2, n)

	// unique index exists
	var idxn int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master
           WHERE type='index' AND name='issues_recurrence_occurrence_uniq'`,
	).Scan(&idxn))
	assert.Equal(t, 1, idxn)
}

func TestSchemaVersionCurrent(t *testing.T) {
	d := openTestDB(t)
	assertSchemaVersion(t, d, db.CurrentSchemaVersion())
}
