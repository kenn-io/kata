package db_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
		"issue.external_root_bound",
		"issue.external_root_paused",
		"issue.external_root_resumed",
		"issue.external_root_unbound",
		"issue.external_comment_resolved",
		"issue.external_field_conflicted",
		"issue.external_field_resolved",
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

// Link reconciliation reads only FoldProjection.Links, and the group fold is
// restricted to FederationLinkAffectingEventTypes for cost. That restriction is
// only sound if it cannot change Links, so assert the two folds agree over a
// stream that mixes link-bearing and unrelated events.
func TestFoldingOnlyLinkAffectingEventsPreservesLinks(t *testing.T) {
	const projectUID = "01HZNQ7VFPK1XGD8R5MABCD4EY"
	child := "01HZNQ7VFPK1XGD8R5MABCD4EA"
	parent := "01HZNQ7VFPK1XGD8R5MABCD4EB"
	peer := "01HZNQ7VFPK1XGD8R5MABCD4EC"

	event := func(clock int64, eventType, issueUID, relatedUID, payload string) db.FoldEvent {
		return db.FoldEvent{
			UID:               "01HZNQ7VFPK1XGD8R5MABCD" + string(rune('A'+clock%26)) + "Z",
			OriginInstanceUID: projectUID,
			ProjectUID:        projectUID,
			IssueUID:          issueUID,
			RelatedIssueUID:   relatedUID,
			Type:              eventType,
			Actor:             "tester",
			HLCPhysicalMS:     clock,
			CreatedAt:         "2026-05-23T12:00:00.000Z",
			Payload:           json.RawMessage(payload),
		}
	}

	all := []db.FoldEvent{
		event(1, "issue.created", child, "",
			`{"uid":"`+child+`","title":"child","author":"tester","status":"open",`+
				`"links":[{"from_uid":"`+child+`","to_uid":"`+peer+`","type":"blocks","author":"tester"}]}`),
		event(2, "issue.created", parent, "", `{"uid":"`+parent+`","title":"parent","author":"tester","status":"open"}`),
		event(3, "issue.updated", child, "", `{"issue_uid":"`+child+`","diff":{"title":{"to":"renamed"}}}`),
		event(4, "issue.linked", child, parent,
			`{"issue_uid":"`+child+`","from_uid":"`+child+`","to_uid":"`+parent+`","type":"parent"}`),
		event(5, "issue.commented", child, "",
			`{"issue_uid":"`+child+`","comment_uid":"01HZNQ7VFPK1XGD8R5MABCD4ED","author":"tester","body":"note"}`),
		event(6, "issue.labeled", child, "", `{"issue_uid":"`+child+`","label":"bug"}`),
		event(7, "issue.unlinked", child, peer,
			`{"issue_uid":"`+child+`","from_uid":"`+child+`","to_uid":"`+peer+`","type":"blocks"}`),
		event(8, "project.metadata_updated", "", "", `{"diff":{"area":{"from":null,"to":"docs"}}}`),
		event(9, "issue.closed", child, "", `{"issue_uid":"`+child+`"}`),
	}

	var linkAffecting []db.FoldEvent
	for _, ev := range all {
		if db.FederationEventAffectsLinks(ev.Type) {
			linkAffecting = append(linkAffecting, ev)
		}
	}
	require.Less(t, len(linkAffecting), len(all), "stream must include events that are filtered out")

	full := db.FoldEvents(all)
	restricted := db.FoldEvents(linkAffecting)

	require.NotEmpty(t, full.Links, "stream must produce link state to compare")
	assert.Equal(t, full.Links, restricted.Links)
}
