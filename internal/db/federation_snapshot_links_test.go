package db

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSnapshotLinksFeature(t *testing.T) {
	for _, tc := range []struct {
		kind, payload, want string
		invalid             bool
	}{
		{"issue.snapshot", `{"uid":"a"}`, "", false},
		{"issue.snapshot", `{"uid":"a","links_scope":"all"}`, SnapshotLinksEventFeature, false},
		{"project.federation_enabled", `{"links_scope":"all"}`, SnapshotLinksEventFeature, false},
		{"issue.moved", `{"snapshot":{"links_scope":"all"}}`, MergeEventFeatures(IssueMoveEventFeature, SnapshotLinksEventFeature), false},
		{"issue.snapshot", `{"links_scope":null}`, "", true},
		{"issue.snapshot", `{"links_scope":"partial"}`, "", true},
		{"issue.snapshot", `{"links_scope":1}`, "", true},
		{"issue.created", `{"links_scope":"all"}`, "", true},
	} {
		t.Run(tc.kind+tc.payload, func(t *testing.T) {
			got, err := PayloadRequiredFeatures(tc.kind, tc.payload)
			if tc.invalid {
				require.ErrorIs(t, err, ErrFederationIngestValidation)
				require.Error(t, ValidateFederationEntries(tc.kind, "event", jsontext.Value(tc.payload)))
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
	for _, tc := range []struct {
		payload string
		version int
		feature string
	}{
		{`{"uid":"a"}`, 30, ""},
		{`{"uid":"a","links_scope":"all"}`, 30, SnapshotLinksEventFeature},
		{`{"uid":"a","links_scope":"all","comments":[{"reply_to_uid":"b"}]}`, 32, SnapshotLinksEventFeature},
	} {
		version, feature, err := FederationEventPayloadWireVersion("issue.snapshot", jsontext.Value(tc.payload))
		require.NoError(t, err)
		require.Equal(t, tc.version, version)
		require.Equal(t, tc.feature, feature)
	}
}
