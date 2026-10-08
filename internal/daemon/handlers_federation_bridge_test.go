package daemon_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

var errRelayPostCommitLookup = errors.New("post-commit event lookup failed")

type relayEventLookupFailureStore struct{ db.Storage }

func (s relayEventLookupFailureStore) EventsByUIDs(context.Context, int64, []string) ([]db.Event, error) {
	return nil, errRelayPostCommitLookup
}

func TestLegacyFederationEnrollmentRejectsExpiringAndRevokedAccountTokens(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store, "legacy-enrollment-lifetime")
		ctx := t.Context()
		_, err := store.UpsertFederationBinding(ctx, db.FederationBinding{
			ProjectID: f.public.ID, Role: db.FederationRoleHub,
			HubProjectID: f.public.ID, HubProjectUID: f.public.UID, Enabled: true,
		})
		require.NoError(t, err)

		expires := time.Now().Add(2 * time.Second).UTC()
		_, _, err = store.CreateAPIToken(ctx, db.CreateAPITokenParams{
			PlaintextToken: "expiring-account-test-token", Actor: "member", AdminActor: "admin", ExpiresAt: &expires,
		})
		require.NoError(t, err)
		createEnrollment := func(accountToken, enrollmentToken, spokeUID string) (int, []byte) {
			t.Helper()
			status, _, body := f.request(t, http.MethodPost, "/api/v1/federation/enrollments", "", map[string]any{
				"spoke_instance_uid": spokeUID, "project_id": f.public.ID,
				"capabilities": "pull", "actor": "member", "token": enrollmentToken,
			}, map[string]string{"Authorization": "Bearer " + accountToken})
			return status, body
		}
		metadataStatus := func(enrollmentToken string) int {
			t.Helper()
			status, _, _ := f.request(t, http.MethodGet,
				fmt.Sprintf("/api/v1/projects/%d/federation/metadata", f.public.ID), "", nil,
				map[string]string{"Authorization": "Bearer " + enrollmentToken})
			return status
		}
		status, raw := createEnrollment("expiring-account-test-token", "expiring-legacy-grant-token", "00000000000000000000000006")
		require.Equal(t, http.StatusForbidden, status, string(raw))
		require.Contains(t, string(raw), "federation_enrollment_requires_relay")
		if delay := time.Until(expires); delay > 0 {
			time.Sleep(delay + 20*time.Millisecond)
		}
		assert.NotEqual(t, http.StatusOK, metadataStatus("expiring-legacy-grant-token"),
			"an expired account token must not leave a legacy grant it could have issued")
		_, err = store.AuthorizeFederationToken(ctx, "expiring-legacy-grant-token", f.public.ID, "pull")
		require.ErrorIs(t, err, db.ErrNotFound)

		parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{
			PlaintextToken: "revocable-account-test-token", Actor: "member", AdminActor: "admin",
		})
		require.NoError(t, err)
		status, raw = createEnrollment("revocable-account-test-token", "revocable-legacy-grant-token", "00000000000000000000000007")
		require.Equal(t, http.StatusForbidden, status, string(raw))
		require.Contains(t, string(raw), "federation_enrollment_requires_relay")

		projectID := f.public.ID
		_, err = store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
			Token: "owner-created-legacy-grant", SpokeInstanceUID: "00000000000000000000000008",
			ProjectID: &projectID, Capabilities: "pull", Actor: "member",
		})
		require.NoError(t, err)
		rotateStatus, _, rotateRaw := f.request(t, http.MethodPost,
			"/api/v1/federation/enrollments/actions/rotate", "", map[string]any{
				"spoke_instance_uid": "00000000000000000000000008", "project_id": f.public.ID,
				"capabilities": "pull", "actor": "member", "token": "account-rotated-legacy-grant",
			}, map[string]string{"Authorization": "Bearer revocable-account-test-token"})
		require.Equal(t, http.StatusForbidden, rotateStatus, string(rotateRaw))
		require.Equal(t, http.StatusOK, metadataStatus("owner-created-legacy-grant"),
			"a rejected account-token rotation must leave the existing legacy grant active")

		_, _, err = store.RevokeAPIToken(ctx, parent.ID, "admin")
		require.NoError(t, err)
		assert.NotEqual(t, http.StatusOK, metadataStatus("revocable-legacy-grant-token"),
			"a revoked account token must not leave a legacy grant it could have issued")
		_, err = store.AuthorizeFederationToken(ctx, "revocable-legacy-grant-token", f.public.ID, "pull")
		require.ErrorIs(t, err, db.ErrNotFound)
	})
}

