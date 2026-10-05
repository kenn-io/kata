package sqlitestore

import (
	"context"
	"database/sql"

	"go.kenn.io/kata/internal/db"
)

// Restore creates a new offer generation, preserving every previously emitted
// or acknowledged envelope. Portable bytes and the exact artifact digest stay
// unchanged; recipients still request vectors only for actual durable misses.
func (d *Store) queueRestoredArtifactsTx(ctx context.Context, tx *sql.Tx, event db.Event) error {
	if db.RelayStateCapture(ctx) {
		return nil
	}
	after := ""
	for {
		rows, err := tx.QueryContext(ctx, `SELECT digest,manifest FROM federation_embedding_artifacts WHERE project_uid=? AND issue_uid=? AND staging_expires_at IS NULL AND digest>? ORDER BY digest LIMIT 32`, event.ProjectUID, *event.IssueUID, after)
		if err != nil {
			return err
		}
		type offer struct{ digest, manifest string }
		page := make([]offer, 0, 32)
		for rows.Next() {
			var item offer
			if err = rows.Scan(&item.digest, &item.manifest); err != nil {
				break
			}
			page = append(page, item)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, item := range page {
			offerCtx := db.WithRelayArtifactSourceUID(db.WithRelayForwardPath(ctx, nil), item.digest+":"+event.UID)
			if err := queueRelayBodyTx(offerCtx, tx, event.ProjectID, event.ProjectUID, db.RelayStreamArtifact, item.digest, item.digest, []byte(item.manifest), d.instanceUID); err != nil {
				return err
			}
		}
		after = page[len(page)-1].digest
	}
}
