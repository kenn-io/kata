package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRelayEventIsLocalAudit(t *testing.T) {
	for _, eventType := range []string{"project.created", "project.renamed", "project.merged", "project.author_rewritten", "project.federation_enabled", "project.alias_removed", "recurrence.created", "recurrence.updated", "recurrence.deleted", "recurrence.materialized", "recurrence.materialization_skipped", "close.throttled", "claim.acquired", "claim.released", "claim.expired", "claim.force_released", "claim.violated"} {
		require.True(t, RelayEventIsLocalAudit(eventType), eventType)
	}
	for _, eventType := range []string{"issue.created", "issue.restored", "project.metadata_updated", "project.removed", "project.restored", "issue.future_operation", "project.future_operation", "claim.future_operation", "recurrence.future_operation"} {
		require.False(t, RelayEventIsLocalAudit(eventType), eventType)
	}
}
