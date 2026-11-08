package agentsviewservice

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

const projectOne = "00000000000000000000000001"
const rootOne = "00000000000000000000000002"
const leafOne = "00000000000000000000000003"

func discoveryFixture(t testing.TB) (Discovery, TrustedRoot) {
	t.Helper()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	now := time.Unix(1_800_000_000, 0).UTC()
	d, err := SignDescriptor(Descriptor{Version: 1, ProjectUID: projectOne, AuthorityUID: rootOne,
		ServiceID: strings.Repeat("a", 64), LocatorBase: "https://archive.example/team",
		EnvelopeVersion: 1, MaxBytes: MaxSnapshotBytes, IssuedAt: now.Unix(), ExpiresAt: now.Add(MaxLifetime).Unix()}, key)
	require.NoError(t, err)
	return Discovery{Descriptor: d, InstanceUID: rootOne, Route: []RouteObservation{{InstanceUID: rootOne, ProjectUID: projectOne}}},
		TrustedRoot{ProjectUID: projectOne, AuthorityUID: rootOne, KeyID: d.KeyID, PublicKey: key.Public().(ed25519.PublicKey)}
}

// The selected project/root tuple, not a display name or supplied public key,
// establishes which root may advertise the destination.
func TestDiscoveryUsesPinnedRootAndExactPath(t *testing.T) {
	d, pin := discoveryFixture(t)
	raw, err := json.Marshal(d)
	require.NoError(t, err)
	now := time.Unix(d.Descriptor.IssuedAt, 0)
	got, err := DecodeDiscovery(raw, pin, []string{rootOne}, now)
	require.NoError(t, err)
	require.Equal(t, d, got)
	pin.AuthorityUID = leafOne
	_, err = DecodeDiscovery(raw, pin, []string{rootOne}, now)
	require.Error(t, err)
}

func TestDiscoveryRejectsMalformedOrExpiredProof(t *testing.T) {
	d, pin := discoveryFixture(t)
	raw, err := json.Marshal(d)
	require.NoError(t, err)
	oversized := append(bytes.Clone(raw), bytes.Repeat([]byte{' '}, MaxDiscoveryBytes-len(raw)+1)...)
	for _, body := range [][]byte{append(bytes.Clone(raw), []byte(`{}`)...), []byte(`{"unknown":true}`), oversized, bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1)} {
		_, err = DecodeDiscovery(body, pin, []string{rootOne}, time.Unix(d.Descriptor.IssuedAt, 0))
		require.Error(t, err)
	}
	_, err = DecodeDiscovery(raw, pin, []string{rootOne}, time.Unix(d.Descriptor.ExpiresAt, 0).Add(ClockSkew+time.Second))
	require.Error(t, err)
	pin.Retired = true
	_, err = DecodeDiscovery(raw, pin, []string{rootOne}, time.Unix(d.Descriptor.IssuedAt, 0))
	require.Error(t, err)
}

func TestDiscoverySignatureCommitsToLocator(t *testing.T) {
	d, pin := discoveryFixture(t)
	hegel.Test(t, func(ht *hegel.T) {
		value := hegel.Draw(ht, hegel.Text())
		if len(value) > 128 {
			value = value[:128]
		}
		changed := d
		changed.Descriptor.LocatorBase = "https://archive.example/team/case-" + hex.EncodeToString([]byte(value))
		raw, err := json.Marshal(changed)
		require.NoError(ht, err)
		_, err = DecodeDiscovery(raw, pin, []string{rootOne}, time.Unix(d.Descriptor.IssuedAt, 0))
		require.ErrorContains(ht, err, "invalid AgentsView service root signature")
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
		changed.Descriptor, err = SignDescriptor(changed.Descriptor, key)
		require.NoError(ht, err)
		raw, err = json.Marshal(changed)
		require.NoError(ht, err)
		got, err := DecodeDiscovery(raw, pin, []string{rootOne}, time.Unix(d.Descriptor.IssuedAt, 0))
		require.NoError(ht, err)
		require.Equal(ht, changed, got)
	}, hegel.WithTestCases(2000))
}

func TestDiscoveryRejectsChangedRoute(t *testing.T) {
	d, pin := discoveryFixture(t)
	d.InstanceUID = leafOne
	d.Route = append(d.Route, RouteObservation{InstanceUID: leafOne, ProjectUID: projectOne, BindingUID: "00000000000000000000000004", UpstreamInstanceUID: rootOne, ResetEpoch: 2})
	raw, err := json.Marshal(d)
	require.NoError(t, err)
	_, err = DecodeDiscovery(raw, pin, []string{rootOne, leafOne}, time.Unix(d.Descriptor.IssuedAt, 0))
	require.NoError(t, err)
	_, err = DecodeDiscovery(raw, pin, []string{rootOne}, time.Unix(d.Descriptor.IssuedAt, 0))
	require.Error(t, err)
	d.Route[1].UpstreamInstanceUID = leafOne
	raw, err = json.Marshal(d)
	require.NoError(t, err)
	_, err = DecodeDiscovery(raw, pin, []string{rootOne, leafOne}, time.Unix(d.Descriptor.IssuedAt, 0))
	require.Error(t, err)
}
