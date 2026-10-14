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

// RunRelayUpstreamLocalAuthority exercises relay upstream local authority on the supplied native store.
// R3/R5: a company receipt establishes accountability, while the personal hub
// uses its independently authenticated local account to access its replica.
func RunRelayUpstreamLocalAuthority(t *testing.T, store db.Storage) {
	for _, receiptFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "receipt_first", false: "event_first"}[receiptFirst], func(t *testing.T) {
			ctx := t.Context()
			project, err := store.CreateProject(ctx, "shared-"+map[bool]string{true: "receipt", false: "event"}[receiptFirst])
			require.NoError(t, err)
			team, _, err := store.CreateTeam(ctx, "team-"+map[bool]string{true: "receipt", false: "event"}[receiptFirst], "admin")
			require.NoError(t, err)
			_, err = store.SetTeamMembership(ctx, team.UID, "personal-member", true, "admin")
			require.NoError(t, err)
			policy, err := store.ProjectAccessPolicy(ctx, project.UID)
			require.NoError(t, err)
			policy.Visibility = "teams"
			policy.TeamUIDs = []string{team.UID}
			_, _, err = store.SetProjectAccessPolicy(ctx, policy, "admin")
			require.NoError(t, err)
			_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "company-member", PushEnabled: true, Enabled: true})
			require.NoError(t, err)
			rootUID := "00000000000000000000000002"
			public, private, err := ed25519.GenerateKey(nil)
			require.NoError(t, err)
			pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}
			require.NoError(t, store.PinRootAuthority(ctx, pin))
			bindingUID, err := uid.New()
			require.NoError(t, err)
			config := db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: bindingUID, UpstreamInstanceUID: rootUID, AuthorityUID: rootUID, HubPath: []string{rootUID, store.InstanceUID()}, LocalActor: "personal-member", ServeDownstream: true, ResetEpoch: 1}
			_, err = store.SetRelayBindingConfig(ctx, project.ID, config)
			require.NoError(t, err)
			issueUID, err := uid.New()
			require.NoError(t, err)
			leafUID := "00000000000000000000000007"
			source := newRemoteEvent(t, project, &issueUID, "issue.created", "source-assistant", leafUID, 300, jsontext.Value(`{"uid":"`+issueUID+`","title":"Shared source task","author":"source-assistant","metadata":{}}`))
			receipt, err := db.SignRootReceipt(db.AttributionReceipt{Version: 1, ProjectUID: project.UID, EventUID: source.EventUID, ContentHash: source.ContentHash, AuthorityUID: rootUID, AccountableActor: "company-member", SourceActor: source.Actor, IngressInstanceUID: leafUID, AcceptedAt: time.Now().UTC(), ResetEpoch: 1, Sequence: 1, KeyID: pin.KeyID}, private)
			require.NoError(t, err)
			sourceBody, err := db.EncodeRelaySourceEvent(source)
			require.NoError(t, err)
			receiptBody, err := json.Marshal(receipt)
			require.NoError(t, err)
			seal := func(stream string, body []byte, path []string) db.RelayBatch {
				envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: db.RelayProtocolVersion, BindingUID: bindingUID, ProjectUID: project.UID, AuthorityUID: rootUID, SenderInstanceUID: rootUID, ReceiverInstanceUID: store.InstanceUID(), Epoch: 1, Sequence: 31, Stream: stream, Path: path, SourceUID: source.EventUID, SourceHash: source.ContentHash, Body: body})
				require.NoError(t, err)
				return db.RelayBatch{Stream: stream, Envelopes: []db.RelayEnvelope{envelope}}
			}
			eventBatch := seal(db.RelayStreamEvent, sourceBody, []string{leafUID, rootUID})
			receiptBatch := seal(db.RelayStreamReceipt, receiptBody, []string{rootUID})
			first, second := eventBatch, receiptBatch
			if receiptFirst {
				first, second = second, first
			}
			_, err = store.AcceptRelayDeliveries(ctx, bindingUID, first)
			require.NoError(t, err)
			_, err = store.AcceptRelayDeliveries(ctx, bindingUID, second)
			require.NoError(t, err)
			issue, err := store.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, "verified", issue.Verification)
			require.Equal(t, "company-member", issue.AccountableActor)
			require.Equal(t, "source-assistant", issue.Author)
			_, err = store.SetTeamMembership(ctx, team.UID, "personal-member", false, "admin")
			require.NoError(t, err)
			_, err = store.AcceptRelayDeliveries(ctx, bindingUID, first)
			require.Error(t, err, "replay cannot borrow the foreign accountable actor as local authority")
		})
	}
}
