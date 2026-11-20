package db

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/notification"
)

func TestRelayEventPeerProjects(t *testing.T) {
	const issueUID = "00000000000000000000000001"
	const commentUID = "00000000000000000000000002"
	const ownIssueUID = "00000000000000000000000005"
	const otherIssueUID = "00000000000000000000000004"
	peer := Issue{ProjectUID: "peer"}
	related := issueUID
	ownIssue := ownIssueUID
	lookup := NewEventIssueLookup(func(uid string) (Issue, error) {
		if uid == issueUID {
			return peer, nil
		}
		if uid == ownIssueUID {
			return Issue{ProjectUID: "own"}, nil
		}
		if uid == otherIssueUID {
			return Issue{ProjectUID: "other"}, nil
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
		related, issue      *string
		want                map[string]bool
	}{
		{"comment related issue", "issue.commented", `{"comment_uid":"` + commentUID + `","reply_to_uid":"00000000000000000000000003","reply_kind":"reply"}`, &related, nil, map[string]bool{"peer": false}},
		{"created", "issue.created", `{"links":[{"to_issue_uid":"` + issueUID + `"}]}`, nil, nil, map[string]bool{"peer": true}},
		{"unresolved link", "issue.created", `{"links":[{"to_issue_uid":"00000000000000000000000003"}]}`, nil, nil, map[string]bool{"": true}},
		{"comment reply", "issue.commented", `{"reply_to_uid":"` + commentUID + `","reply_kind":"reply"}`, nil, nil, map[string]bool{"peer": false}},
		{"missing comment reply", "issue.commented", `{"reply_to_uid":"00000000000000000000000003","reply_kind":"reply"}`, nil, nil, map[string]bool{}},
		{"metadata notification", "issue.created", `{"metadata":{"` + notification.MetadataKey("recipient") + `":{"re":"` + commentUID + `"}}}`, nil, nil, map[string]bool{"peer": false}},
		{"moved snapshot notification", "issue.moved", `{"snapshot":{"metadata":{"` + notification.MetadataKey("recipient") + `":{"re":"` + commentUID + `"}}}}`, nil, nil, map[string]bool{"peer": false}},
		{"link added", "issue.linked", `{"from_uid":"` + ownIssueUID + `","to_uid":"` + otherIssueUID + `"}`, nil, &ownIssue, map[string]bool{"own": true, "other": true}},
		{"link removed", "issue.unlinked", `{"from_uid":"` + ownIssueUID + `","to_uid":"` + otherIssueUID + `"}`, nil, &ownIssue, map[string]bool{"own": true, "other": true}},
		{"subject belongs to peer", "issue.updated", `{}`, nil, &related, map[string]bool{"peer": false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			projects := map[string]bool{}
			err := RelayEventPeerProjects(RemoteEvent{ProjectUID: "own", Type: tc.kind, Payload: jsontext.Value(tc.payload), RelatedIssueUID: tc.related, IssueUID: tc.issue}, lookup, func(uid string, link bool) error { projects[uid] = link; return nil })
			require.NoError(t, err)
			require.Equal(t, tc.want, projects)
		})
	}
}
