package sqlitestore

import (
	"context"
	"iter"

	"go.kenn.io/kata/internal/db"
)

// ExportMCPEventSubscriptions streams subscriptions for a full backup.
func (d *Store) ExportMCPEventSubscriptions(ctx context.Context, filter db.ExportFilter) iter.Seq2[db.MCPEventSubscriptionExport, error] {
	return db.ExportMCPEventSubscriptionsSQL(ctx, d.readQ.QueryContext, filter)
}
