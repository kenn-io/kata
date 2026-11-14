package db

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRelayEventPeerProjects(t *testing.T) {
	const issueUID = "00000000000000000000000001"
	const commentUID = "00000000000000000000000002"
	peer := Issue{ProjectUID: "peer"}
	related := issueUID
	lookup := NewEventIssueLookup(func(uid string) (Issue, error) {
		if uid == issueUID {
			return peer, nil
		}
		return Issue{}, ErrNotFound
	}, func(_, ref string) (Issue, error) {
		if ref == "peer#1" {
			return peer, nil
		}
		return Issue{}, ErrNotFound
	}, func(uid string) (Issue, error) {
		if uid == commentUID {
			return peer, nil
		}
		return Issue{}, ErrNotFound
	})
	cases := []struct {
		name, kind, payload string
		related             *string
		peer, unprovable    bool
	}{
		{"linked", "issue.linked", `{}`, &related, true, false},
		{"links changed", "issue.links_changed", `{"blocks_added_uids":["` + issueUID + `"]}`, nil, true, false},
		{"created", "issue.created", `{"links":[{"to_issue_uid":"` + issueUID + `"}]}`, nil, true, false},
		{"snapshot", "issue.snapshot", `{"comments":[{"reply_to_uid":"` + commentUID + `","reply_kind":"reply"}]}`, nil, true, false},
		{"restated", RepeatedImportEventType, `{"links":[{"to_issue_uid":"` + issueUID + `"}]}`, nil, true, false},
		{"moved snapshot", "issue.moved", `{"from_project_uid":"own","to_project_uid":"own","snapshot":{"links":[{"to_issue_uid":"` + issueUID + `"}]}}`, nil, true, false},
		{"close parent", "issue.closed", `{"parent_uid":"` + issueUID + `"}`, nil, true, false},
		{"close evidence", "issue.closed", `{"evidence":[{"type":"duplicate-of","issue_ref":"peer#1"}]}`, nil, true, false},
		{"purged issue", "issue.closed", `{"evidence":[{"type":"duplicate-of","issue_ref":"gone"}]}`, nil, false, false},
		{"short parent", "issue.closed", `{"parent_short_id":"peer#1"}`, nil, false, true},
		{"metadata notification", "issue.created", `{"metadata":{"notify.recipient":{"re":"` + commentUID + `"}}}`, nil, true, false},
		{"diff notification", "issue.metadata_updated", `{"diff":{"notify.recipient":{"from":{"re":"` + commentUID + `"},"to":{"re":"` + commentUID + `"}}}}`, nil, true, false},
		{"deleted comment", "issue.created", `{"metadata":{"notify.recipient":{"re":"00000000000000000000000003"}}}`, nil, false, true},
	}
	t.Run("subject moved to peer", func(t *testing.T) {
		projects, err := RelayEventPeerProjects(RemoteEvent{ProjectUID: "own", Type: "issue.updated", IssueUID: &related, Payload: jsontext.Value(`{}`)}, lookup)
		require.NoError(t, err)
		require.Contains(t, projects, "peer")
	})
	missing := "00000000000000000000000003"
	for _, tc := range []struct {
		name  string
		event RemoteEvent
	}{
		{"missing subject", RemoteEvent{Type: "issue.updated", IssueUID: &missing, Payload: jsontext.Value(`{}`)}},
		{"missing related", RemoteEvent{Type: "issue.linked", RelatedIssueUID: &missing, Payload: jsontext.Value(`{}`)}},
		{"missing link diff", RemoteEvent{Type: "issue.links_changed", Payload: jsontext.Value(`{"blocks_added_uids":["` + missing + `"]}`)}},
		{"missing snapshot link", RemoteEvent{Type: "issue.snapshot", Payload: jsontext.Value(`{"links":[{"to_issue_uid":"` + missing + `"}]}`)}},
		{"missing close parent", RemoteEvent{Type: "issue.closed", Payload: jsontext.Value(`{"parent_uid":"` + missing + `"}`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RelayEventPeerProjects(tc.event, lookup)
			require.ErrorIs(t, err, ErrRelayReferenceUnprovable)
		})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			projects, err := RelayEventPeerProjects(RemoteEvent{ProjectUID: "own", Type: tc.kind, Payload: jsontext.Value(tc.payload), RelatedIssueUID: tc.related}, lookup)
			if tc.unprovable {
				require.ErrorIs(t, err, ErrRelayReferenceUnprovable)
				return
			}
			require.NoError(t, err)
			if tc.peer {
				require.Contains(t, projects, "peer")
			} else {
				require.NotContains(t, projects, "peer")
			}
		})
	}
}
