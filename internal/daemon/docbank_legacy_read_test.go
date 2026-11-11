package daemon_test

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/docbank/docbanktest"
	"go.kenn.io/kata/internal/testenv"
)

type unavailableFormerOriginStore struct{ db.Storage }

func (s unavailableFormerOriginStore) FormerInstanceUIDs(context.Context) ([]string, error) {
	return nil, errors.New("synthetic former origin read failure")
}

func TestLegacyAttachmentReadDoesNotRequireFormerOrigins(t *testing.T) {
	vault := docbanktest.New(t)
	version := uuid.MustParse(linkVersion)
	vault.Seed("/kata/file.pdf", docbank.Node{ID: 42, Name: "file.pdf", Kind: "file", CurrentVersionID: version}, map[uuid.UUID][]byte{version: []byte("fixture")})
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.DB = unavailableFormerOriginStore{Storage: cfg.DB}
		cfg.Docbank = docbank.NewClient(vault.Server.URL, func() (string, error) { return "test-key", nil })
	})
	project, issue := seedProjectAndIssue(t, env)
	_, err := env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "operator", Patch: map[string]jsontext.Value{"attachment.file": []byte(`{"name":"file.pdf","media_type":"application/pdf","size":7,"docbank_version":"` + linkVersion + `"}`)}})
	require.NoError(t, err)
	status, body := env.Get(t, fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments/file/content", project.ID, issue.UID))
	require.Equal(t, http.StatusOK, status, string(body))
	require.Equal(t, "fixture", string(body))
}
