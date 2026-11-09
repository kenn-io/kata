package dbtest

import (
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// A downstream enrollment cannot author root policy, even if it asserts the
// pinned root in its source event. Upstream baseline and recovery still work.
func checkCronControlIngress(t *testing.T, store db.Storage) error {
	request, err := json.Marshal(map[string]any{"kind": "run", "job_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAV", "definition_event_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAW", "requested_by": "operator", "requested_at": "2026-01-02T03:04:05Z", "expires_at": "2026-01-02T03:05:05Z"})
	require.NoError(t, err)
	for _, control := range []struct{ name, key, value string }{
		{"pause", "cron.pause", `{"paused_by":"operator","paused_at":"2026-01-02T03:04:05Z","reason":"maintenance"}`},
		{"passover", "cron.passover", `{"version":1,"through":"2026-01-02T03:04:05Z","by":"operator"}`},
		{"request", "cron.request." + requestUIDAt(t, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)), string(request)},
	} {
		t.Run(control.name, func(t *testing.T) { checkCronControlIngressValue(t, store, control.name, control.key, control.value) })
	}
	return nil
}

func checkCronControlIngressValue(t *testing.T, store db.Storage, name, key, value string) {
	readValue := func(t *testing.T, p db.Project) jsontext.Value {
		t.Helper()
		var values map[string]jsontext.Value
		require.NoError(t, json.Unmarshal([]byte(p.Metadata), &values))
		return values[key]
	}
	t.Run("legacy enrollment", func(t *testing.T) {
		ctx := t.Context()
		p, err := store.CreateProject(ctx, name+"-ingress-project")
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleHub, HubProjectID: p.ID, HubProjectUID: p.UID, Enabled: true})
		require.NoError(t, err)
		peer := "00000000000000000000000006"
		source := newRemoteEvent(t, p, nil, "project.metadata_updated", "member", peer, 300, jsontext.Value(`{"project_uid":"`+p.UID+`","diff":{"`+key+`":{"from":null,"to":`+value+`}}}`))
		_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: p.ID, SpokeInstanceUID: peer, BoundActor: "member", Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: source}}})
		require.ErrorIs(t, err, db.ErrFederationIngestValidation)
		current, err := store.ProjectByID(ctx, p.ID)
		require.NoError(t, err)
		require.Empty(t, readValue(t, current))
	})
	for _, downstream := range []bool{false, true} {
		direction := "upstream"
		if downstream {
			direction = "downstream"
		}
		t.Run(direction, func(t *testing.T) {
			ctx := t.Context()
			p, err := store.CreateProject(ctx, name+"-relay-"+direction)
			require.NoError(t, err)
			root := "00000000000000000000000002"
			public, _, err := ed25519.GenerateKey(nil)
			require.NoError(t, err)
			require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: p.UID, AuthorityUID: root, KeyID: db.RootPublicKeyID(public), PublicKey: public}))
			_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: 42, HubProjectUID: p.UID, Actor: "member", PushEnabled: true, Enabled: true})
			require.NoError(t, err)
			binding, err := uid.New()
			require.NoError(t, err)
			_, err = store.SetRelayBindingConfig(ctx, p.ID, db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: binding, UpstreamInstanceUID: root, AuthorityUID: root, HubPath: []string{root, store.InstanceUID()}, LocalActor: "member", ServeDownstream: true, ResetEpoch: 1})
			require.NoError(t, err)
			sender := root
			path := []string{root}
			if downstream {
				token, _, e := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "member", AdminActor: "admin", PlaintextToken: name + "-parent-token"})
				require.NoError(t, e)
				sender = "00000000000000000000000006"
				grant, e := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: p.ID, ParentTokenID: token.ID, SpokeInstanceUID: sender, ProtocolVersion: db.RelayProtocolVersion, Token: name + "-child-token", ServeDownstream: true})
				require.NoError(t, e)
				binding = grant.Enrollment.RelayBindingUID
				path = []string{root, sender}
			}
			deliver := func(sequence int64, raw string) error {
				source := newRemoteEvent(t, p, nil, "project.metadata_updated", "operator", root, 300+sequence, jsontext.Value(`{"project_uid":"`+p.UID+`","diff":{"`+key+`":{"from":null,"to":`+raw+`}}}`))
				body, e := db.EncodeRelaySourceEvent(source)
				require.NoError(t, e)
				envelope, e := db.SealRelayEnvelope(db.RelayEnvelope{Version: db.RelayProtocolVersion, BindingUID: binding, ProjectUID: p.UID, AuthorityUID: root, SenderInstanceUID: sender, ReceiverInstanceUID: store.InstanceUID(), Epoch: 1, Sequence: sequence, Stream: db.RelayStreamEvent, Path: path, SourceUID: source.EventUID, SourceHash: source.ContentHash, Body: body})
				require.NoError(t, e)
				_, e = store.AcceptRelayDeliveries(ctx, binding, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{envelope}})
				return e
			}
			if downstream {
				require.ErrorIs(t, deliver(1, value), db.ErrFederationIngestValidation)
				current, e := store.ProjectByID(ctx, p.ID)
				require.NoError(t, e)
				require.Empty(t, readValue(t, current))
				return
			}
			require.ErrorIs(t, deliver(1, `{"unknown":true}`), db.ErrFederationIngestValidation)
			require.NoError(t, deliver(1, value), "root from-empty baseline must remain accepted")
			current, err := store.ProjectByID(ctx, p.ID)
			require.NoError(t, err)
			require.JSONEq(t, value, string(readValue(t, current)))
			if key == "cron.passover" {
				require.ErrorIs(t, deliver(2, `null`), db.ErrFederationIngestValidation)
				return
			}
			require.NoError(t, deliver(2, `null`))
			current, err = store.ProjectByID(ctx, p.ID)
			require.NoError(t, err)
			require.Empty(t, readValue(t, current))
		})
	}
}
