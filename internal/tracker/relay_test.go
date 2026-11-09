package tracker

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRelayAssertionKeepsQualifiedSubjectAndLiveHop(t *testing.T) {
	now := time.Unix(1900000000, 0)
	const source = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	const relay = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	const root = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
	const binding = "01ARZ3NDEKTSV4RRFFQ69G5FAY"
	a := RelayAssertion{Version: 1, RootInstanceUID: root, ProjectUID: binding, Source: Principal{AuthorityInstanceUID: source, Kind: SubjectAccount, Subject: "member"}, IssuerInstanceUID: relay, AudienceInstanceUID: root, OperationUID: binding, RequestDigest: selectionDigestFixture(), ExpiresAt: now.Add(30 * time.Second).Unix(), Path: []RelayHop{{InstanceUID: source, BindingUID: binding, ResetEpoch: 1}, {InstanceUID: relay, BindingUID: binding, ResetEpoch: 2}}}
	require.NoError(t, a.Validate(root, relay, binding, 2, true, now))
	require.Error(t, a.Validate(root, relay, binding, 2, false, now), "ordinary leaf transport cannot assert a downstream subject")
	require.Error(t, a.Validate(root, relay, binding, 3, true, now), "stale enrollment incarnation")
	require.Error(t, a.Validate(root, relay, binding, 2, true, now.Add(31*time.Second)), "expired assertion")
	changed := a
	changed.Source.AuthorityInstanceUID = root
	require.Error(t, changed.Validate(root, relay, binding, 2, true, now), "same name under another authority is not this subject")
	changed = a
	changed.Path = slices.Clone(a.Path)
	changed.Path[0].InstanceUID = relay
	require.Error(t, changed.Validate(root, relay, binding, 2, true, now), "cycle")
	changed = a
	changed.Source.Kind = SubjectService
	require.Error(t, changed.Validate(root, relay, binding, 2, true, now), "transport cannot invent background service authority")
}
func selectionDigestFixture() string {
	return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}