// R1/R3/R4: self-enrollment narrows a live ordinary user credential to one
// chosen project and returns the root pin through the existing enrollment API.
func TestRelayEnrollmentHTTPAccountAndHandshake(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		ctx := t.Context()
		_, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: f.private.ID, Role: db.FederationRoleHub, HubProjectID: f.private.ID, HubProjectUID: f.private.UID, Enabled: true})
		require.NoError(t, err)
		pub, _, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)
		pin := db.RootKeyPin{ProjectUID: f.private.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(pub), PublicKey: pub}
		require.NoError(t, store.PinRootAuthority(ctx, pin))
		//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
		body := map[string]any{"spoke_instance_uid": "00000000000000000000000006", "project_id": f.private.ID, "capabilities": "pull,push,claim", "actor": "member", "token": "http-relay-test-token", "relay": map[string]any{"protocol_version": 1, "serve_downstream": true}}
		status, _, raw := f.request(t, http.MethodPost, "/api/v1/federation/enrollments", "member", body, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var response struct {
			Actor string `json:"actor"`
			Token string `json:"token"`
			Relay struct {
				ProtocolVersion     int           `json:"protocol_version"`
				BindingUID          string        `json:"binding_uid"`
				UpstreamInstanceUID string        `json:"upstream_instance_uid"`
				ResetEpoch          int64         `json:"reset_epoch"`
				HubPath             []string      `json:"hub_path"`
				Root                db.RootKeyPin `json:"root"`
			} `json:"relay"`
		}
		require.NoError(t, json.Unmarshal(raw, &response))
		require.Equal(t, 1, response.Relay.ProtocolVersion, string(raw))
		require.Equal(t, "member", response.Actor)
		require.Equal(t, pin, response.Relay.Root)
		require.Equal(t, store.InstanceUID(), response.Relay.UpstreamInstanceUID)
		require.Equal(t, int64(1), response.Relay.ResetEpoch)
		require.Equal(t, []string{store.InstanceUID(), "00000000000000000000000006"}, response.Relay.HubPath)
		grant, err := store.AuthorizeFederationToken(ctx, response.Token, f.private.ID, "push")
		require.NoError(t, err)
		require.Equal(t, response.Relay.BindingUID, grant.RelayBindingUID)
		parent, err := store.ResolveAPIToken(ctx, "member-test-token")
		require.NoError(t, err)
		require.Equal(t, &parent.ID, grant.ParentTokenID)
		status, _, retry := f.request(t, http.MethodPost, "/api/v1/federation/enrollments", "member", body, nil)
		require.Equal(t, http.StatusOK, status, string(retry))
		require.JSONEq(t, string(raw), string(retry))
		body["actor"] = "nonmember"
		status, _, raw = f.request(t, http.MethodPost, "/api/v1/federation/enrollments", "nonmember", body, nil)
		require.Equal(t, http.StatusNotFound, status, string(raw))
		body["actor"] = "impostor"
		status, _, raw = f.request(t, http.MethodPost, "/api/v1/federation/enrollments", "member", body, nil)
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		body["actor"] = "member"
		body["relay"] = map[string]any{"protocol_version": 2}
		status, _, raw = f.request(t, http.MethodPost, "/api/v1/federation/enrollments", "member", body, nil)
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		body["relay"] = map[string]any{"protocol_version": 1}
		body["project_id"] = nil
		status, _, raw = f.request(t, http.MethodPost, "/api/v1/federation/enrollments", "member", body, nil)
		require.Equal(t, http.StatusBadRequest, status, string(raw))
	})
}

type relayCountingBody struct {
	reader io.Reader
	reads  int
}

func (b *relayCountingBody) Read(p []byte) (int, error) { b.reads++; return b.reader.Read(p) }
func (*relayCountingBody) Close() error                 { return nil }

// R3: transport authentication precedes decoding attacker-controlled bodies,
// including on the small ACK route. Scope is resolved from the URL and grant.
func TestRelayHTTPAuthenticatesBeforeReadingBody(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		for _, suffix := range []string{":accept", ":ack"} {
			t.Run(suffix, func(t *testing.T) {
				body := &relayCountingBody{reader: strings.NewReader(`{"stream":"events","after":0,"envelopes":[]}`)}
				if suffix == ":ack" {
					body.reader = strings.NewReader(`{"stream":"events","epoch":1,"through":1,"digest":"digest"}`)
				}
				request := httptest.NewRequest(http.MethodPost, "http://localhost/api/v1/projects/1/federation/relay"+suffix, body)
				request.Header.Set("Authorization", "Bearer invalid-relay-test-token")
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, request)
				require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
				require.Zero(t, body.reads, "invalid credentials must not read the relay body")
			})
		}
	})
}

