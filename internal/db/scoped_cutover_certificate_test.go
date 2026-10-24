package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScopedCutoverCertificate(t *testing.T) {
	v, pins, keys := verificationView(t)
	expected := ScopedProtocolCutoverExpectation{UID: verificationID(60), AuthorityUID: v.AuthorityUID, RequestDigest: strings.Repeat("a", 64), ProjectUIDs: []string{pins[0].ProjectUID, pins[1].ProjectUID}, Floor: 2, ReadinessDigest: ScopedOfferedDigest([]byte(" readiness \n")), SuspendedBindingsDigest: ScopedOfferedDigest([]byte("[]\n"))}
	c := ScopedProtocolCutoverCertificate{Version: 1, Cutover: expected}
	raw, err := ScopedCutoverCertificateSigningBytes(c)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"domain":"kata.scoped.cutover.v1"`)
	require.Contains(t, string(raw), `"signatures":[]`)
	require.Equal(t, `{"domain":"kata.scoped.cutover.v1","record":{"cutover":{"authority_uid":"00000000000000000000000009","floor":2,"project_uids":["00000000000000000000000001","00000000000000000000000002"],"readiness_digest":"c5fe29fb76f0604ae608fa7f4ed42e0d5d9e6b7fc1d64f00a74b02fde018d329","request_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","suspended_bindings_digest":"37517e5f3dc66819f61f5a7bb8ace1921282415f10551d2defa5c3eb0985b570","uid":"00000000000000000000000060"},"signatures":[],"version":1}}`, string(raw))
	c.Signatures = verificationSign(raw, pins, keys)
	require.NoError(t, VerifyScopedCutoverCertificate(c, expected, pins))
	require.Error(t, VerifyScopedCutoverCertificate(c, expected, nil), "embedded or incoming signer identity supplies no trust")
	c.Signatures = c.Signatures[:1]
	require.Error(t, VerifyScopedCutoverCertificate(c, expected, pins), "distinct-key second authority missing")
	c.Signatures = verificationSign(raw, pins, keys)
	changed := expected
	changed.ReadinessDigest = ScopedOfferedDigest([]byte("readiness\n"))
	require.Error(t, VerifyScopedCutoverCertificate(c, changed, pins), "exact retained inventory bytes are committed")
	for _, mutate := range []func(*ScopedProtocolCutoverCertificate){
		func(c *ScopedProtocolCutoverCertificate) { c.Version = 2 },
		func(c *ScopedProtocolCutoverCertificate) { c.Cutover.Floor = 1 },
		func(c *ScopedProtocolCutoverCertificate) {
			c.Cutover.ProjectUIDs = []string{pins[1].ProjectUID, pins[0].ProjectUID}
		},
		func(c *ScopedProtocolCutoverCertificate) {
			c.Cutover.ProjectUIDs = []string{pins[0].ProjectUID, pins[0].ProjectUID}
		},
		func(c *ScopedProtocolCutoverCertificate) { c.Cutover.ReadinessDigest = strings.Repeat("A", 64) },
	} {
		bad := c
		mutate(&bad)
		require.Error(t, VerifyScopedCutoverCertificate(bad, bad.Cutover, pins))
	}
	// Each project has an independently enrolled pin; identical actual keys may
	// share one endorsement, as with the other scoped manifests.
	pins[1].KeyID = pins[0].KeyID
	pins[1].PublicKey = pins[0].PublicKey
	c.Signatures = verificationSign(raw, pins[:1], keys)
	require.NoError(t, VerifyScopedCutoverCertificate(c, expected, pins))
}
