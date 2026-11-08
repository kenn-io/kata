package daemon_test

import (
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

const gateAttachment = `{"origin":"01ARZ3NDEKTSV4RRFFQ69G5FAV","docbank_version":"6f1c2e8a-2222-4222-8222-222222222222","name":"file.pdf","size":12,"media_type":"application/pdf"}`

// Origin-bearing data must stop at every transport, including retained offers
// and cached checkpoints, while legacy references still work without headers.
func TestDocbankOriginTransportFence(t *testing.T) {
	for _, marked := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "origin"}[marked], func(t *testing.T) {
			store := openReplicaServiceStore(t)
			f := newProjectAccessFixture(t, store)
			ctx := t.Context()
			_, err := store.EnableProjectFederation(ctx, f.private.ID, "admin")
			require.NoError(t, err)
			public, private, err := ed25519.GenerateKey(nil)
			require.NoError(t, err)
			signer := db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}
			require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: f.private.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
			server := daemon.NewServer(daemon.ServerConfig{DB: store, RootSigning: daemon.NewRootSigning(signer), Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			endpoint := httptest.NewServer(server.Handler())
			t.Cleanup(endpoint.Close)
			f.server = endpoint
			parent, err := store.ResolveAPIToken(ctx, "member-test-token")
			require.NoError(t, err)
			grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: f.private.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: 1, Token: "docbank-gate-test-token", ServeDownstream: true}) //nolint:gosec // G101: synthetic enrollment credential for an isolated test daemon.
			require.NoError(t, err)
			_, err = store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ProjectID: &f.private.ID, SpokeInstanceUID: "00000000000000000000000007", Token: "docbank-direct-gate-fixture-token", Capabilities: "pull", Actor: "member"}) //nolint:gosec // G101: synthetic enrollment credential for an isolated test daemon.
			require.NoError(t, err)
			directHeaders := bearer("docbank-direct-gate-fixture-token")
			headers := bearer(grant.Token)
			aware := bearer(grant.Token)
			aware[db.EventFeaturesHeader] = db.SupportedEventFeatures
			ref := gateAttachment
			if !marked {
				ref = strings.ReplaceAll(ref, `"origin":"01ARZ3NDEKTSV4RRFFQ69G5FAV",`, "")
			}
			_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: f.issue.ID, Actor: "member", Patch: map[string]jsontext.Value{"attachment.id": []byte(ref)}})
			require.NoError(t, err)
			want := http.StatusOK
			if marked {
				want = http.StatusConflict
			}
			// Project metadata must reject old peers even before they poll a page.
			status, _, raw := f.request(t, http.MethodGet, projectPath(f.private.ID)+"/federation/metadata", "", nil, headers)
			assert.Equal(t, want, status, string(raw))
			base := projectPath(f.private.ID) + "/federation/relay"
			for range 2 {
				status, _, raw = f.request(t, http.MethodGet, base+"?stream=events&limit=1024", "", nil, headers)
				assert.Equal(t, want, status, string(raw))
			}
			status, _, raw = f.request(t, http.MethodGet, base+"?stream=events&limit=1024", "", nil, aware)
			require.Equal(t, http.StatusOK, status, string(raw))
			var offered db.RelayBatch
			require.NoError(t, json.Unmarshal(raw, &offered))
			require.NotEmpty(t, offered.Envelopes)
			// A current-state scan must not substitute for page history checks.
			_, closeEvents, changed, err := store.CloseIssueGuarded(ctx, db.CloseIssueParams{IssueID: f.issue.ID, Reason: "done", Actor: "member", Message: "Documented completed work", Evidence: []db.Evidence{{Type: "attachment", AttachmentID: "id"}}})
			require.NoError(t, err)
			require.True(t, changed)
			_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: f.issue.ID, Actor: "member", Patch: map[string]jsontext.Value{"attachment.id": nil}})
			require.NoError(t, err)
			status, _, raw = f.request(t, http.MethodGet, projectPath(f.private.ID)+"/federation/events?after_id=0&limit=1024", "", nil, directHeaders)
			assert.Equal(t, want, status, string(raw))
			for _, event := range closeEvents {
				if event.Type == "issue.closed" {
					status, _, raw = f.request(t, http.MethodGet, projectPath(f.private.ID)+"/federation/events?after_id="+fmt.Sprint(event.ID-1)+"&limit=1", "", nil, directHeaders)
					assert.Equal(t, want, status, "frozen close descriptor must remain fenced: %s", raw)
				}
			}
			// Reset bodies include retained source history as encoded byte slices.
			for range 2 {
				status, _, raw = f.request(t, http.MethodGet, base+"/reset", "", nil, headers)
				assert.Equal(t, want, status, string(raw))
			}
			status, _, raw = f.request(t, http.MethodGet, base+"/reset", "", nil, aware)
			require.Equal(t, http.StatusOK, status, string(raw))
			var checkpoint db.RelayResetCheckpoint
			require.NoError(t, json.Unmarshal(raw, &checkpoint))
			pin, err := store.RootAuthority(ctx, f.private.UID)
			require.NoError(t, err)
			require.NoError(t, db.VerifyRootResetManifest(pin, checkpoint.Manifest, checkpoint.Snapshot))
		})
	}
}