// R4/R5: each transport route authenticates the selected live grant, retains
// emitted bytes across lost replies and commits source/proof before acceptance.
func TestRelayHTTPTransportPrefixAndRevocation(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		ctx := t.Context()
		_, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: f.private.ID, Role: db.FederationRoleHub, HubProjectID: f.private.ID, HubProjectUID: f.private.UID, Enabled: true})
		require.NoError(t, err)
		pub, private, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)
		pin := db.RootKeyPin{ProjectUID: f.private.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(pub), PublicKey: pub}
		require.NoError(t, store.PinRootAuthority(ctx, pin))
		signer := db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}
		server := daemon.NewServer(daemon.ServerConfig{DB: store, RootAttributionSigner: &signer, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		httpServer := httptest.NewServer(server.Handler())
		t.Cleanup(httpServer.Close)
		f.server = httpServer
		parent, err := store.ResolveAPIToken(ctx, "member-test-token")
		require.NoError(t, err)
		//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
		grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: f.private.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: 1, Token: "relay-http-transport-test-token", ServeDownstream: true})
		require.NoError(t, err)
		headers := bearer(grant.Token)
		_, offered, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.private.ID, Author: "member", Title: "Offered task"})
		require.NoError(t, err)
		base := projectPath(f.private.ID) + "/federation/relay"
		status, _, raw := f.request(t, http.MethodGet, base+"?stream=events&limit=1024", "", nil, headers)
		require.Equal(t, http.StatusOK, status, string(raw))
		var offer db.RelayBatch
		require.NoError(t, json.Unmarshal(raw, &offer))
		require.Len(t, offer.Envelopes, 2, "bootstrap history precedes the later live write")
		require.Equal(t, grant.Enrollment.RelayBindingUID, offer.Envelopes[0].BindingUID)
		seeded, err := db.DecodeRelaySourceEvent(offer.Envelopes[0].Body)
		require.NoError(t, err)
		require.Equal(t, f.issue.UID, *seeded.IssueUID)
		require.Equal(t, offered.UID, offer.Envelopes[1].SourceUID)
		status, _, retry := f.request(t, http.MethodGet, base+"?stream=events&limit=1024", "", nil, headers)
		require.Equal(t, http.StatusOK, status, string(retry))
		require.JSONEq(t, string(raw), string(retry))
		last := offer.Envelopes[len(offer.Envelopes)-1]
		ack := map[string]any{"stream": "events", "epoch": int64(1), "through": last.Sequence, "digest": last.Digest}
		ack["digest"] = "wrong"
		status, _, raw = f.request(t, http.MethodPost, base+":ack", "", ack, headers)
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		ack["digest"] = last.Digest
		status, _, raw = f.request(t, http.MethodPost, base+":ack", "", ack, headers)
		require.Equal(t, http.StatusOK, status, string(raw))
		status, _, raw = f.request(t, http.MethodGet, base+"?stream=events&limit=1", "", nil, headers)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.NoError(t, json.Unmarshal(raw, &offer))
		require.Empty(t, offer.Envelopes)
		event := federationRemoteIssueCreatedEvent(t, f.private, "00000000000000000000000007")
		body, err := db.EncodeRelaySourceEvent(event)
		require.NoError(t, err)
		envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: 1, BindingUID: grant.Enrollment.RelayBindingUID, ProjectUID: f.private.UID, AuthorityUID: store.InstanceUID(), SenderInstanceUID: grant.Enrollment.SpokeInstanceUID, ReceiverInstanceUID: store.InstanceUID(), Epoch: 1, Sequence: 15, Stream: db.RelayStreamEvent, Path: []string{event.OriginInstanceUID, grant.Enrollment.SpokeInstanceUID}, SourceUID: event.EventUID, SourceHash: event.ContentHash, Body: body})
		require.NoError(t, err)
		batch := db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{envelope}}
		status, _, raw = f.request(t, http.MethodPost, base+":accept", "", batch, headers)
		require.Equal(t, http.StatusOK, status, string(raw))
		var accepted db.RelayAcceptance
		require.NoError(t, json.Unmarshal(raw, &accepted))
		require.Equal(t, int64(15), accepted.Through)
		require.Equal(t, envelope.Digest, accepted.Digest)
		proof, err := store.EntityAttribution(ctx, f.private.UID, "issue", *event.IssueUID)
		require.NoError(t, err)
		require.Equal(t, "member", proof.AccountableActor)
		require.Equal(t, "tester", proof.SourceActor)
		require.NoError(t, db.VerifyRootReceipt(pin, proof))
		status, _, retry = f.request(t, http.MethodPost, base+":accept", "", batch, headers)
		require.Equal(t, http.StatusOK, status, string(retry))
		require.JSONEq(t, string(raw), string(retry))
		// The project in the URL is authority; a body cannot widen it.
		status, _, raw = f.request(t, http.MethodGet, projectPath(f.public.ID)+"/federation/relay?stream=events", "", nil, headers)
		require.Equal(t, http.StatusForbidden, status, string(raw))
		_, _, err = store.RevokeAPIToken(ctx, parent.ID, "admin")
		require.NoError(t, err)
		for _, route := range []struct {
			method, path string
			body         any
		}{{http.MethodGet, base + "?stream=events", nil}, {http.MethodPost, base + ":ack", ack}, {http.MethodPost, base + ":accept", batch}} {
			status, _, raw = f.request(t, route.method, route.path, "", route.body, headers)
			require.Equal(t, http.StatusForbidden, status, string(raw))
		}
	})
}

