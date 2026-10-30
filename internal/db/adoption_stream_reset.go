package db

import (
	"context"
	"slices"
)

// AdoptionFormerProjects captures streams that lose move departures
// when adoption replaces this project's history with portable snapshots.
func AdoptionFormerProjects(ctx context.Context, tx projectStreamResetQuery, project Project) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT payload FROM events WHERE project_id=$1 AND type='issue.moved'`, project.ID)
	if err != nil {
		return nil, err
	}
	projects := map[string]bool{}
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			_ = rows.Close()
			return nil, err
		}
		values := PayloadMap([]byte(payload))
		for _, key := range []string{"from_project_uid", "to_project_uid"} {
			projectUID, _ := StringValue(values[key])
			if projectUID != "" && projectUID != project.UID {
				projects[projectUID] = true
			}
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
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
