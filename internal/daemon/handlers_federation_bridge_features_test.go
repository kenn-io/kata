package daemon_test

import (
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// A relay that does not say it reads the whole-issue event of a repeated
// import is not offered one. It gets the events before it, then the upgrade
// error, and resumes once it advertises the feature.
func TestRelayOfferHoldsEventsTheRelayCannotRead(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		ctx := t.Context()
		_, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: f.private.ID, Role: db.FederationRoleHub, HubProjectID: f.private.ID, HubProjectUID: f.private.UID, Enabled: true})
		require.NoError(t, err)
		pub, private, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)
		require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: f.private.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(pub), PublicKey: pub}))
		signer := db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}
		server := daemon.NewServer(daemon.ServerConfig{DB: store, RootSigning: daemon.NewRootSigning(signer), Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		httpServer := httptest.NewServer(server.Handler())
		t.Cleanup(httpServer.Close)
		f.server = httpServer
		parent, err := store.ResolveAPIToken(ctx, "member-test-token")
		require.NoError(t, err)
		//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
		sender, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: f.private.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: 1, Token: "relay-feature-sender-test-token", ServeDownstream: true})
		require.NoError(t, err)
		//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
		reader, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: f.private.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000008", ProtocolVersion: 1, Token: "relay-feature-reader-test-token", ServeDownstream: true})
		require.NoError(t, err)
		base := projectPath(f.private.ID) + "/federation/relay"

		created := federationRemoteIssueCreatedEvent(t, f.private, "00000000000000000000000007")
		restated := created
		restated.EventUID = "01HZNQ7VFPK1XGD8R5MABCD4ED"
		restated.Type, restated.HLCPhysicalMS = db.RepeatedImportEventType, 2
		restated.CreatedAt = time.Now().UTC().Truncate(time.Millisecond)
		restated.Payload = jsontext.Value(`{"title":"spoke work, copied again","body":"","status":"open","updated_at":"` + restated.CreatedAt.Format("2006-01-02T15:04:05.000Z") + `"}`)
		restated.ContentHash, err = db.EventContentHash(db.EventHashInput{
			UID: restated.EventUID, OriginInstanceUID: restated.OriginInstanceUID, ProjectUID: restated.ProjectUID,
			ProjectName: restated.ProjectName, IssueUID: restated.IssueUID, Type: restated.Type, Actor: restated.Actor,
			HLCPhysicalMS: restated.HLCPhysicalMS, HLCCounter: restated.HLCCounter,
			CreatedAt: restated.CreatedAt.Format("2006-01-02T15:04:05.000Z"), Payload: restated.Payload,
		})
		require.NoError(t, err)
		for index, event := range []db.RemoteEvent{created, restated} {
			body, err := db.EncodeRelaySourceEvent(event)
			require.NoError(t, err)
			envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: 1, BindingUID: sender.Enrollment.RelayBindingUID, ProjectUID: f.private.UID, AuthorityUID: store.InstanceUID(), SenderInstanceUID: sender.Enrollment.SpokeInstanceUID, ReceiverInstanceUID: store.InstanceUID(), Epoch: 1, Sequence: int64(index + 1), Stream: db.RelayStreamEvent, Path: []string{event.OriginInstanceUID, sender.Enrollment.SpokeInstanceUID}, SourceUID: event.EventUID, SourceHash: event.ContentHash, Body: body})
			require.NoError(t, err)
			status, _, raw := f.request(t, http.MethodPost, base+":accept", "", db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{envelope}}, bearer(sender.Token))
			require.Equal(t, http.StatusOK, status, string(raw))
		}

		offerTypes := func(headers map[string]string) (int, []string, db.RelayBatch, string) {
			t.Helper()
			status, _, raw := f.request(t, http.MethodGet, base+"?stream=events&limit=1024", "", nil, headers)
			var offer db.RelayBatch
			var types []string
			if status == http.StatusOK {
				require.NoError(t, json.Unmarshal(raw, &offer))
				for _, envelope := range offer.Envelopes {
					event, err := db.DecodeRelaySourceEvent(envelope.Body)
					require.NoError(t, err)
					types = append(types, event.Type)
				}
			}
			return status, types, offer, string(raw)
		}
		// A relay built before the feature sends no feature header.
		status, types, offer, raw := offerTypes(bearer(reader.Token))
		require.Equal(t, http.StatusOK, status, raw)
		require.NotEmpty(t, types)
		require.NotContains(t, types, db.RepeatedImportEventType)
		last := offer.Envelopes[len(offer.Envelopes)-1]
		status, _, ackRaw := f.request(t, http.MethodPost, base+":ack", "", map[string]any{"stream": "events", "epoch": int64(1), "through": last.Sequence, "digest": last.Digest}, bearer(reader.Token))
		require.Equal(t, http.StatusOK, status, string(ackRaw))
		status, _, _, raw = offerTypes(bearer(reader.Token))
		require.Equal(t, http.StatusConflict, status, raw)
		require.Contains(t, raw, "unsupported_event_features")
		require.Contains(t, raw, db.RepeatedAdoptionEventFeature)

		// After its upgrade the same relay advertises the feature and resumes.
		upgraded := bearer(reader.Token)
		upgraded[db.EventFeaturesHeader] = db.SupportedEventFeatures
		status, types, _, raw = offerTypes(upgraded)
		require.Equal(t, http.StatusOK, status, raw)
		require.Equal(t, []string{db.RepeatedImportEventType}, types)
	})
}
