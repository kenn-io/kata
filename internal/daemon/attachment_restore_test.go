package daemon_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/testenv"
)

func TestRestoredAttachmentOrigin(t *testing.T) {
	source, vault := attachmentEnv(t)
	project, issue := seedProjectAndIssue(t, source)
	_, err := source.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	require.NoError(t, source.DB.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: source.DB.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", project.ID, issue.ShortID)
	code, body := attachmentUpload(t, source, endpoint, "image.png", strings.NewReader("image bytes"))
	require.Equal(t, http.StatusOK, code, string(body))
	var attachment struct {
		ID    string              `json:"attachment_id"`
		Entry metadata.Attachment `json:"attachment"`
	}
	require.NoError(t, json.Unmarshal(body, &attachment))
	foreign := attachment.Entry
	foreign.Origin = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	foreignRaw, err := json.Marshal(foreign)
	require.NoError(t, err)
	_, err = source.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "operator", Patch: map[string]jsontext.Value{"attachment.foreign": foreignRaw}})
	require.NoError(t, err)
	var backup bytes.Buffer
	require.NoError(t, jsonl.Export(t.Context(), source.DB, &backup, jsonl.ExportOptions{IncludeDeleted: true}))
	for _, standalone := range []bool{false, true} {
		t.Run(fmt.Sprintf("standalone=%t", standalone), func(t *testing.T) {
			target := testenv.New(t, func(cfg *daemon.ServerConfig) {
				cfg.Docbank = docbank.NewClient(vault.Server.URL, func() (string, error) { return "test-key", nil })
				cfg.DocbankFolder = "/kata"
			})
			require.NoError(t, jsonl.ImportWithOptions(t.Context(), bytes.NewReader(backup.Bytes()), target.DB, jsonl.ImportOptions{NewInstance: true, AsStandalone: standalone}))
			require.NotEqual(t, source.DB.InstanceUID(), target.DB.InstanceUID())
			status, raw := target.Get(t, endpoint+"/"+attachment.ID+"/content")
			require.Equal(t, http.StatusOK, status, string(raw))
			require.Equal(t, "image bytes", string(raw))
			status, raw = target.Get(t, endpoint+"/foreign/content")
			require.Equal(t, http.StatusGone, status, string(raw))
			client := docbank.NewClient(vault.Server.URL, func() (string, error) { return "test-key", nil })
			version, err := client.Version(t.Context(), attachment.Entry.DocbankVersion)
			require.NoError(t, err)
			status, raw, err = postAttachmentLink(t, target, endpoint+"/links", fmt.Sprintf(`{"node_id":%d,"reuse_existing":true,"actor":"operator"}`, version.NodeID))
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, status, string(raw))
			var reused api.AddAttachmentResponseBody
			require.NoError(t, json.Unmarshal(raw, &reused))
			require.Equal(t, attachment.ID, reused.ID)
			if !standalone {
				code, body := attachmentUpload(t, target, endpoint, "new.png", strings.NewReader("new bytes"))
				require.Equal(t, http.StatusOK, code, string(body))
				var uploaded api.AddAttachmentResponseBody
				require.NoError(t, json.Unmarshal(body, &uploaded))
				require.Equal(t, source.DB.InstanceUID(), uploaded.Attachment.Origin)
				status, raw, err = postAttachmentLink(t, target, endpoint+"/links", fmt.Sprintf(`{"node_id":%d,"actor":"operator"}`, version.NodeID))
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, status, string(raw))
				var linked api.AddAttachmentResponseBody
				require.NoError(t, json.Unmarshal(raw, &linked))
				require.Equal(t, source.DB.InstanceUID(), linked.Attachment.Origin)
			}
		})
	}
}

// A standalone copy retains its source identity as a former origin. Once the
// copy serves a project as a new hub, references must carry its own origin;
// once it is a replica again, the former origin belongs to the upstream.
func TestStandaloneCopyAttachmentOriginsAfterRejoining(t *testing.T) {
	source, vault := attachmentEnv(t)
	project, issue := seedProjectAndIssue(t, source)
	_, err := source.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	endpoint := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments", project.ID, issue.ShortID)
	code, body := attachmentUpload(t, source, endpoint, "image.png", strings.NewReader("image bytes"))
	require.Equal(t, http.StatusOK, code, string(body))
	var attachment api.AddAttachmentResponseBody
	require.NoError(t, json.Unmarshal(body, &attachment))
	require.Equal(t, source.DB.InstanceUID(), attachment.Attachment.Origin)
	var backup bytes.Buffer
	require.NoError(t, jsonl.Export(t.Context(), source.DB, &backup, jsonl.ExportOptions{IncludeDeleted: true}))
	restore := func(t *testing.T) *testenv.Env {
		target := testenv.New(t, func(cfg *daemon.ServerConfig) {
			cfg.Docbank = docbank.NewClient(vault.Server.URL, func() (string, error) { return "test-key", nil })
			cfg.DocbankFolder = "/kata"
		})
		require.NoError(t, jsonl.ImportWithOptions(t.Context(), bytes.NewReader(backup.Bytes()), target.DB, jsonl.ImportOptions{NewInstance: true, AsStandalone: true}))
		return target
	}

	t.Run("new hub links its own origin", func(t *testing.T) {
		target := restore(t)
		_, err := target.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
		require.NoError(t, err)
		version, err := docbank.NewClient(vault.Server.URL, func() (string, error) { return "test-key", nil }).Version(t.Context(), attachment.Attachment.DocbankVersion)
		require.NoError(t, err)
		status, raw, err := postAttachmentLink(t, target, endpoint+"/links", fmt.Sprintf(`{"node_id":%d,"reuse_existing":true,"actor":"operator"}`, version.NodeID))
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, status, string(raw))
		var linked api.AddAttachmentResponseBody
		require.NoError(t, json.Unmarshal(raw, &linked))
		require.NotEqual(t, attachment.ID, linked.ID, "a reference naming the former origin must not be reused")
		require.Equal(t, target.DB.InstanceUID(), linked.Attachment.Origin)
	})

	t.Run("replica reads the former origin upstream", func(t *testing.T) {
		target := restore(t)
		_, err := target.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Enabled: false})
		require.NoError(t, err)
		before := len(vault.Requests())
		status, raw := target.Get(t, endpoint+"/"+attachment.ID+"/content")
		require.Equal(t, http.StatusServiceUnavailable, status, string(raw))
		require.Contains(t, string(raw), "federation_offline")
		require.Len(t, vault.Requests(), before, "a replica must not read its former origin from the local vault")
	})
}
