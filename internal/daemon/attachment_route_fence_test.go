package daemon

import (
	"crypto/ed25519"
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func TestAttachmentRouteFenceRejectsRelayIdentityChange(t *testing.T) {
	store := openClaimGateHelperDB(t)
	project, issue := createClaimGateHelperIssue(t, store)
	binding := db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "operator"}
	observed, err := store.UpsertFederationBinding(t.Context(), binding)
	require.NoError(t, err)
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	const origin = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	err = store.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: origin, KeyID: db.RootPublicKeyID(public), PublicKey: public})
	require.NoError(t, err)
	_, err = store.SetRelayBindingConfig(t.Context(), project.ID, db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", UpstreamInstanceUID: origin, AuthorityUID: origin, HubPath: []string{origin, store.InstanceUID()}, LocalActor: "operator", ResetEpoch: 1})
	require.NoError(t, err)
	patch := map[string]jsontext.Value{"attachment.file": []byte(`{"name":"file.pdf","media_type":"application/pdf","size":7,"docbank_version":"6f1c2e8a-2222-4222-8222-222222222222","origin":"01ARZ3NDEKTSV4RRFFQ69G5FAV"}`)}
	ctx := db.WithMetadataPatchHook(t.Context(), attachmentPatchHook(patch, &observed, nil, origin, nil))
	_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "operator", Patch: patch})
	var apiErr *api.APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, "attachment_route_changed", apiErr.Code)
	stored, err := store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.NotContains(t, stored.Metadata, "attachment.file")
}

func TestAttachmentRouteFenceAllowsUnchangedRoute(t *testing.T) {
	store := openClaimGateHelperDB(t)
	project, issue := createClaimGateHelperIssue(t, store)
	binding := db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "operator"}
	observed, err := store.UpsertFederationBinding(t.Context(), binding)
	require.NoError(t, err)
	patch := map[string]jsontext.Value{"attachment.file": []byte(`{"name":"file.pdf","media_type":"application/pdf","size":7,"docbank_version":"6f1c2e8a-2222-4222-8222-222222222222","origin":"01ARZ3NDEKTSV4RRFFQ69G5FAV"}`)}
	ctx := db.WithMetadataPatchHook(t.Context(), attachmentPatchHook(patch, &observed, nil, "01ARZ3NDEKTSV4RRFFQ69G5FAV", nil))
	_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "operator", Patch: patch})
	require.NoError(t, err)
	stored, err := store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Contains(t, stored.Metadata, "attachment.file")
}