func TestDocbankOriginRelayAcceptanceFence(t *testing.T) {
	store := openReplicaServiceStore(t)
	f := newProjectAccessFixture(t, store)
	ctx := t.Context()
	_, err := store.EnableProjectFederation(ctx, f.private.ID, "admin")
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: f.private.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	server := daemon.NewServer(daemon.ServerConfig{DB: store, RootSigning: daemon.NewRootSigning(db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}), Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	endpoint := httptest.NewServer(server.Handler())
	t.Cleanup(endpoint.Close)
	f.server = endpoint
	parent, err := store.ResolveAPIToken(ctx, "member-test-token")
	require.NoError(t, err)
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: f.private.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: 1, Token: "docbank-accept-test-token", ServeDownstream: true})
	require.NoError(t, err)
	event := federationRemoteIssueCreatedEvent(t, f.private, grant.Enrollment.SpokeInstanceUID)
	event.Payload = []byte(strings.Replace(string(event.Payload), `"metadata":{}`, `"metadata":{"attachment.id":`+gateAttachment+`}`, 1))
	event.ContentHash, err = db.EventContentHash(db.EventHashInput{UID: event.EventUID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID, ProjectName: event.ProjectName, IssueUID: event.IssueUID, Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS, CreatedAt: event.CreatedAt.UTC().Format(db.EventTimestampFormat), Payload: event.Payload})
	require.NoError(t, err)
	body, err := db.EncodeRelaySourceEvent(event)
	require.NoError(t, err)
	envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: 1, BindingUID: grant.Enrollment.RelayBindingUID, ProjectUID: f.private.UID, AuthorityUID: store.InstanceUID(), SenderInstanceUID: grant.Enrollment.SpokeInstanceUID, ReceiverInstanceUID: store.InstanceUID(), Epoch: 1, Sequence: 1, Stream: db.RelayStreamEvent, Path: []string{grant.Enrollment.SpokeInstanceUID}, SourceUID: event.EventUID, SourceHash: event.ContentHash, Body: body})
	require.NoError(t, err)
	batch := db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{envelope}}
	base := projectPath(f.private.ID) + "/federation/relay:accept"
	status, _, raw := f.request(t, http.MethodPost, base, "", batch, bearer(grant.Token))
	assert.Equal(t, http.StatusConflict, status, string(raw))
	committed, err := store.EventsByUIDs(ctx, f.private.ID, []string{event.EventUID})
	require.ErrorIs(t, err, db.ErrNotFound)
	assert.Empty(t, committed, "refused feature must not advance durable acceptance")
	headers := bearer(grant.Token)
	headers[db.EventFeaturesHeader] = db.SupportedEventFeatures
	status, _, raw = f.request(t, http.MethodPost, base, "", batch, headers)
	require.Equal(t, http.StatusOK, status, string(raw))
	status, _, raw = f.request(t, http.MethodPost, base, "", batch, bearer(grant.Token))
	assert.Equal(t, http.StatusConflict, status, "retained acceptance still requires feature support: %s", raw)
	committed, err = store.EventsByUIDs(ctx, f.private.ID, []string{event.EventUID})
	require.NoError(t, err)
	require.Len(t, committed, 1)
	assert.Equal(t, string(event.Payload), committed[0].Payload)
	assert.Equal(t, event.ContentHash, committed[0].ContentHash)
}
