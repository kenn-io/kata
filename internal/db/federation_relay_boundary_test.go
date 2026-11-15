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
		if ref == issueUID {
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
		peer                bool
	}{
		{"linked", "issue.linked", `{}`, &related, true},
		{"linked payload", "issue.linked", `{"from_uid":"` + issueUID + `","to_uid":"` + issueUID + `"}`, nil, true},
		{"unlinked payload", "issue.unlinked", `{"from_issue_uid":"` + issueUID + `","to_issue_uid":"` + issueUID + `"}`, nil, true},
		{"links changed", "issue.links_changed", `{"blocks_added_uids":["` + issueUID + `"]}`, nil, true},
		{"created", "issue.created", `{"links":[{"to_issue_uid":"` + issueUID + `"}]}`, nil, true},
		{"comment reply", "issue.commented", `{"reply_to_uid":"` + commentUID + `","reply_kind":"reply"}`, nil, true},
		{"missing comment reply", "issue.commented", `{"reply_to_uid":"00000000000000000000000003","reply_kind":"reply"}`, nil, false},
		{"missing close parent", "issue.closed", `{"parent_uid":"00000000000000000000000003"}`, nil, false},
		{"snapshot", "issue.snapshot", `{"comments":[{"reply_to_uid":"` + commentUID + `","reply_kind":"reply"}]}`, nil, true},
		{"restated", RepeatedImportEventType, `{"links":[{"to_issue_uid":"` + issueUID + `"}]}`, nil, true},
		{"restated notification", RepeatedImportEventType, `{"metadata":{"notify.recipient":{"re":"` + commentUID + `"}}}`, nil, true},
		{"moved snapshot", "issue.moved", `{"from_project_uid":"own","to_project_uid":"own","snapshot":{"links":[{"to_issue_uid":"` + issueUID + `"}]}}`, nil, true},
		{"close parent", "issue.closed", `{"parent_uid":"` + issueUID + `"}`, nil, true},
		{"close UID evidence", "issue.closed", `{"evidence":[{"type":"duplicate-of","issue_ref":"` + issueUID + `"}]}`, nil, true},
		{"missing UID evidence", "issue.closed", `{"evidence":[{"type":"duplicate-of","issue_ref":"00000000000000000000000003"}]}`, nil, false},
		{"short parent", "issue.closed", `{"parent_short_id":"1234"}`, nil, false},
		{"metadata notification", "issue.created", `{"metadata":{"notify.recipient":{"re":"` + commentUID + `"}}}`, nil, true},
		{"diff notification", "issue.metadata_updated", `{"diff":{"notify.recipient":{"from":{"re":"` + commentUID + `"},"to":{"re":"` + commentUID + `"}}}}`, nil, true},
		{"deleted comment", "issue.created", `{"metadata":{"notify.recipient":{"re":"00000000000000000000000003"}}}`, nil, false},
	}
	t.Run("subject moved to peer", func(t *testing.T) {
		projects, err := RelayEventPeerProjects(RemoteEvent{ProjectUID: "own", Type: "issue.updated", IssueUID: &related, Payload: jsontext.Value(`{}`)}, lookup)
		require.NoError(t, err)
		require.Contains(t, projects, "peer")
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			projects, err := RelayEventPeerProjects(RemoteEvent{ProjectUID: "own", Type: tc.kind, Payload: jsontext.Value(tc.payload), RelatedIssueUID: tc.related}, lookup)
			require.NoError(t, err)
			if tc.peer {
				require.Contains(t, projects, "peer")
			} else {
				require.NotContains(t, projects, "peer")
			}
		})
	}
}
