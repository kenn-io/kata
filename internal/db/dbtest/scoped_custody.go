package dbtest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

type scopedCustodyStore interface {
	RetainSourceOperation(context.Context, db.CustodyPrincipal, db.SourceSubmission) (db.SourceStatus, error)
}

func scopedCustodyNative(t *testing.T, s db.Storage) scopedCustodyStore {
	t.Helper()
	native, ok := s.(scopedCustodyStore)
	require.True(t, ok, "native whole-manifest custody must be implemented")
	return native
}

// This fixture uses real native grant/parent/policy/pins. Deterministic signing
// keys belong to the existing root fixture, never production credentials.
func newScopedCustodySubmission(t *testing.T, s db.Storage, existing ...ScopedCaptureFixture) (ScopedCaptureFixture, db.CustodyPrincipal, db.SourceSubmission) {
	t.Helper()
	var f ScopedCaptureFixture
	if len(existing) == 0 {
		f = NewScopedCaptureFixture(t, s)
	} else {
		f = existing[0]
	}
	v := f.Params.View
	p := db.CustodyPrincipal{AuthorityUID: v.AuthorityUID, BindingUID: v.Route[0].BindingUID, Epoch: v.Route[0].Epoch, AccountableActor: f.Parent.Actor, RouteDigest: v.RouteDigest}
	op := db.SourceOperation{Version: 1, UID: scopedOwnerUID(901), SourceInstanceUID: v.DestinationUID, CommandKind: "patchIssueMetadata", Principal: p, IdempotencyKey: "example-retry"}
	proof := db.ScopedMembershipProof{Endpoint: db.ScopedEndpoint{ProjectUID: f.Issue.ProjectUID, IssueUID: f.Issue.UID}, Kind: "root_membership"}
	raw, err := db.ScopedMembershipSigningBytes(proof, p)
	require.NoError(t, err)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))
	proof.Signatures = []db.ScopedSignature{{ProjectUID: f.Issue.ProjectUID, KeyID: db.RootPublicKeyID(key.Public().(ed25519.PublicKey)), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, raw))}}
	op.Membership = []db.ScopedMembershipProof{proof}
	submission := db.SourceSubmission{Operation: op}
	for n := range 2 {
		event := db.RemoteEvent{EventUID: scopedOwnerUID(910 + n), OriginInstanceUID: op.SourceInstanceUID, ProjectUID: f.Issue.ProjectUID, IssueUID: &f.Issue.UID, Type: "issue.metadata_updated", Actor: "source-author", HLCPhysicalMS: 100, HLCCounter: int64(n), CreatedAt: time.Date(2026, 10, 1, 0, 0, n, 0, time.UTC), Payload: []byte(fmt.Sprintf("{ \"diff\" : { \"key%d\" : {\"from\":null,\"to\":\"value\"} } } ", n))}
		event.ContentHash, err = db.RemoteEventContentHash(event)
		require.NoError(t, err)
		body, err := db.EncodeRelaySourceEvent(event)
		require.NoError(t, err)
		body = append(append([]byte(" \n"), body...), '\n')
		submission.Originals = append(submission.Originals, event)
		submission.OriginalBytes = append(submission.OriginalBytes, body)
		submission.Operation.Members = append(submission.Operation.Members, db.SourceMember{ProjectUID: event.ProjectUID, EventUID: event.EventUID, ContentHash: event.ContentHash, Ordinal: n})
	}
	submission.Operation.Digest, err = db.SourceOperationDigest(submission.Operation)
	require.NoError(t, err)
	wire, err := db.EncodeScopedSourceSubmission(submission)
	require.NoError(t, err)
	wire = append(append([]byte(" \n"), wire...), []byte("\n ")...)
	submission, err = db.DecodeScopedSourceSubmission(wire)
	require.NoError(t, err)
	return f, p, submission
}

// Contract: an incomplete component cannot acquire complete custody, and its
// exact complete retry retains every original and outer byte atomically.
func RunScopedCustodyWholeManifest(t *testing.T, s db.Storage) {
	t.Helper()
	native := scopedCustodyNative(t, s)
	f, principal, submission := newScopedCustodySubmission(t, s)
	ctx := t.Context()
	partial := submission
	partial.WireBytes = nil
	partial.Originals = partial.Originals[:1]
	partial.OriginalBytes = partial.OriginalBytes[:1]
	status, err := native.RetainSourceOperation(ctx, principal, partial)
	require.NoError(t, err)
	require.Equal(t, "staged", status.State)
	require.Nil(t, status.Result)
	for _, record := range scopedAuthoritySnapshot(t, s) {
		if c, ok := record.(*db.ScopedSourceCustodyExport); ok && c.OperationUID == submission.Operation.UID {
			require.False(t, c.OwnsComplete, "no prefix custody ACK")
		}
	}
	status, err = native.RetainSourceOperation(ctx, principal, submission)
	require.NoError(t, err)
	require.Equal(t, "staged", status.State, "hop ACK is not root acceptance")
	require.NotEqual(t, "settled", status.Settlement)
	require.Nil(t, status.Result)
	before := scopedAuthoritySnapshot(t, s)
	var bodies int
	var complete bool
	for _, record := range before {
		switch r := record.(type) {
		case *db.ScopedSourceOperationExport:
			if r.OperationUID == submission.Operation.UID {
				require.Equal(t, submission.WireBytes, r.ManifestBytes)
				require.NotNil(t, r.AdmissionBytes)
				require.Nil(t, r.TerminalBytes)
			}
		case *db.ScopedPrivateSourceExport:
			if r.OperationUID != nil && *r.OperationUID == submission.Operation.UID {
				require.Equal(t, submission.OriginalBytes[*r.Ordinal], r.EventBytes)
				bodies++
			}
		case *db.ScopedSourceCustodyExport:
			if r.OperationUID == submission.Operation.UID {
				complete = complete || r.OwnsComplete
				require.False(t, r.ResultAcknowledged)
				hop, evidence, err := db.DecodeScopedSourceCustodyPrincipal(r.PrincipalBytes)
				require.NoError(t, err)
				require.NotNil(t, evidence)
				require.Equal(t, f.Params.View.Key, evidence.View)
				require.Equal(t, db.ScopedOfferedDigest(scopedOwnerBytes(t, f.Params.View)), evidence.DescriptorDigest)
				require.Equal(t, principal, hop)
			}
		}
	}
	require.Equal(t, 2, bodies)
	require.True(t, complete)
	_, err = native.RetainSourceOperation(ctx, principal, submission)
	require.NoError(t, err)
	require.Equal(t, before, scopedAuthoritySnapshot(t, s), "lost-response retry preserves exact retained bytes")
	changed := submission
	changed.WireBytes = nil
	changed.Operation.UID = scopedOwnerUID(902)
	changed.Operation.Digest, err = db.SourceOperationDigest(changed.Operation)
	require.NoError(t, err)
	_, err = native.RetainSourceOperation(ctx, principal, changed)
	require.Error(t, err, "changed keyed manifest cannot reuse original bodies")
	require.Equal(t, before, scopedAuthoritySnapshot(t, s))
}

