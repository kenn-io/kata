package pgstore

import (
	"context"

	"go.kenn.io/kata/internal/db"
)

// A hop ACK is delivery, not root acceptance. Retained source intent survives
// compaction; an exact validated root receipt is what clears negotiated pending
// state. A compacted source has no backend-local event high-water ID to report.
func (d *Store) pendingRelayRootStats(ctx context.Context, projectID int64, originInstanceUID string) (count, highWater int64, negotiated bool, err error) {
	args := []any{projectID, db.RelayStreamEvent, originInstanceUID}
	query := `WITH configured AS (
 SELECT relay_config FROM federation_bindings WHERE project_id=$1 AND role='spoke' AND relay_config IS NOT NULL
)
SELECT (SELECT COUNT(*) FROM configured),COUNT(o.id),COALESCE(MAX(e.id),0)
FROM configured c
JOIN federation_relay_outbox o ON o.binding_uid=c.relay_config::jsonb->>'binding_uid' AND o.reset_epoch=(c.relay_config::jsonb->>'reset_epoch')::BIGINT AND o.stream=$2
JOIN projects p ON p.uid=o.project_uid
LEFT JOIN events e ON e.uid=o.source_uid AND e.project_id=p.id
LEFT JOIN federation_event_provenance r ON r.project_uid=o.project_uid AND r.event_uid=o.source_uid AND r.content_hash=o.source_hash
WHERE o.envelope::jsonb->'path'->>0=$3 AND p.id=$1 AND r.event_uid IS NULL AND ` + authorizedProjectPredicate(ctx, "p.uid", &args)
	var configured int64
	err = d.QueryRowContext(ctx, query, args...).Scan(&configured, &count, &highWater)
	if err != nil {
		return 0, 0, false, err
	}
	return count, highWater, configured > 0, nil
}
