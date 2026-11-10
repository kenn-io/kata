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
		{"issue.snapshot", `{"uid":"a","links_scope":"all"}`, "", false},
		{"project.federation_enabled", `{"links_scope":"all"}`, "", false},
		{"issue.moved", `{"snapshot":{"links_scope":"all"}}`, IssueMoveEventFeature, false},
		{"issue.snapshot", `{"links_scope":null}`, "", true},
		{"issue.snapshot", `{"links_scope":"partial"}`, "", true},
		{"issue.snapshot", `{"links_scope":1}`, "", true},
		{"issue.created", `{"links_scope":"all"}`, "", false},
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
	version, feature, err := FederationEventPayloadWireVersion("issue.moved", jsontext.Value(`{"snapshot":{"comments":[{"reply_to_uid":"b"}]}}`))
	require.NoError(t, err)
	require.Equal(t, 32, version)
	require.Equal(t, IssueMoveEventFeature, feature)
}
