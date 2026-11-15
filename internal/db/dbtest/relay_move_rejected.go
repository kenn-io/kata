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

// RunRelayRejectsIssueMoves ensures a project-scoped relay enrollment cannot
// move an issue into another project without destination authority.
func RunRelayRejectsIssueMoves(t *testing.T, store db.Storage) {
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	issue, created, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Relayed issue", Author: "personal-member"})
	require.NoError(t, err)
	snapshot := map[string]any{}
	require.NoError(t, json.Unmarshal([]byte(created.Payload), &snapshot))
	snapshot["uid"], snapshot["short_id"] = issue.UID, issue.ShortID
	var movePayload struct {
		IssueUID       string         `json:"issue_uid"`
		FromProjectUID string         `json:"from_project_uid"`
		FromShortID    string         `json:"from_short_id"`
		ToProjectUID   string         `json:"to_project_uid"`
		ToShortID      string         `json:"to_short_id"`
		Snapshot       map[string]any `json:"snapshot"`
	}
	movePayload.IssueUID = issue.UID
	movePayload.FromProjectUID, movePayload.FromShortID = source.UID, issue.ShortID
	movePayload.ToProjectUID, movePayload.ToShortID = target.UID, issue.ShortID
	movePayload.Snapshot = snapshot
	rawMove, err := json.Marshal(movePayload)
	require.NoError(t, err)

	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: source.ID, Role: db.FederationRoleSpoke,
		HubURL: "https://daemon.example", HubProjectID: 42, HubProjectUID: source.UID,
		Actor: "company-member", Enabled: true, PushEnabled: true,
	})
	require.NoError(t, err)
	root := "00000000000000000000000002"
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{
		ProjectUID: source.UID, AuthorityUID: root, KeyID: db.RootPublicKeyID(public), PublicKey: public,
	}))
	_, err = store.SetRelayBindingConfig(ctx, source.ID, db.RelayBindingConfig{
		ProtocolVersion: db.RelayProtocolVersion, BindingUID: "00000000000000000000000005",
		UpstreamInstanceUID: root, AuthorityUID: root,
		HubPath: []string{root, store.InstanceUID()}, LocalActor: "personal-member",
		ServeDownstream: true, ResetEpoch: 1,
	})
	require.NoError(t, err)
	parentToken, err := uid.New()
	require.NoError(t, err)
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{
		Actor: "personal-member", AdminActor: "admin", PlaintextToken: parentToken,
	})
	require.NoError(t, err)
	downstream := "00000000000000000000000006"
	childToken, err := uid.New()
	require.NoError(t, err)
	child, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{
		ProjectID: source.ID, ParentTokenID: parent.ID, SpokeInstanceUID: downstream,
		ProtocolVersion: db.RelayProtocolVersion, Token: childToken,
	})
	require.NoError(t, err)
	move := newRemoteEvent(t, source, &issue.UID, "issue.moved", "personal-member", downstream,
		time.Now().UTC().UnixMilli()+1000, jsontext.Value(rawMove))
	body, err := db.EncodeRelaySourceEvent(move)
	require.NoError(t, err)
	envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{
		Version: db.RelayProtocolVersion, BindingUID: child.Enrollment.RelayBindingUID,
		ProjectUID: source.UID, AuthorityUID: root, SenderInstanceUID: downstream,
		ReceiverInstanceUID: store.InstanceUID(), Epoch: 1, Sequence: 1,
		Stream: db.RelayStreamEvent, Path: []string{downstream}, SourceUID: move.EventUID,
		SourceHash: move.ContentHash, Body: body,
	})
	require.NoError(t, err)
	_, err = store.AcceptRelayDeliveries(ctx, child.Enrollment.RelayBindingUID, db.RelayBatch{
		Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{envelope},
	})
	require.ErrorIs(t, err, db.ErrFederationIngestValidation,
		"relay ingestion cannot apply issue moves outside the enrollment's project")
	current, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, source.ID, current.ProjectID, "a rejected relay move leaves the issue in its authorized source")
}
