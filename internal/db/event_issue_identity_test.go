package db

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoteEventIssueUIDReadsOnlyTheHeader(t *testing.T) {
	const issue = "01HZZZZZZZZZZZZZZZZZZZZZZA"
	const other = "01HZZZZZZZZZZZZZZZZZZZZZZB"
	header := func(uid string) *string { return &uid }

	accepted := []struct {
		name    string
		kind    string
		header  *string
		payload string
		want    string
	}{
		{name: "creation repeats the header as uid", kind: "issue.created", header: header(issue), payload: `{"uid":"` + issue + `"}`, want: issue},
		{name: "snapshot repeats the header as uid", kind: "issue.snapshot", header: header(issue), payload: `{"uid":"` + issue + `"}`, want: issue},
		{name: "label change repeats the header as issue_uid", kind: "issue.labeled", header: header(issue), payload: `{"issue_uid":"` + issue + `","label":"bug"}`, want: issue},
		{name: "comment carries no payload identity", kind: "issue.commented", header: header(issue), payload: `{"comment_uid":"` + other + `"}`, want: issue},
		{name: "project event names no issue", kind: "project.metadata_updated", payload: `{"diff":{}}`, want: ""},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RemoteEventIssueUID(RemoteEvent{Type: tc.kind, IssueUID: tc.header, Payload: jsontext.Value(tc.payload)})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	rejected := []struct {
		name    string
		kind    string
		header  *string
		payload string
	}{
		{name: "creation names its issue only in the payload", kind: "issue.created", payload: `{"uid":"` + issue + `"}`},
		{name: "label change names its issue only in the payload", kind: "issue.labeled", payload: `{"issue_uid":"` + issue + `"}`},
		{name: "issue event names no issue", kind: "issue.updated", payload: `{"body":"text"}`},
		{name: "issue event has an empty header", kind: "issue.updated", header: header(""), payload: `{"body":"text"}`},
		{name: "payload uid differs from the header", kind: "issue.snapshot", header: header(issue), payload: `{"uid":"` + other + `"}`},
		{name: "payload issue_uid differs from the header", kind: "issue.unlabeled", header: header(issue), payload: `{"issue_uid":"` + other + `"}`},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RemoteEventIssueUID(RemoteEvent{Type: tc.kind, IssueUID: tc.header, Payload: jsontext.Value(tc.payload)})
			require.ErrorIs(t, err, ErrFederationIngestValidation)
		})
	}
}

func TestEventIssueUIDIsTheHeaderValue(t *testing.T) {
	uid := "01HZZZZZZZZZZZZZZZZZZZZZZA"
	assert.Equal(t, uid, EventIssueUID(&uid))
	assert.Empty(t, EventIssueUID(nil))
}

func TestPrepareReplayEventRequiresHeaderIdentityForIssueEvents(t *testing.T) {
	const project = "01HZZZZZZZZZZZZZZZZZZZZZZP"
	issue := "01HZZZZZZZZZZZZZZZZZZZZZZA"
	replay := func(kind string, header *string, payload string) error {
		return PrepareReplayEvent(&EventExport{
			ID: 7, UID: "01HZZZZZZZZZZZZZZZZZZZZZZE", OriginInstanceUID: "01HZZZZZZZZZZZZZZZZZZZZZZ1",
			ProjectName: "example-project", IssueUID: header, Type: kind, Actor: "worker",
			Payload: jsontext.Value(payload), HLCPhysicalMS: 1, CreatedAt: "2026-05-23T12:00:00.000Z",
		}, project, "example-project", true)
	}

	require.NoError(t, replay("issue.updated", &issue, `{"title":"Edit"}`))
	require.NoError(t, replay("project.metadata_updated", nil, `{"diff":{}}`))

	err := replay("issue.updated", nil, `{"issue_uid":"`+issue+`","title":"Edit"}`)
	require.ErrorContains(t, err, "event 7 (issue.updated) has no issue_uid")
	err = replay("issue.created", nil, `{"uid":"`+issue+`"}`)
	require.ErrorContains(t, err, "event 7 (issue.created) has no issue_uid")
}
