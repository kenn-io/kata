package db

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestScopedContractNegotiatedV2StorageShape(t *testing.T) {
	v, _, _ := verificationView(t)
	c := RelayBindingConfig{ProtocolVersion: ScopedRelayProtocolVersion, BindingUID: verificationID(7), UpstreamInstanceUID: verificationID(6), AuthorityUID: v.AuthorityUID, HubPath: []string{v.AuthorityUID, verificationID(6), v.DestinationUID}, LocalActor: "member", ResetEpoch: 1, ScopedView: &v.Key, RootCutoverUID: verificationID(60)}
	require.NoError(t, c.Validate(v.DestinationUID), "persisted v2 config keeps native path/account checks and explicit scoped ownership")
	legacy := c
	legacy.ProtocolVersion = RelayProtocolVersion
	require.Error(t, legacy.Validate(v.DestinationUID), "v1 cannot carry v2 identity")
	legacy.ScopedView = nil
	legacy.RootCutoverUID = ""
	require.NoError(t, legacy.Validate(v.DestinationUID))
	missing := c
	missing.ScopedView = nil
	require.Error(t, missing.Validate(v.DestinationUID))
	missing = c
	missing.RootCutoverUID = ""
	require.Error(t, missing.Validate(v.DestinationUID))
	bad := c
	bad.HubPath = []string{v.AuthorityUID, v.DestinationUID}
	require.Error(t, bad.Validate(v.DestinationUID), "v2 keeps the native upstream path fence")
	parent, project := int64(1), int64(2)
	require.NoError(t, ValidateFederationEnrollmentRelayShape(ScopedRelayProtocolVersion, &c.BindingUID, &parent, &project, 1, false))
	require.Error(t, ValidateFederationEnrollmentRelayShape(ScopedRelayProtocolVersion, &c.BindingUID, nil, &project, 1, false), "v2 does not invent credential-free enrollment")
}
