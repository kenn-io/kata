package db_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/kata/internal/db"
)

func uidPtr(value string) *string { return &value }

// The peer carried inside a snapshot payload is the case that indexed columns
// cannot see, and the one an adoption baseline depends on.
func TestFederationEventLinkEndpointUIDsReadsSnapshotPayloadLinks(t *testing.T) {
	event := db.RemoteEvent{
		Type:     "issue.snapshot",
		IssueUID: uidPtr("01HZNQ7VFPK1XGD8R5MABCD4EA"),
		Payload: json.RawMessage(`{"uid":"01HZNQ7VFPK1XGD8R5MABCD4EA","title":"t",` +
			`"links":[{"type":"blocks","to_issue_uid":"01HZNQ7VFPK1XGD8R5MABCD4EB"},` +
			`{"type":"parent","to_issue_uid":"01HZNQ7VFPK1XGD8R5MABCD4EC","incoming":true}]}`),
	}

	assert.Equal(t, []string{
		"01HZNQ7VFPK1XGD8R5MABCD4EA",
		"01HZNQ7VFPK1XGD8R5MABCD4EB",
		"01HZNQ7VFPK1XGD8R5MABCD4EC",
	}, db.FederationEventLinkEndpointUIDs(event))
}

func TestFederationEventLinkEndpointUIDsReadsLinkEventPeers(t *testing.T) {
	event := db.RemoteEvent{
		Type:            "issue.linked",
		IssueUID:        uidPtr("01HZNQ7VFPK1XGD8R5MABCD4EA"),
		RelatedIssueUID: uidPtr("01HZNQ7VFPK1XGD8R5MABCD4EB"),
		Payload: json.RawMessage(`{"issue_uid":"01HZNQ7VFPK1XGD8R5MABCD4EA",` +
			`"from_uid":"01HZNQ7VFPK1XGD8R5MABCD4EA","to_uid":"01HZNQ7VFPK1XGD8R5MABCD4EB","type":"blocks"}`),
	}

	assert.Equal(t, []string{
		"01HZNQ7VFPK1XGD8R5MABCD4EA",
		"01HZNQ7VFPK1XGD8R5MABCD4EB",
	}, db.FederationEventLinkEndpointUIDs(event))
}

// links_changed names peers only in per-relation lists, so every list has to be
// read or a newly linked peer project falls outside the reconciled scope.
func TestFederationEventLinkEndpointUIDsReadsEveryLinksChangedList(t *testing.T) {
	event := db.RemoteEvent{
		Type:     "issue.links_changed",
		IssueUID: uidPtr("01HZNQ7VFPK1XGD8R5MABCD4EA"),
		Payload: json.RawMessage(`{"issue_uid":"01HZNQ7VFPK1XGD8R5MABCD4EA",` +
			`"parent_set_uid":"01HZNQ7VFPK1XGD8R5MABCD4EB",` +
			`"parent_removed_uid":"01HZNQ7VFPK1XGD8R5MABCD4EC",` +
			`"blocks_added_uids":["01HZNQ7VFPK1XGD8R5MABCD4ED"],` +
			`"blocks_removed_uids":["01HZNQ7VFPK1XGD8R5MABCD4EE"],` +
			`"blocked_by_added_uids":["01HZNQ7VFPK1XGD8R5MABCD4EF"],` +
			`"blocked_by_removed_uids":["01HZNQ7VFPK1XGD8R5MABCD4EG"],` +
			`"related_added_uids":["01HZNQ7VFPK1XGD8R5MABCD4EH"],` +
			`"related_removed_uids":["01HZNQ7VFPK1XGD8R5MABCD4EJ"]}`),
	}

	assert.Equal(t, []string{
		"01HZNQ7VFPK1XGD8R5MABCD4EA",
		"01HZNQ7VFPK1XGD8R5MABCD4EB",
		"01HZNQ7VFPK1XGD8R5MABCD4EC",
		"01HZNQ7VFPK1XGD8R5MABCD4ED",
		"01HZNQ7VFPK1XGD8R5MABCD4EE",
		"01HZNQ7VFPK1XGD8R5MABCD4EF",
		"01HZNQ7VFPK1XGD8R5MABCD4EG",
		"01HZNQ7VFPK1XGD8R5MABCD4EH",
		"01HZNQ7VFPK1XGD8R5MABCD4EJ",
	}, db.FederationEventLinkEndpointUIDs(event))
}

func TestFederationEventLinkEndpointUIDsIgnoresNonLinkEvents(t *testing.T) {
	event := db.RemoteEvent{
		Type:     "issue.commented",
		IssueUID: uidPtr("01HZNQ7VFPK1XGD8R5MABCD4EA"),
		Payload:  json.RawMessage(`{"issue_uid":"01HZNQ7VFPK1XGD8R5MABCD4EA","body":"note"}`),
	}

	assert.Nil(t, db.FederationEventLinkEndpointUIDs(event))
}
