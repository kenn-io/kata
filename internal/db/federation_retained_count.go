package db

import (
	"context"
	"database/sql"
)

// CountRetainedFederationEvents counts supported retained events in one query.
func CountRetainedFederationEvents(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, instanceUID string, binding FederationBinding) (int64, error) {
	var count int64
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE project_id=$1 AND id>0 AND id<=$2 AND type IN (`+FederationPushEventTypesSQL()+`) AND `+LocalInstanceOriginSQL("origin_instance_uid", "$3")+` AND (origin_instance_uid<>$3 OR (id<=$4 AND actor<>$5))`, binding.ProjectID, binding.RetainedThroughEventID, instanceUID, binding.PushCursorEventID, binding.Actor).Scan(&count)
	return count, err
}
