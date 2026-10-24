package db

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/embedding"
)

func verificationID(n int) string { return fmt.Sprintf("%026d", n) }
func verificationView(t *testing.T) (ScopedViewDescriptor, []RootKeyPin, map[string]ed25519.PrivateKey) {
	t.Helper()
	v := ScopedViewDescriptor{Version: 2, Schema: 1, Key: ScopedViewKey{verificationID(20), 1}, AuthorityUID: verificationID(9), DestinationUID: verificationID(8), RouteDigest: strings.Repeat("a", 64), RequestDigest: strings.Repeat("b", 64), Route: []ScopedHopBinding{{BindingUID: verificationID(7), InstanceUID: verificationID(8), Epoch: 1}}}
	keys := map[string]ed25519.PrivateKey{}
	var pins []RootKeyPin
	for n := 1; n <= 2; n++ {
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{byte(n)}, 32))
		public := key.Public().(ed25519.PublicKey)
		pin := RootKeyPin{ProjectUID: verificationID(n), AuthorityUID: v.AuthorityUID, KeyID: RootPublicKeyID(public), PublicKey: public}
		pins = append(pins, pin)
		keys[pin.ProjectUID] = key
		v.Projects = append(v.Projects, ScopedProjectPin{ProjectUID: pin.ProjectUID, Pin: pin})
	}
	require.NoError(t, v.Validate())
	return v, pins, keys
}
func verificationSign(raw []byte, pins []RootKeyPin, keys map[string]ed25519.PrivateKey) []ScopedSignature {
	var out []ScopedSignature
	for _, pin := range pins {
		out = append(out, ScopedSignature{ProjectUID: pin.ProjectUID, KeyID: pin.KeyID, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(keys[pin.ProjectUID], raw))})
	}
	return out
}
func verificationCollections() ScopedCollections {
	return ScopedCollections{Projects: []ScopedProjectState{}, Issues: []ScopedIssueState{}, Comments: []ScopedCommentState{}, Labels: []ScopedLabelState{}, Definitions: []ScopedDefinitionState{}, References: []ScopedReference{}, Provenance: []ScopedProvenance{}, Artifacts: []embedding.ArtifactManifest{}}
}
func verificationCheckpoint(t *testing.T) (ScopedCheckpoint, ScopedViewDescriptor, []RootKeyPin, map[string]ed25519.PrivateKey) {
	t.Helper()
	v, pins, keys := verificationView(t)
	c := ScopedCheckpoint{View: v, Cut: ScopedCut{Sequence: 1, Digest: strings.Repeat("0", 64), CaptureUID: verificationID(21)}, Collections: verificationCollections()}
	var err error
	c.Cut.Digest, err = ScopedCheckpointDigest(c)
	require.NoError(t, err)
	raw, err := ScopedCheckpointSigningBytes(c)
	require.NoError(t, err)
	c.Signatures = verificationSign(raw, pins, keys)
	return c, v, pins, keys
}

// The golden pins json/v2's native nil-slice [] transcript. Changing it to
// omitted/null or signing a different domain must break interoperable bytes.
func TestScopedVerificationTranscriptGolden(t *testing.T) {
	r := SourceResult{OperationUID: verificationID(30), OperationDigest: strings.Repeat("a", 64), State: "rejected", ReasonCode: "conflict"}
	raw, err := ScopedSourceResultSigningBytes(r)
	require.NoError(t, err)
	want := `{"domain":"kata.scoped.result.v1","record":{"members":[],"operation_digest":"` + strings.Repeat("a", 64) + `","operation_uid":"00000000000000000000000030","reason_code":"conflict","receipts":[],"signatures":[],"state":"rejected"}}`
	require.Equal(t, want, string(raw))
	r.Signatures = []ScopedSignature{{ProjectUID: verificationID(1), Signature: "outer"}}
	same, err := ScopedSourceResultSigningBytes(r)
	require.NoError(t, err)
	require.Equal(t, raw, same)
	r.Receipts = []AttributionReceipt{{Signature: []byte{1, 2, 3}}}
	nested, err := ScopedSourceResultSigningBytes(r)
	require.NoError(t, err)
	require.Contains(t, string(nested), `"signature":"AQID"`, "nested receipt endorsement remains committed")
}

