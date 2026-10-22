package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/docbank/docbanktest"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/testenv"
)

func executeAttachmentCLI(t *testing.T, base string, args ...string) (string, error) {
	return executeAttachmentCLIWithRootFlags(t, base, nil, args...)
}

func executeAttachmentCLIWithRootFlags(t *testing.T, base string, rootFlags []string, args ...string) (string, error) {
	t.Helper()
	resetFlags(t)
	cmd := newRootCmd()
	var b bytes.Buffer
	cmd.SetOut(&b)
	cmd.SetErr(&b)
	cmd.SetContext(contextWithBaseURL(context.Background(), base))
	commandArgs := append([]string(nil), rootFlags...)
	commandArgs = append(commandArgs, "--project", "spoke-project", "docbank", "attachments")
	cmd.SetArgs(append(commandArgs, args...))
	err := cmd.Execute()
	return b.String(), err
}

func attachmentCLIEnv(t *testing.T) (*testenv.Env, *docbanktest.Fake, string) {
	t.Helper()
	t.Setenv("KATA_AUTHOR", "operator")
	f := docbanktest.New(t)
	c := docbank.NewClient(f.Server.URL, func() (string, error) { return "test-key", nil })
	env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.Docbank = c })
	p, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	iss, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Attachments", Author: "operator"})
	require.NoError(t, err)
	return env, f, iss.ShortID
}
func TestAttachmentCLIWorkflow(t *testing.T) {
	env, _, ref := attachmentCLIEnv(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "screen.png")
	require.NoError(t, os.WriteFile(file, []byte("sample bytes"), 0600))
	out, err := executeAttachmentCLIWithRootFlags(t, env.URL, []string{"--agent"}, "add", ref, file)
	require.NoError(t, err)
	require.Contains(t, out, "attachment_id=")
	out, err = executeAttachmentCLI(t, env.URL, "add", ref, file, file, "--json")
	require.NoError(t, err)
	var added struct {
		Results []struct {
			ID string `json:"attachment_id"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &added))
	require.Len(t, added.Results, 2)
	id := added.Results[0].ID
	other := added.Results[1].ID
	p, err := env.DB.ProjectByName(t.Context(), "spoke-project")
	require.NoError(t, err)
	iss, err := env.DB.IssueByShortID(t.Context(), p.ID, ref, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: iss.ID, Actor: "operator", Patch: map[string]jsontext.Value{"attachment.x": jsontext.Value(`"junk"`), "work.branch": jsontext.Value(`"feature"`)}})
	require.NoError(t, err)
	out, err = executeAttachmentCLI(t, env.URL, "ls", ref)
	require.NoError(t, err)
	require.Contains(t, out, "screen.png")
	require.NotContains(t, out, "junk")
	out, err = executeAttachmentCLIWithRootFlags(t, env.URL, []string{"--agent"}, "ls", ref)
	require.NoError(t, err)
	require.Contains(t, out, "count=3")
	out, err = executeAttachmentCLI(t, env.URL, "ls", ref, "--json")
	require.NoError(t, err)
	var listed struct {
		Attachments map[string]metadata.Attachment `json:"attachments"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &listed))
	require.Len(t, listed.Attachments, 3)
	require.NotContains(t, out, "junk")
	suffix := id[len(id)-8:]
	dest := filepath.Join(dir, "download.png")
	_, err = executeAttachmentCLI(t, env.URL, "get", ref, suffix, "-o", dest)
	require.NoError(t, err)
	b, err := os.ReadFile(dest) //nolint:gosec // Destination is inside t.TempDir.
	require.NoError(t, err)
	require.Equal(t, "sample bytes", string(b))
	require.NoError(t, os.WriteFile(dest, []byte("existing"), 0600))
	_, err = executeAttachmentCLI(t, env.URL, "get", ref, suffix, "-o", dest)
	require.Error(t, err)
	b, err = os.ReadFile(dest) //nolint:gosec // Destination is inside t.TempDir.
	require.NoError(t, err)
	require.Equal(t, "existing", string(b))
	_, err = executeAttachmentCLI(t, env.URL, "get", ref, suffix, "-o", dest, "--force")
	require.NoError(t, err)
	b, err = os.ReadFile(dest) //nolint:gosec // Destination is inside t.TempDir.
	require.NoError(t, err)
	require.Equal(t, "sample bytes", string(b))
	_, err = executeAttachmentCLI(t, env.URL, "rm", ref, suffix, "--json")
	require.NoError(t, err)
	out, err = executeAttachmentCLI(t, env.URL, "ls", ref, "--json")
	require.NoError(t, err)
	listed.Attachments = nil
	require.NoError(t, json.Unmarshal([]byte(out), &listed))
	require.Len(t, listed.Attachments, 2)
	require.Contains(t, listed.Attachments, other)
	for key := range listed.Attachments {
		_, err = executeAttachmentCLI(t, env.URL, "rm", ref, key)
		require.NoError(t, err)
	}
	iss, err = env.DB.IssueByID(t.Context(), iss.ID)
	require.NoError(t, err)
	require.JSONEq(t, `{"attachment.x":"junk","work.branch":"feature"}`, string(iss.Metadata))
}
func TestAttachmentCLIRawUpload(t *testing.T) {
	file := filepath.Join(t.TempDir(), "shot.png")
	require.NoError(t, os.WriteFile(file, []byte("image"), 0600))
	c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "shot.png", r.URL.Query().Get("name"))
		require.Equal(t, "operator", r.URL.Query().Get("actor"))
		require.EqualValues(t, 5, r.ContentLength)
		data, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, "image", string(data))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"attachment_id":"saved"}`)), Header: http.Header{"Content-Type": {"application/json"}}}, nil
	})}
	id, err := uploadAttachmentCLI(t.Context(), c, "https://daemon.example", 7, "abc4", "operator", file)
	require.NoError(t, err)
	require.Equal(t, "saved", id)
}
func TestAttachmentCLIDownloadSizeFailurePreservesDestination(t *testing.T) {
	for _, body := range []string{"short", "too long"} {
		for _, force := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/force=%t", body, force), func(t *testing.T) {
				c := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
				})}
				dest := filepath.Join(t.TempDir(), "download.png")
				if force {
					require.NoError(t, os.WriteFile(dest, []byte("existing"), 0600))
				}
				err := downloadAttachmentCLI(t.Context(), c, "https://daemon.example/content", metadata.Attachment{Size: 6}, dest, force)
				require.Error(t, err)
				if force {
					data, err := os.ReadFile(dest) //nolint:gosec // Destination is inside t.TempDir.
					require.NoError(t, err)
					require.Equal(t, "existing", string(data))
				} else {
					_, err := os.Stat(dest)
					require.ErrorIs(t, err, os.ErrNotExist)
				}
			})
		}
	}
}
