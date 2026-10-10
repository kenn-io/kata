package sqlitestore

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestFederationReplyCanPrecedeRelatedIssue(t *testing.T) {
	source, target := "01AAAAAAAAAAAAAAAAAAAAAAAA", "01BBBBBBBBBBBBBBBBBBBBBBBB"
	event := db.RemoteEvent{ProjectUID: "project", OriginInstanceUID: "peer", EventUID: "event", Actor: "worker", HLCPhysicalMS: 1, Type: "issue.commented", IssueUID: &source, RelatedIssueUID: &target, Payload: jsontext.Value(`{"comment_uid":"01CCCCCCCCCCCCCCCCCCCCCCCC","reply_to_uid":"01DDDDDDDDDDDDDDDDDDDDDDDD","reply_kind":"reply"}`)}
	require.NoError(t, validateFederationProjectEvent("project", "peer", event, map[string]struct{}{source: {}}))
	event.Payload = jsontext.Value(`{"comment_uid":"01CCCCCCCCCCCCCCCCCCCCCCCC"}`)
	require.ErrorIs(t, validateFederationProjectEvent("project", "peer", event, map[string]struct{}{source: {}}), db.ErrFederationIngestValidation)
}