func TestScopedVerificationCheckpointTrustAndDigest(t *testing.T) {
	c, v, pins, keys := verificationCheckpoint(t)
	require.NoError(t, VerifyScopedCheckpoint(c, v, pins))
	require.Error(t, VerifyScopedCheckpoint(c, v, nil), "embedded pins cannot establish trust")
	expected := v
	expected.RequestDigest = strings.Repeat("f", 64)
	require.Error(t, VerifyScopedCheckpoint(c, expected, pins), "expected staged descriptor is binding")
	c.Signatures = c.Signatures[:1]
	require.Error(t, VerifyScopedCheckpoint(c, v, pins), "all selected projects endorse common manifest")
	raw, err := ScopedCheckpointSigningBytes(c)
	require.NoError(t, err)
	c.Signatures = verificationSign(raw, pins, keys)
	c.Signatures = append(c.Signatures, c.Signatures[0])
	require.Error(t, VerifyScopedCheckpoint(c, v, pins), "duplicate endorsement")
	c.Signatures = verificationSign(raw, pins, keys)
	c.Cut.Digest = strings.Repeat("c", 64)
	raw, err = ScopedCheckpointSigningBytes(c)
	require.NoError(t, err)
	c.Signatures = verificationSign(raw, pins, keys)
	require.Error(t, VerifyScopedCheckpoint(c, v, pins), "valid signature cannot excuse wrong content digest")
	c.Cut.Sequence = 9223372036854775807
	raw, err = ScopedCheckpointSigningBytes(c)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"sequence":9223372036854775807`)
}

func TestScopedVerificationReferenceEndorsements(t *testing.T) {
	v, pins, keys := verificationView(t)
	r := ScopedReference{Slot: "close", Kind: "close", Subject: ScopedEndpoint{pins[0].ProjectUID, verificationID(40)}, Revision: 1}
	raw, err := ScopedReferenceSigningBytes(v, r)
	require.NoError(t, err)
	r.Signatures = verificationSign(raw, pins[:1], keys)
	require.NoError(t, VerifyScopedReference(v, r, pins))
	r.Signatures = verificationSign(raw, pins, keys)
	require.Error(t, VerifyScopedReference(v, r, pins), "null names subject only")
	r.Present = true
	r.Close = &ScopedCloseValue{Kind: "duplicate", PeerProjectUID: pins[1].ProjectUID, PeerIssueUID: verificationID(41)}
	raw, err = ScopedReferenceSigningBytes(v, r)
	require.NoError(t, err)
	r.Signatures = verificationSign(raw, pins[:1], keys)
	require.Error(t, VerifyScopedReference(v, r, pins), "present close needs peer")
	r.Signatures = verificationSign(raw, pins, keys)
	require.NoError(t, VerifyScopedReference(v, r, pins))
	r.Close.PeerProjectUID = verificationID(3)
	require.Error(t, VerifyScopedReference(v, r, pins), "peer outside selected scope")
}

func TestScopedVerificationSharedKeyEndorsement(t *testing.T) {
	v, pins, keys := verificationView(t)
	// Two independently enrolled project bindings share one actual root key.
	// One named participating endorsement covers both, without trusting labels.
	pins[1].PublicKey = bytes.Clone(pins[0].PublicKey)
	pins[1].KeyID = pins[0].KeyID
	v.Projects[1].Pin = pins[1]
	c := ScopedCheckpoint{View: v, Cut: ScopedCut{Sequence: 1, CaptureUID: verificationID(21)}, Collections: verificationCollections()}
	var err error
	c.Cut.Digest, err = ScopedCheckpointDigest(c)
	require.NoError(t, err)
	raw, err := ScopedCheckpointSigningBytes(c)
	require.NoError(t, err)
	c.Signatures = verificationSign(raw, pins[:1], keys)
	require.NoError(t, VerifyScopedCheckpoint(c, v, pins))
	pins[1].AuthorityUID = verificationID(90)
	require.Error(t, VerifyScopedCheckpoint(c, v, pins), "shared key never bridges authorities")
}

func TestScopedVerificationNestedAndBatch(t *testing.T) {
	c, v, pins, keys := verificationCheckpoint(t)
	p := ScopedProvenance{ProjectUID: pins[0].ProjectUID, Kind: "issue", EntityUID: verificationID(40), Creator: "member", AttributionStatus: "verified"}
	raw, err := ScopedProvenanceSigningBytes(v, p)
	require.NoError(t, err)
	p.Signatures = verificationSign(raw, pins[:1], keys)
	require.NoError(t, VerifyScopedProvenance(v, p, pins))
	c.Collections.Provenance = append(c.Collections.Provenance, p)
	c.Cut.Digest, err = ScopedCheckpointDigest(c)
	require.NoError(t, err)
	raw, err = ScopedCheckpointSigningBytes(c)
	require.NoError(t, err)
	c.Signatures = verificationSign(raw, pins, keys)
	require.NoError(t, VerifyScopedCheckpoint(c, v, pins))
	c.Collections.Provenance[0].Signatures = nil
	c.Cut.Digest, err = ScopedCheckpointDigest(c)
	require.NoError(t, err)
	raw, err = ScopedCheckpointSigningBytes(c)
	require.NoError(t, err)
	c.Signatures = verificationSign(raw, pins, keys)
	require.Error(t, VerifyScopedCheckpoint(c, v, pins), "outer endorsement cannot replace nested endorsement")
	b := ScopedBatch{View: v, Cut: c.Cut, PreviousDigest: strings.Repeat("1", 64), Effects: []ScopedEffect{
		{Collection: "projects", Key: pins[0].ProjectUID, Action: "replace", Revision: 1, Project: &ScopedProjectState{UID: pins[0].ProjectUID, Name: "example-project"}},
		{Collection: "issues", Key: pins[0].ProjectUID + ":" + verificationID(40), Action: "remove", Revision: 1},
	}}
	b.ComponentDigests, err = ScopedBatchComponentDigests(b.Effects)
	require.NoError(t, err)
	b.Cut.Digest, err = ScopedBatchDigest(b)
	require.NoError(t, err)
	raw, err = ScopedBatchSigningBytes(b)
	require.NoError(t, err)
	b.Signatures = verificationSign(raw, pins, keys)
	require.NoError(t, VerifyScopedBatch(b, v, pins))
	b.PreviousDigest = strings.Repeat("2", 64)
	require.Error(t, VerifyScopedBatch(b, v, pins))
}

func TestScopedVerificationSourceResultAndCoverage(t *testing.T) {
	v, pins, keys := verificationView(t)
	op := SourceOperation{Version: 1, UID: verificationID(50), SourceInstanceUID: verificationID(8), CommandKind: "createIssue", Principal: CustodyPrincipal{AuthorityUID: v.AuthorityUID, BindingUID: verificationID(7), Epoch: 1, AccountableActor: "member", RouteDigest: v.RouteDigest}, Members: []SourceMember{{ProjectUID: pins[0].ProjectUID, EventUID: verificationID(51), ContentHash: strings.Repeat("3", 64), Ordinal: 0}}}
	var err error
	op.Digest, err = SourceOperationDigest(op)
	require.NoError(t, err)
	receipt, err := SignRootReceipt(AttributionReceipt{Version: 1, ProjectUID: pins[0].ProjectUID, EventUID: op.Members[0].EventUID, ContentHash: op.Members[0].ContentHash, AuthorityUID: v.AuthorityUID, AccountableActor: "member", SourceActor: "member", IngressInstanceUID: verificationID(8), AcceptedAt: time.Unix(1, 0).UTC(), ResetEpoch: 1, Sequence: 1, KeyID: pins[0].KeyID}, keys[pins[0].ProjectUID])
	require.NoError(t, err)
	result := SourceResult{OperationUID: op.UID, OperationDigest: op.Digest, State: "accepted", Members: op.Members, Receipts: []AttributionReceipt{receipt}}
	raw, err := ScopedSourceResultSigningBytes(result)
	require.NoError(t, err)
	result.Signatures = verificationSign(raw, pins[:1], keys)
	require.NoError(t, VerifyScopedSourceResult(result, op, pins))
	result.Receipts[0].Signature = bytes.Repeat([]byte{0}, 64)
	raw, err = ScopedSourceResultSigningBytes(result)
	require.NoError(t, err)
	result.Signatures = verificationSign(raw, pins[:1], keys)
	require.Error(t, VerifyScopedSourceResult(result, op, pins), "outer signature does not excuse bad native receipt")
	cut := ScopedCut{Sequence: 2, Digest: strings.Repeat("4", 64), CaptureUID: verificationID(52)}
	c := SourceCoverage{OperationUID: op.UID, OperationDigest: op.Digest, View: v.Key, Cut: cut, Outcome: "future_unknown"}
	raw, err = ScopedSourceCoverageSigningBytes(c)
	require.NoError(t, err)
	c.Signatures = verificationSign(raw, pins[:1], keys)
	require.Error(t, VerifyScopedSourceCoverage(c, op, v, cut, pins), "valid root signature does not widen the closed outcome contract")
	for _, outcome := range []string{"represented", "unchanged", "outside_scope"} {
		c.Outcome = outcome
		raw, err = ScopedSourceCoverageSigningBytes(c)
		require.NoError(t, err)
		c.Signatures = verificationSign(raw, pins[:1], keys)
		require.NoError(t, VerifyScopedSourceCoverage(c, op, v, cut, pins))
	}
	coverage := SourceCoverage{OperationUID: op.UID, OperationDigest: op.Digest, View: v.Key, Cut: cut, Outcome: "outside_scope"}
	raw, err = ScopedSourceCoverageSigningBytes(coverage)
	require.NoError(t, err)
	coverage.Signatures = verificationSign(raw, pins[:1], keys)
	narrow := v
	narrow.Projects = v.Projects[1:]
	require.NoError(t, VerifyScopedSourceCoverage(coverage, op, narrow, cut, pins), "operation-project signer required even outside current scope")
	coverage.Signatures = verificationSign(raw, pins[1:], keys)
	require.Error(t, VerifyScopedSourceCoverage(coverage, op, narrow, cut, pins))
	coverage.Signatures = verificationSign(raw, pins[:1], keys)
	cut.Sequence++
	require.Error(t, VerifyScopedSourceCoverage(coverage, op, narrow, cut, pins))
}

// Every single-byte mutation of an otherwise valid endorsement must reject.
func TestScopedVerificationRejectsSignatureMutation(t *testing.T) {
	c, v, pins, _ := verificationCheckpoint(t)
	original, err := base64.StdEncoding.DecodeString(c.Signatures[0].Signature)
	require.NoError(t, err)
	property := func(index uint8, mask uint8) bool {
		changed := bytes.Clone(original)
		if mask == 0 {
			mask = 1
		}
		changed[int(index)%len(changed)] ^= mask
		copy := c
		copy.Signatures = append([]ScopedSignature(nil), c.Signatures...)
		copy.Signatures[0].Signature = base64.StdEncoding.EncodeToString(changed)
		return VerifyScopedCheckpoint(copy, v, pins) != nil
	}
	require.NoError(t, quick.Check(property, &quick.Config{MaxCount: 128}))
	// Ordinary JSON encoding is only used for the derived object, not any retained
	// native source bytes. Round-trip must preserve verification and key history.
	raw, err := json.Marshal(c)
	require.NoError(t, err)
	var restored ScopedCheckpoint
	require.NoError(t, json.Unmarshal(raw, &restored))
	require.NoError(t, VerifyScopedCheckpoint(restored, v, pins))
}

func TestScopedVerificationRejectsComponentInventoryMismatch(t *testing.T) {
	v, pins, keys := verificationView(t)
	b := ScopedBatch{View: v, Cut: ScopedCut{Sequence: 1, CaptureUID: verificationID(21)}, PreviousDigest: strings.Repeat("1", 64), Effects: []ScopedEffect{{Collection: "issues", Key: pins[0].ProjectUID + ":" + verificationID(40), Action: "remove", Revision: 1}}}
	var err error
	b.Cut.Digest, err = ScopedBatchDigest(b)
	require.NoError(t, err)
	raw, err := ScopedBatchSigningBytes(b)
	require.NoError(t, err)
	b.Signatures = verificationSign(raw, pins, keys)
	require.Error(t, VerifyScopedBatch(b, v, pins), "signed batch cannot omit its represented effect component")
	b.ComponentDigests, err = ScopedBatchComponentDigests(b.Effects)
	require.NoError(t, err)
	b.Cut.Digest, err = ScopedBatchDigest(b)
	require.NoError(t, err)
	raw, err = ScopedBatchSigningBytes(b)
	require.NoError(t, err)
	b.Signatures = verificationSign(raw, pins, keys)
	require.NoError(t, VerifyScopedBatch(b, v, pins))
}

func TestScopedVerificationComponentGolden(t *testing.T) {
	p, i := verificationID(1), verificationID(40)
	remove := ScopedEffect{Collection: "issues", Key: p + ":" + i, Action: "remove", Revision: 7}
	reference := ScopedReference{Slot: "parent", Kind: "parent", Subject: ScopedEndpoint{ProjectUID: p, IssueUID: i}, Revision: 7, Signatures: []ScopedSignature{{ProjectUID: p, KeyID: strings.Repeat("a", 64), Signature: "AQID"}}}
	replace := ScopedEffect{Collection: "references", Key: p + ":" + i + "::parent", Action: "replace", Revision: 7, Reference: &reference}
	got, err := ScopedBatchComponentDigests([]ScopedEffect{replace, remove})
	require.NoError(t, err)
	// Independently calculated SHA-256 vectors for canonical domain-wrapped
	// effect transcripts, including explicit null bodies and nested endorsement.
	want := []ScopedComponentDigest{{ProjectUID: p, Collection: "issues", Digest: "13a155996dd9f12a316b5baab61e38ebc96c7df87fb13c828c2a78d7a43ad4f8"}, {ProjectUID: p, Collection: "references", Digest: "9a64e82c3093130a1d9cc0dba2c8971665429ddc91aff4b6325814cf819bec86"}}
	require.Equal(t, want, got)
	reordered, err := ScopedBatchComponentDigests([]ScopedEffect{remove, replace})
	require.NoError(t, err)
	require.Equal(t, want, reordered)
	reference.Signatures[0].Signature = "BAUG"
	changed, err := ScopedBatchComponentDigests([]ScopedEffect{replace, remove})
	require.NoError(t, err)
	require.NotEqual(t, want[1].Digest, changed[1].Digest)
	empty, err := ScopedBatchComponentDigests(nil)
	require.NoError(t, err)
	require.Empty(t, empty)
}

func TestScopedVerificationStableEffectKeys(t *testing.T) {
	p, i := verificationID(1), verificationID(40)
	for _, tc := range []struct {
		collection, key string
		valid           bool
	}{
		{"projects", p, true}, {"issues", p + ":" + i, true}, {"comments", p + ":" + i, true}, {"definitions", p + ":job:" + i, true},
		{"labels", p + ":" + i + ":label:with:colons", true}, {"references", p + ":" + i + "::slot:with:colons", true},
		{"references", p + "::" + i + ":definition", true}, {"provenance", p + ":issue:" + i, true}, {"artifacts", p + ":" + strings.Repeat("a", 64), true},
		{"issues", i, false}, {"comments", p + ":alias", false}, {"projects", p + ":" + i, false}, {"labels", p + ":" + i + ":", false},
		{"references", p + ":::slot", false}, {"artifacts", p + ":invalid", false}, {"provenance", p + ":issue:" + i + ":extra", false},
	} {
		t.Run(tc.collection+"/"+tc.key, func(t *testing.T) {
			effect := ScopedEffect{Collection: tc.collection, Key: tc.key, Action: "remove", Revision: 1}
			got, err := ScopedEffectProjectUID(effect)
			if tc.valid {
				require.NoError(t, err)
				require.Equal(t, p, got)
			} else {
				require.Error(t, err)
			}
		})
	}
	malformed := ScopedEffect{Collection: "issues", Key: p + ":invalid", Action: "replace", Revision: 1, Issue: &ScopedIssueState{UID: "invalid", ProjectUID: p}}
	_, err := ScopedBatchComponentDigests([]ScopedEffect{malformed})
	require.Error(t, err, "native fact identity must be valid independently of batch inventory")
	wrong := ScopedEffect{Collection: "issues", Key: p + ":" + i, Action: "replace", Revision: 1, Issue: &ScopedIssueState{UID: verificationID(41), ProjectUID: p}}
	_, err = ScopedBatchComponentDigests([]ScopedEffect{wrong})
	require.Error(t, err)
	duplicate := ScopedEffect{Collection: "issues", Key: p + ":" + i, Action: "remove", Revision: 1}
	_, err = ScopedBatchComponentDigests([]ScopedEffect{duplicate, duplicate})
	require.Error(t, err)
}

func TestScopedVerificationRejectsEmptyBatch(t *testing.T) {
	v, pins, keys := verificationView(t)
	b := ScopedBatch{View: v, Cut: ScopedCut{Sequence: 2, CaptureUID: verificationID(21)}, PreviousDigest: strings.Repeat("a", 64), Effects: []ScopedEffect{}, ComponentDigests: []ScopedComponentDigest{}}
	var err error
	b.Cut.Digest, err = ScopedBatchDigest(b)
	require.NoError(t, err)
	raw, err := ScopedBatchSigningBytes(b)
	require.NoError(t, err)
	b.Signatures = verificationSign(raw, pins, keys)
	require.Error(t, VerifyScopedBatch(b, v, pins), "hidden-only or empty work never allocates a visible batch")
}

func TestScopedVerificationProvenanceFamily(t *testing.T) {
	v, pins, keys := verificationView(t)
	for _, tc := range []struct {
		kind, entity string
		valid        bool
	}{
		{"future_kind", verificationID(40), false}, {"project", verificationID(40), false},
		{"project", pins[0].ProjectUID, true}, {"issue", verificationID(40), true}, {"comment", verificationID(40), true}, {"cron_job", verificationID(40), true}, {"cron_workflow", verificationID(40), true},
	} {
		t.Run(tc.kind+"/"+tc.entity, func(t *testing.T) {
			p := ScopedProvenance{ProjectUID: pins[0].ProjectUID, EntityUID: tc.entity, Kind: tc.kind, AttributionStatus: "known"}
			raw, err := ScopedProvenanceSigningBytes(v, p)
			require.NoError(t, err)
			p.Signatures = verificationSign(raw, pins[:1], keys)
			err = VerifyScopedProvenance(v, p, pins)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}

		})
	}
}