func RunScopedCustodyLiveAuthority(t *testing.T, s db.Storage) {
	t.Helper()
	native := scopedCustodyNative(t, s)
	f, principal, submission := newScopedCustodySubmission(t, s)
	wrong := principal
	wrong.AccountableActor = "different-member"
	_, err := native.RetainSourceOperation(t.Context(), wrong, submission)
	require.Error(t, err, "transport actor cannot self-supply account authority")
	_, _, err = s.RevokeAPIToken(t.Context(), f.Parent.ID, "admin")
	require.NoError(t, err)
	_, err = native.RetainSourceOperation(t.Context(), principal, submission)
	require.Error(t, err, "real parent revocation fences custody admission")
}

func RunScopedCustodyExactBytes(t *testing.T, s db.Storage) {
	t.Helper()
	native := scopedCustodyNative(t, s)
	_, principal, submission := newScopedCustodySubmission(t, s)
	ctx := t.Context()
	bad := submission
	bad.WireBytes = nil
	bad.OriginalBytes = append([][]byte{}, submission.OriginalBytes...)
	bad.OriginalBytes[0] = []byte(`{"not":"a native original"}`)
	_, err := native.RetainSourceOperation(ctx, principal, bad)
	require.Error(t, err)
	_, err = native.RetainSourceOperation(ctx, principal, submission)
	require.NoError(t, err)
	before := scopedAuthoritySnapshot(t, s)
	changed, err := db.DecodeScopedSourceSubmission(append([]byte(" \n"), submission.WireBytes...))
	require.NoError(t, err)
	_, err = native.RetainSourceOperation(ctx, principal, changed)
	require.Error(t, err, "same operation cannot replace authenticated outer bytes")
	changed = submission
	changed.WireBytes = nil
	changed.Operation.UID = scopedOwnerUID(903)
	changed.Operation.IdempotencyKey = "different-key"
	changed.Operation.Digest, err = db.SourceOperationDigest(changed.Operation)
	require.NoError(t, err)
	_, err = native.RetainSourceOperation(ctx, principal, changed)
	require.Error(t, err, "an original cannot move to another manifest even under a new retry key")
	require.Equal(t, before, scopedAuthoritySnapshot(t, s))
}

// Retention validates the closed token shape and preserves its exact bytes.
// Root issuance/currentness/consumption belongs to later root adjudication.
func RunScopedCustodyOpaqueExpectationForwarding(t *testing.T, s db.Storage) {
	t.Helper()
	native := scopedCustodyNative(t, s)
	_, receiving, submission := newScopedCustodySubmission(t, s)
	submission.WireBytes = nil
	submission.Operation.Expectations = []db.MutationExpectation{{Token: "opaque-root-token-unknown-at-this-hop"}}
	var err error
	submission.Operation.Digest, err = db.SourceOperationDigest(submission.Operation)
	require.NoError(t, err)
	wire, err := db.EncodeScopedSourceSubmission(submission)
	require.NoError(t, err)
	wire = append(append([]byte(" \n"), wire...), []byte("\n ")...)
	submission, err = db.DecodeScopedSourceSubmission(wire)
	require.NoError(t, err)
	for _, record := range scopedAuthoritySnapshot(t, s) {
		_, isToken := record.(*db.ScopedMutationExpectationExport)
		require.False(t, isToken, "fixture contains no local issuer row")
	}
	status, err := native.RetainSourceOperation(t.Context(), receiving, submission)
	require.NoError(t, err, "complete custody must not adjudicate an opaque root token")
	require.Equal(t, "staged", status.State)
	require.Nil(t, status.Result)
	records := scopedAuthoritySnapshot(t, s)
	for _, record := range records {
		switch row := record.(type) {
		case *db.ScopedSourceOperationExport:
			require.Equal(t, wire, row.ManifestBytes)
			retained, err := db.DecodeScopedSourceSubmission(row.ManifestBytes)
			require.NoError(t, err)
			require.Equal(t, submission.Operation.Expectations, retained.Operation.Expectations)
		case *db.ScopedMutationExpectationExport:
			t.Fatal("retention invented root issuer state")
		}
	}
	require.NoError(t, db.ValidateImportRecords(records), "root staging an unknown token is also restorable before adjudication")
}
