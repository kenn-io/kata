package daemon_test

import (
	"bytes"
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
	closeURL := fmt.Sprintf("/api/v1/projects/%d/issues/%s/actions/close", project.ID, issue.ShortID)
	closeInput := map[string]any{"actor": "operator", "reason": "done", "message": attachmentCloseTestMessage, "evidence": []any{map[string]any{"type": "attachment", "attachment_id": attachment.ID}}}
	closed, raw := envDoRaw(t, source, http.MethodPost, closeURL, closeInput, nil)
	require.Equal(t, http.StatusOK, closed.StatusCode, string(raw))
	closeResult := api.MutationResponse{}.Body
	require.NoError(t, json.Unmarshal(raw, &closeResult))
	require.NotNil(t, closeResult.Event)
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
			var nextBackup bytes.Buffer
			require.NoError(t, jsonl.Export(t.Context(), target.DB, &nextBackup, jsonl.ExportOptions{IncludeDeleted: true}))
			second := testenv.New(t, func(cfg *daemon.ServerConfig) {
				cfg.Docbank = docbank.NewClient(vault.Server.URL, func() (string, error) { return "test-key", nil })
				cfg.DocbankFolder = "/kata"
			})
			require.NoError(t, jsonl.ImportWithOptions(t.Context(), bytes.NewReader(nextBackup.Bytes()), second.DB, jsonl.ImportOptions{NewInstance: true}))
			status, raw = second.Get(t, endpoint+"/"+attachment.ID+"/content")
			require.Equal(t, http.StatusOK, status, string(raw))
			require.Equal(t, "image bytes", string(raw))
			status, raw = target.Get(t, endpoint+"/foreign/content")
			require.Equal(t, http.StatusGone, status, string(raw))
			events, err := target.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
			require.NoError(t, err)
			var found bool
			for _, event := range events {
				if event.UID == closeResult.Event.UID {
					require.Equal(t, closeResult.Event.Payload, event.Payload)
					found = true
				}
			}
			require.True(t, found)
			reopen, raw := envDoRaw(t, target, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues/%s/actions/reopen", project.ID, issue.ShortID), map[string]any{"actor": "operator"}, nil)
			require.Equal(t, http.StatusOK, reopen.StatusCode, string(raw))
			client := docbank.NewClient(vault.Server.URL, func() (string, error) { return "test-key", nil })
			version, err := client.Version(t.Context(), attachment.Entry.DocbankVersion)
			require.NoError(t, err)
			status, raw, err = postAttachmentLink(t, target, endpoint+"/links", fmt.Sprintf(`{"node_id":%d,"reuse_existing":true,"actor":"operator"}`, version.NodeID))
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, status, string(raw))
			var reused api.AddAttachmentResponseBody
			require.NoError(t, json.Unmarshal(raw, &reused))
			require.Equal(t, attachment.ID, reused.ID)
		})
	}
}
