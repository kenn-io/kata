package db

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRelayEventReferencedProjects(t *testing.T) {
	own := Issue{ProjectID: 1, UID: "own", ShortID: "own"}
	peer := Issue{ProjectID: 2, UID: "peer", ShortID: "peer"}
	for _, test := range []struct {
		name, kind, payload string
		refs                []string
		want                []int64
		bad                 bool
	}{
		{name: "native endpoints", kind: "issue.linked", payload: `{}`, refs: []string{"peer"}, want: []int64{1, 2}},
		{name: "parent UID", kind: "issue.closed", payload: `{"parent_uid":"peer","parent_short_id":"peer"}`, want: []int64{2, 1}},
		{name: "parent short ID", kind: "issue.closed", payload: `{"parent_short_id":"peer"}`, want: []int64{2, 1}},
		{name: "UID evidence", kind: "issue.closed", payload: `{"evidence":[{"type":"duplicate-of","issue_ref":"peer"}]}`, want: []int64{2, 1}},
		{name: "qualified evidence", kind: "issue.closed", payload: `{"evidence":[{"type":"superseded-by","issue_ref":"peer-project#peer"}]}`, want: []int64{2, 1}},
		{name: "reply", kind: "issue.commented", payload: `{"reply_to_uid":"comment"}`, want: []int64{1, 2}},
		{name: "snapshot replies", kind: "issue.snapshot", payload: `{"comments":[{"reply_to_uid":"comment"}]}`, want: []int64{1, 2}},
		{name: "ordinary text", kind: "issue.updated", payload: `{"body":"peer"}`, want: []int64{1}},
		{name: "unresolved endpoint", kind: "issue.linked", payload: `{}`, refs: []string{"missing"}, bad: true},
		{name: "malformed parent", kind: "issue.closed", payload: `{"parent_uid":42}`, bad: true},
		{name: "null parent", kind: "issue.closed", payload: `{"parent_uid":null}`, bad: true},
		{name: "ambiguous parent", kind: "issue.closed", payload: `{"parent_short_id":"missing"}`, bad: true},
		{name: "renamed parent display ID", kind: "issue.closed", payload: `{"parent_uid":"peer","parent_short_id":"other"}`, want: []int64{2, 1}},
		{name: "malformed evidence", kind: "issue.closed", payload: `{"evidence":{}}`, bad: true},
		{name: "null evidence", kind: "issue.closed", payload: `{"evidence":null}`, bad: true},
		{name: "unresolved evidence", kind: "issue.closed", payload: `{"evidence":[{"type":"duplicate-of","issue_ref":"missing"}]}`, bad: true},
		{name: "unresolved reply", kind: "issue.commented", payload: `{"reply_to_uid":"missing"}`, bad: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			byUID := func(ref string) (Issue, error) {
				switch ref {
				case "own":
					return own, nil
				case "peer", "peer-project#peer":
					return peer, nil
				}
				return Issue{}, ErrNotFound
			}
			lookup := NewEventIssueLookup(byUID, func(_, ref string) (Issue, error) { return byUID(ref) })
			comment := func(_ string, ref string) (Issue, error) {
				if ref == "comment" {
					return peer, nil
				}
				return Issue{}, ErrNotFound
			}
			got, err := RelayEventReferencedProjects(RemoteEvent{Type: test.kind, IssueUID: &own.UID, Payload: jsontext.Value(test.payload)}, test.refs, lookup, comment, byUID)
			if test.bad {
				require.ErrorIs(t, err, ErrNotFound)
				return
			}
			require.NoError(t, err)
			require.ElementsMatch(t, test.want, got)
		})
	}
}
