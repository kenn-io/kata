package db

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRelayCommentReplyRelease(t *testing.T) {
	const commentUID = "00000000000000000000000002"
	event := RemoteEvent{ProjectUID: "own", Type: "issue.commented", Payload: jsontext.Value(`{"reply_to_uid":"` + commentUID + `","reply_kind":"reply"}`)}
	lookup := func(known bool) *EventIssueLookup {
		return NewEventIssueLookup(nil, nil, func(uid string) (Issue, error) {
			require.Equal(t, commentUID, uid)
			if known {
				return Issue{ProjectUID: "peer"}, nil
			}
			return Issue{}, ErrNotFound
		})
	}
	denied := errors.New("peer denied")
	ctx := WithProjectAccessTransactionCheck(t.Context(), func(_ context.Context, _ Transaction, projectUID string) error {
		require.Equal(t, "peer", projectUID)
		return denied
	})
	require.ErrorIs(t, AuthorizeRelayRelease(ctx, nil, lookup(true), "own", []RemoteEvent{event}), denied)
	require.ErrorIs(t, AuthorizeRelayRelease(t.Context(), nil, lookup(false), "own", []RemoteEvent{event}), ErrRelayReferenceUnprovable)
	allowed := WithProjectAccessTransactionCheck(t.Context(), func(_ context.Context, _ Transaction, projectUID string) error {
		require.Equal(t, "peer", projectUID)
		return nil
	})
	require.NoError(t, AuthorizeRelayRelease(allowed, nil, lookup(true), "own", []RemoteEvent{event}))
}

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
	}, nil, func(uid string) (Issue, error) {
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
		{"linked payload", "issue.linked", `{"from_uid":"` + issueUID + `","to_uid":"` + issueUID + `"}`, nil, true, false},
		{"unlinked payload", "issue.unlinked", `{"from_issue_uid":"` + issueUID + `","to_issue_uid":"` + issueUID + `"}`, nil, true, false},
		{"links changed", "issue.links_changed", `{"blocks_added_uids":["` + issueUID + `"]}`, nil, true, false},
		{"created", "issue.created", `{"links":[{"to_issue_uid":"` + issueUID + `"}]}`, nil, true, false},
		{"snapshot", "issue.snapshot", `{"comments":[{"reply_to_uid":"` + commentUID + `","reply_kind":"reply"}]}`, nil, true, false},
		{"restated", RepeatedImportEventType, `{"links":[{"to_issue_uid":"` + issueUID + `"}]}`, nil, true, false},
		{"restated notification", RepeatedImportEventType, `{"metadata":{"notify.recipient":{"re":"` + commentUID + `"}}}`, nil, true, false},
		{"moved snapshot", "issue.moved", `{"from_project_uid":"own","to_project_uid":"own","snapshot":{"links":[{"to_issue_uid":"` + issueUID + `"}]}}`, nil, true, false},
		{"close parent", "issue.closed", `{"parent_uid":"` + issueUID + `"}`, nil, true, false},
		{"qualified close evidence", "issue.closed", `{"evidence":[{"type":"duplicate-of","issue_ref":"peer#1"}]}`, nil, false, true},
		{"close UID evidence", "issue.closed", `{"evidence":[{"type":"duplicate-of","issue_ref":"` + issueUID + `"}]}`, nil, true, false},
		{"missing qualified evidence", "issue.closed", `{"evidence":[{"type":"duplicate-of","issue_ref":"other#1234"}]}`, nil, false, true},
		{"missing UID evidence", "issue.closed", `{"evidence":[{"type":"duplicate-of","issue_ref":"00000000000000000000000003"}]}`, nil, false, true},
		{"purged issue", "issue.closed", `{"evidence":[{"type":"duplicate-of","issue_ref":"gone"}]}`, nil, false, false},
		{"short parent", "issue.closed", `{"parent_short_id":"1234"}`, nil, false, false},
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
		{"missing link payload", RemoteEvent{Type: "issue.linked", Payload: jsontext.Value(`{"from_uid":"` + issueUID + `","to_uid":"` + missing + `"}`)}},
		{"missing unlink payload", RemoteEvent{Type: "issue.unlinked", Payload: jsontext.Value(`{"from_issue_uid":"` + missing + `","to_issue_uid":"` + issueUID + `"}`)}},
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