// Accepted relay events must be published from the acceptance transaction's
// returned rows. A read failure after commit must not turn success into an
// HTTP error or lose the event notification.
func TestRelayHTTPPublishesAcceptedEventsAfterCommitWithoutLookup(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		ctx := t.Context()
		_, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: f.private.ID, Role: db.FederationRoleHub, HubProjectID: f.private.ID, HubProjectUID: f.private.UID, Enabled: true})
		require.NoError(t, err)
		pub, private, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)
		pin := db.RootKeyPin{ProjectUID: f.private.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(pub), PublicKey: pub}
		require.NoError(t, store.PinRootAuthority(ctx, pin))
		parent, err := store.ResolveAPIToken(ctx, "member-test-token")
		require.NoError(t, err)
		//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
		grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: f.private.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: db.RelayProtocolVersion, Token: "relay-post-commit-test-token", ServeDownstream: true})
		require.NoError(t, err)

		sink := &publisherSink{}
		server := daemon.NewServer(daemon.ServerConfig{
			DB:                    relayEventLookupFailureStore{Storage: store},
			RootAttributionSigner: &db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private},
			Broadcaster:           f.broadcaster,
			Hooks:                 sink,
			Auth:                  config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true},
		})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		httpServer := httptest.NewServer(server.Handler())
		t.Cleanup(httpServer.Close)
		f.server = httpServer
		subscription := f.broadcaster.Subscribe(daemon.SubFilter{ProjectID: f.private.ID})
		t.Cleanup(subscription.Unsub)

		const peer = "00000000000000000000000007"
		event := federationRemoteIssueCreatedEvent(t, f.private, peer)
		body, err := db.EncodeRelaySourceEvent(event)
		require.NoError(t, err)
		envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{
			Version: db.RelayProtocolVersion, BindingUID: grant.Enrollment.RelayBindingUID,
			ProjectUID: f.private.UID, AuthorityUID: store.InstanceUID(),
			SenderInstanceUID: grant.Enrollment.SpokeInstanceUID, ReceiverInstanceUID: store.InstanceUID(),
			Epoch: 1, Sequence: 1, Stream: db.RelayStreamEvent,
			Path:      []string{peer, grant.Enrollment.SpokeInstanceUID},
			SourceUID: event.EventUID, SourceHash: event.ContentHash, Body: body,
		})
		require.NoError(t, err)
		status, _, raw := f.request(t, http.MethodPost, projectPath(f.private.ID)+"/federation/relay:accept", "", db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{envelope}}, bearer(grant.Token))

		committed, err := store.EventsByUIDs(ctx, f.private.ID, []string{event.EventUID})
		require.NoError(t, err)
		require.Len(t, committed, 1, "acceptance must already be committed when response-only work runs")
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Equal(t, []int64{committed[0].ID}, sink.ids())
		msg := receiveMsg(t, subscription.Ch, time.Second, "accepted relay event broadcast")
		require.Equal(t, daemon.StreamKindEvent, msg.Kind)
		require.NotNil(t, msg.Event)
		require.Equal(t, committed[0].ID, msg.Event.ID)
	})
}
