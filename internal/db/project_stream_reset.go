package db

import (
	"context"
	"database/sql"
	"slices"
	"strconv"
)

// ProjectStreamResetMetadataPrefix identifies one stream reset cursor per
// project. It contains no issue history; it is retired with the project UID.
const ProjectStreamResetMetadataPrefix = "project_stream_reset."

type projectStreamResetQuery interface {
	Transaction
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// PurgeResetProjects finds streams whose retained events will disappear.
// Move payloads also identify former streams when their creation was compacted.
func PurgeResetProjects(ctx context.Context, tx projectStreamResetQuery, issue Issue) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT p.uid,e.type,e.payload FROM events e
 JOIN projects p ON p.id=e.project_id
 WHERE e.issue_id=$1 OR e.issue_uid=$2 OR ((e.related_issue_id=$1 OR e.related_issue_uid=$2)
 AND e.type NOT IN ('issue.links_changed','issue.commented'))`, issue.ID, issue.UID)
	if err != nil {
		return nil, err
	}
	projects := map[string]bool{}
	for rows.Next() {
		var projectUID, eventType, payload string
		if err := rows.Scan(&projectUID, &eventType, &payload); err != nil {
			_ = rows.Close()
			return nil, err
		}
		projects[projectUID] = true
		if eventType == "issue.moved" {
			for _, key := range []string{"from_project_uid", "to_project_uid"} {
				projectUID, _ := StringValue(PayloadMap([]byte(payload))[key])
				if projectUID != "" {
					projects[projectUID] = true
				}
			}
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	delete(projects, issue.ProjectUID) // Its ordinary purge audit already carries the cursor.
	result := make([]string, 0, len(projects))
	for projectUID := range projects {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE uid=$1)`, projectUID).Scan(&exists); err != nil {
			return nil, err
		}
		if exists {
			result = append(result, projectUID)
		}
	}
	slices.Sort(result)
	return result, nil
}

// RecordProjectStreamResets invalidates former project streams atomically with
// history deletion. One metadata value replaces any earlier reset for each UID.
func RecordProjectStreamResets(ctx context.Context, tx Transaction, projectUIDs []string, cursor int64) error {
	for _, projectUID := range projectUIDs {
		_, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2)
 ON CONFLICT(key) DO UPDATE SET value=excluded.value
 WHERE CAST(meta.value AS BIGINT)<CAST(excluded.value AS BIGINT)`, ProjectStreamResetMetadataPrefix+projectUID, strconv.FormatInt(cursor, 10))
		if err != nil {
			return err
		}
	}
	return nil
}

// ProjectStreamResetAfter also serves ordinary project polls and streams.
// The reset contains only a cursor; project policy filters the metadata lookup.
func ProjectStreamResetAfter(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, afterID, projectID int64) (int64, error) {
	args := []any{ProjectStreamResetMetadataPrefix, afterID}
	// Guard the cast itself because PostgreSQL can evaluate WHERE expressions
	// before eliminating unrelated meta rows whose values contain JSON.
	value := `CASE WHEN m.key=$1||p.uid THEN CAST(m.value AS BIGINT) END`
	statement := `SELECT COALESCE(MAX(` + value + `),0) FROM meta m JOIN projects p ON m.key=$1||p.uid WHERE ` + value + `>$2`
	if projectID != 0 {
		args = append(args, projectID)
		statement += ` AND p.id=$3`
	}
	statement += " AND " + AuthorizedProjectPredicate(ctx, "p.uid", func(value any) string {
		args = append(args, value)
		return "$" + strconv.Itoa(len(args))
	})
	var cursor int64
	err := query.QueryRowContext(ctx, statement, args...).Scan(&cursor)
	return cursor, err
}

// ProjectStreamResetProjectIDs captures the exact streams invalidated by this
// transaction's unique reserved cursor, before post-commit publication can fail.
// These internal project IDs are not part of the portable purge audit record.
func ProjectStreamResetProjectIDs(ctx context.Context, query projectStreamResetQuery, projectID, cursor int64) ([]int64, error) {
	rows, err := query.QueryContext(ctx, `SELECT p.id FROM projects p JOIN meta m ON m.key=$1||p.uid
 WHERE CASE WHEN m.key=$1||p.uid THEN CAST(m.value AS BIGINT) END=$2`, ProjectStreamResetMetadataPrefix, cursor)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	projects := []int64{projectID}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if id != projectID {
			projects = append(projects, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.Sort(projects)
	return projects, nil
}
