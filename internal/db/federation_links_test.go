package db_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/kata/internal/db"
)

// The event types here are the complete set handled by FoldProjection.apply.
// Keeping this exhaustive means a new fold case cannot be added without a
// deliberate decision about whether it affects link reconciliation.
func TestFederationEventAffectsLinks(t *testing.T) {
	affects := []string{
		"issue.created",
		"issue.snapshot",
		"issue.linked",
		"issue.unlinked",
		"issue.links_changed",
		"issue.moved",
		"issue.soft_deleted",
		"issue.restored",
		"project.author_rewritten",
	}
	for _, eventType := range affects {
		t.Run(eventType+" affects links", func(t *testing.T) {
			assert.True(t, db.FederationEventAffectsLinks(eventType))
		})
	}

	unaffected := []string{
		"issue.updated",
		"issue.assigned",
		"issue.unassigned",
		"issue.priority_set",
		"issue.priority_cleared",
		"issue.closed",
		"issue.reopened",
		"issue.commented",
		"issue.comment_edited",
		"issue.labeled",
		"issue.unlabeled",
		"issue.metadata_updated",
		"project.metadata_updated",
		"project.federation_enabled",
		"claim.acquired",
		"claim.released",
		"claim.expired",
		"claim.force_released",
		"claim.violated",
	}
	for _, eventType := range unaffected {
		t.Run(eventType+" does not affect links", func(t *testing.T) {
			assert.False(t, db.FederationEventAffectsLinks(eventType))
		})
	}
}
