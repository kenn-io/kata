package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/json/jsontext"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func executeAttachmentCLIWithErrorOutput(t *testing.T, base string, rootFlags []string, args ...string) (string, string, error) {
	t.Helper()
	commandArgs := append([]string(nil), rootFlags...)
	commandArgs = append(commandArgs, "--project", "spoke-project", "docbank", "attachments")
	commandArgs = append(commandArgs, args...)
	return executeRootCapture(t, contextWithBaseURL(context.Background(), base), commandArgs...)
}

func attachmentCLIEnv(t *testing.T) (*testenv.Env, *docbanktest.Fake, string) {
	t.Helper()
	t.Setenv("KATA_AUTHOR", "operator")
	f := docbanktest.New(t)
	c, err := docbank.NewClient(f.Server.URL, func() (string, error) { return "test-key", nil })
	require.NoError(t, err)
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
	out, err := executeAttachmentCLI(t, env.URL, "add", ref, file, file, "--json")
	require.NoError(t, err)
	var results []struct {
		AttachmentID string `json:"attachment_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &struct {
		Results any `json:"results"`
	}{Results: &results}))
	require.Len(t, results, 2)
	require.NotEqual(t, results[0].AttachmentID, results[1].AttachmentID)
	out, err = executeAttachmentCLI(t, env.URL, "ls", ref)
	require.NoError(t, err)
	require.Contains(t, out, "screen.png")
	require.Contains(t, out, "12")
	id := results[0].AttachmentID
	suffix := id[len(id)-8:]
	dest := filepath.Join(dir, "download.png")
	_, err = executeAttachmentCLI(t, env.URL, "get", ref, suffix, "-o", dest)
	require.NoError(t, err)
	b, err := os.ReadFile(dest) //nolint:gosec // G304: destination is an isolated test fixture.
	require.NoError(t, err)
	require.Equal(t, "sample bytes", string(b))
	_, err = executeAttachmentCLI(t, env.URL, "get", ref, suffix, "-o", dest)
	require.Error(t, err)
	_, err = executeAttachmentCLI(t, env.URL, "get", ref, suffix, "-o", dest, "--force")
	require.NoError(t, err)
	link := filepath.Join(dir, "link.png")
	require.NoError(t, os.Symlink(dest, link))
	_, err = executeAttachmentCLI(t, env.URL, "get", ref, suffix, "-o", link)
	require.Error(t, err)
	out, err = executeAttachmentCLI(t, env.URL, "rm", ref, suffix, "--json")
	require.NoError(t, err)
	var removed struct {
		AttachmentID string `json:"attachment_id"`
		Removed      bool   `json:"removed"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &removed))
	require.Equal(t, id, removed.AttachmentID)
	require.True(t, removed.Removed)
	_, err = executeAttachmentCLI(t, env.URL, "get", ref, suffix)
	require.Error(t, err)
}

func TestAttachmentCLIOutputModes(t *testing.T) {
	env, _, ref := attachmentCLIEnv(t)
	file := filepath.Join(t.TempDir(), "agent-note.txt")
	content := []byte("attachment text")
	require.NoError(t, os.WriteFile(file, content, 0600))
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])

	out, err := executeAttachmentCLIWithRootFlags(t, env.URL, []string{"--json"}, "add", ref, file)
	require.NoError(t, err)
	var added struct {
		Results []struct {
			AttachmentID string `json:"attachment_id"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &added))
	require.Len(t, added.Results, 1)
	attachmentID := added.Results[0].AttachmentID

	out, err = executeAttachmentCLIWithRootFlags(t, env.URL, []string{"--agent"}, "add", ref, file)
	require.NoError(t, err)
	require.Contains(t, out, "OK docbank-attachments-add count=1")
	require.Contains(t, out, "attachment_id=")
	require.NotContains(t, out, `"kata_api_version"`)

	out, err = executeAttachmentCLIWithRootFlags(t, env.URL, []string{"--agent"}, "ls", ref)
	require.NoError(t, err)
	require.Contains(t, out, "OK docbank-attachments-ls count=2")
	require.Contains(t, out, "name=agent-note.txt")
	require.NotContains(t, out, `"kata_api_version"`)

	out, err = executeAttachmentCLIWithRootFlags(t, env.URL, []string{"--json"}, "ls", ref)
	require.NoError(t, err)
	var listed struct {
		KataAPIVersion int                        `json:"kata_api_version"`
		Attachments    map[string]json.RawMessage `json:"attachments"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &listed))
	require.Equal(t, 1, listed.KataAPIVersion)
	require.Len(t, listed.Attachments, 2)
	require.Contains(t, listed.Attachments, attachmentID)

	jsonPath := filepath.Join(t.TempDir(), "json-download.txt")
	out, err = executeAttachmentCLIWithRootFlags(t, env.URL, []string{"--json"}, "get", ref, attachmentID, "-o", jsonPath)
	require.NoError(t, err)
	var downloaded struct {
		AttachmentID string `json:"attachment_id"`
		Path         string `json:"path"`
		Size         int64  `json:"size"`
		SHA256       string `json:"sha256"`
		Downloaded   bool   `json:"downloaded"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &downloaded))
	require.Equal(t, attachmentID, downloaded.AttachmentID)
	require.Equal(t, jsonPath, downloaded.Path)
	require.EqualValues(t, len(content), downloaded.Size)
	require.Equal(t, sha, downloaded.SHA256)
	require.True(t, downloaded.Downloaded)

	agentPath := filepath.Join(t.TempDir(), "agent-download.txt")
	out, err = executeAttachmentCLIWithRootFlags(t, env.URL, []string{"--agent"}, "get", ref, attachmentID, "-o", agentPath)
	require.NoError(t, err)
	require.Contains(t, out, "OK docbank-attachments-get attachment_id="+attachmentID)
	require.Contains(t, out, "path="+agentValue(agentPath))
	require.Contains(t, out, "size=15 sha256="+sha)

	out, err = executeAttachmentCLIWithRootFlags(t, env.URL, []string{"--agent"}, "rm", ref, attachmentID)
	require.NoError(t, err)
	require.Contains(t, out, "OK docbank-attachments-rm attachment_id="+attachmentID)
	require.NotContains(t, out, `"kata_api_version"`)
}

func TestAttachmentCLIDaemonErrorsKeepCLIClassification(t *testing.T) {
	env, _, ref := attachmentCLIEnv(t)
	file := filepath.Join(t.TempDir(), "remote-error.txt")
	require.NoError(t, os.WriteFile(file, []byte("attachment text"), 0600))
	out, err := executeAttachmentCLIWithRootFlags(t, env.URL, []string{"--json"}, "add", ref, file)
	require.NoError(t, err)
	var added struct {
		Results []struct {
			AttachmentID string `json:"attachment_id"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &added))
	require.Len(t, added.Results, 1)
	id := added.Results[0].AttachmentID

	tests := []struct {
		name        string
		method      string
		suffix      string
		status      int
		body        string
		args        []string
		wantKind    errKind
		wantCode    string
		wantExit    int
		wantHint    string
		wantMessage string
	}{
		{
			name: "remove not found", method: http.MethodDelete, suffix: "/attachments/" + id,
			status: http.StatusNotFound,
			body:   `{"error":{"code":"attachment_not_found","message":"attachment not found","hint":"refresh the issue"}}`,
			args:   []string{"rm", ref, id}, wantKind: kindNotFound, wantCode: "attachment_not_found", wantExit: ExitNotFound,
			wantHint: "refresh the issue", wantMessage: "attachment not found",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = transport })
			intercepted := false
			http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == test.method && strings.HasSuffix(r.URL.Path, test.suffix) {
					intercepted = true
					return &http.Response{
						StatusCode:    test.status,
						Header:        http.Header{"Content-Type": []string{"application/json"}},
						Body:          io.NopCloser(strings.NewReader(test.body)),
						ContentLength: int64(len(test.body)),
						Request:       r,
					}, nil
				}
				return transport.RoundTrip(r)
			})
			stdout, stderr, err := executeAttachmentCLIWithErrorOutput(t, env.URL, []string{"--json"}, test.args...)
			require.True(t, intercepted, "test must replace the attachment daemon response")
			require.Error(t, err)
			require.Empty(t, stdout)
			cli := requireCLIError(t, err, test.wantExit)
			require.Equal(t, test.wantKind, cli.Kind)
			require.Equal(t, test.wantCode, cli.Code)
			require.Equal(t, test.wantHint, cli.Hint)
			require.Equal(t, test.wantMessage, cli.Message)
			envelope := parseErrorEnvelope(t, []byte(stderr))
			require.Equal(t, string(test.wantKind), envelope.Error.Kind)
			require.Equal(t, test.wantCode, envelope.Error.Code)
			require.Equal(t, test.wantExit, envelope.Error.ExitCode)
		})
	}
}

func TestAttachmentCLIInputErrorsKeepCLIClassification(t *testing.T) {
	env, _, ref := attachmentCLIEnv(t)
	file := filepath.Join(t.TempDir(), "input-error.txt")
	require.NoError(t, os.WriteFile(file, []byte("attachment text"), 0600))
	out, err := executeAttachmentCLIWithRootFlags(t, env.URL, []string{"--json"}, "add", ref, file)
	require.NoError(t, err)
	var added struct {
		Results []struct {
			AttachmentID string `json:"attachment_id"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &added))
	require.Len(t, added.Results, 1)
	id := added.Results[0].AttachmentID

	existing := filepath.Join(t.TempDir(), "existing.txt")
	require.NoError(t, os.WriteFile(existing, []byte("keep"), 0600))
	tests := []struct {
		name     string
		args     []string
		wantKind errKind
		wantExit int
		message  string
	}{
		{name: "missing attachment", args: []string{"rm", ref, "missing"}, wantKind: kindNotFound, wantExit: ExitNotFound, message: "attachment not found"},
		{name: "existing destination", args: []string{"get", ref, id, "-o", existing}, wantKind: kindConflict, wantExit: ExitConflict, message: "destination already exists (use --force)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stdout, stderr, err := executeAttachmentCLIWithErrorOutput(t, env.URL, []string{"--json"}, test.args...)
			require.Error(t, err)
			require.Empty(t, stdout)
			cli := requireCLIError(t, err, test.wantExit)
			require.Equal(t, test.wantKind, cli.Kind)
			require.Equal(t, test.message, cli.Message)
			envelope := parseErrorEnvelope(t, []byte(stderr))
			require.Equal(t, string(test.wantKind), envelope.Error.Kind)
			require.Equal(t, test.wantExit, envelope.Error.ExitCode)
			require.Equal(t, test.message, envelope.Error.Message)
		})
	}
}

func TestResolveAttachmentSuffixClassifiesMissingAndAmbiguous(t *testing.T) {
	tests := []struct {
		name     string
		ids      map[string]bool
		suffix   string
		wantKind errKind
		wantExit int
	}{
		{name: "empty", suffix: " ", wantKind: kindValidation, wantExit: ExitValidation},
		{name: "missing", ids: map[string]bool{}, suffix: "missing", wantKind: kindNotFound, wantExit: ExitNotFound},
		{name: "ambiguous", ids: map[string]bool{"01JB3Z6V2K8M4Q9T1X5W7Y0RNA": true, "01JB3Z6V2K8M4Q9T1X5W7Y1RNA": true}, suffix: "RNA", wantKind: kindConflict, wantExit: ExitConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := resolveAttachmentSuffix(test.ids, test.suffix)
			cli := requireCLIError(t, err, test.wantExit)
			require.Equal(t, test.wantKind, cli.Kind)
		})
	}
}

func TestAttachmentCLIQuietSuppressesMutationStatus(t *testing.T) {
	env, _, ref := attachmentCLIEnv(t)
	first := filepath.Join(t.TempDir(), "quiet-human.txt")
	second := filepath.Join(t.TempDir(), "quiet-agent.txt")
	require.NoError(t, os.WriteFile(first, []byte("human attachment"), 0600))
	require.NoError(t, os.WriteFile(second, []byte("agent attachment"), 0600))

	out, err := executeAttachmentCLIWithRootFlags(t, env.URL, []string{"--json"}, "add", ref, first)
	require.NoError(t, err)
	var added struct {
		Results []struct {
			AttachmentID string `json:"attachment_id"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &added))
	require.Len(t, added.Results, 1)
	humanID := added.Results[0].AttachmentID

	out, err = executeAttachmentCLIWithRootFlags(t, env.URL, []string{"--quiet"}, "add", ref, first)
	require.NoError(t, err)
	require.Empty(t, out, "quiet human add suppresses per-file mutation status")

	out, err = executeAttachmentCLIWithRootFlags(t, env.URL, []string{"--quiet", "--agent"}, "add", ref, second)
	require.NoError(t, err)
	require.NotContains(t, out, "OK docbank-attachments-add")
	require.Contains(t, out, "- file=")
	parts := strings.SplitN(out, "attachment_id=", 2)
	require.Len(t, parts, 2)
	agentID := strings.Fields(parts[1])[0]

	out, err = executeAttachmentCLIWithRootFlags(t, env.URL, []string{"--quiet"}, "rm", ref, humanID)
	require.NoError(t, err)
	require.Empty(t, out, "quiet human remove suppresses mutation status")

	out, err = executeAttachmentCLIWithRootFlags(t, env.URL, []string{"--quiet", "--agent"}, "rm", ref, agentID)
	require.NoError(t, err)
	require.Empty(t, out, "quiet agent remove suppresses its OK status line")
}

func TestAttachmentCLIAgentAddFailureReportsPerFileResults(t *testing.T) {
	env, _, ref := attachmentCLIEnv(t)
	file := filepath.Join(t.TempDir(), "upload.txt")
	require.NoError(t, os.WriteFile(file, []byte("attachment text"), 0600))

	out, err := executeAttachmentCLIWithRootFlags(t, env.URL, []string{"--agent"}, "add", ref, file, file+"-missing")
	require.Error(t, err)
	cli := requireCLIError(t, err, ExitValidation)
	require.Equal(t, kindValidation, cli.Kind)
	require.NotContains(t, out, "OK docbank-attachments-add")
	require.Equal(t, 2, strings.Count(out, "- file="))
	require.Contains(t, out, "attachment_id=")
	require.Contains(t, out, "error=")
}

func TestAttachmentCLIGetInvalidPeerEntryIsValidationError(t *testing.T) {
	env, _, ref := attachmentCLIEnv(t)
	file := filepath.Join(t.TempDir(), "invalid-peer.txt")
	require.NoError(t, os.WriteFile(file, []byte("attachment text"), 0600))
	out, err := executeAttachmentCLIWithRootFlags(t, env.URL, []string{"--json"}, "add", ref, file)
	require.NoError(t, err)
	var added struct {
		Results []struct {
			AttachmentID string `json:"attachment_id"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &added))
	require.Len(t, added.Results, 1)
	id := added.Results[0].AttachmentID

	project, err := env.DB.ProjectByName(t.Context(), "spoke-project")
	require.NoError(t, err)
	issue, err := env.DB.IssueByShortID(t.Context(), project.ID, ref, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{
		IssueID: issue.ID,
		Actor:   "operator",
		Patch: map[string]jsontext.Value{
			"attachments": jsontext.Value(`{"` + id + `":{"future":true}}`),
		},
	})
	require.NoError(t, err)

	stdout, stderr, err := executeAttachmentCLIWithErrorOutput(t, env.URL, []string{"--json"}, "get", ref, id)
	require.Error(t, err)
	require.Empty(t, stdout)
	cli := requireCLIError(t, err, ExitValidation)
	require.Equal(t, kindValidation, cli.Kind)
	envelope := parseErrorEnvelope(t, []byte(stderr))
	require.Equal(t, string(kindValidation), envelope.Error.Kind)
	require.Equal(t, ExitValidation, envelope.Error.ExitCode)
}

func TestAttachmentCLIDownloadAllowsBothMaximumTransferLegs(t *testing.T) {
	env, _, ref := attachmentCLIEnv(t)
	file := filepath.Join(t.TempDir(), "file.txt")
	require.NoError(t, os.WriteFile(file, []byte("ok"), 0600))
	out, err := executeAttachmentCLI(t, env.URL, "add", ref, file, "--json")
	require.NoError(t, err)
	var added struct {
		Results []struct {
			AttachmentID string `json:"attachment_id"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &added))
	require.Len(t, added.Results, 1)

	transport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = transport })
	var budget time.Duration
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.String(), env.URL+"/") && strings.HasSuffix(r.URL.Path, "/content") {
			deadline, ok := r.Context().Deadline()
			require.True(t, ok, "attachment transfers must remain bounded")
			budget = time.Until(deadline)
		}
		return transport.RoundTrip(r)
	})
	dest := filepath.Join(t.TempDir(), "download.txt")
	_, err = executeAttachmentCLI(t, env.URL, "get", ref, added.Results[0].AttachmentID, "-o", dest)
	require.NoError(t, err)
	// The daemon completes the upstream read before sending the verified body,
	// so a 2 GiB attachment can take 34m38s on each sequential transfer leg.
	require.GreaterOrEqual(t, budget, 74*time.Minute)
	data, err := os.ReadFile(dest) //nolint:gosec // G304: destination is an isolated test fixture.
	require.NoError(t, err)
	require.Equal(t, "ok", string(data))
}

func TestAttachmentCLIUploadAllowsBothMaximumTransferLegs(t *testing.T) {
	env, _, ref := attachmentCLIEnv(t)
	file := filepath.Join(t.TempDir(), "file.txt")
	require.NoError(t, os.WriteFile(file, []byte("ok"), 0600))

	transport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = transport })
	var budget time.Duration
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.String(), env.URL+"/") && strings.HasSuffix(r.URL.Path, "/attachments") {
			deadline, ok := r.Context().Deadline()
			require.True(t, ok, "attachment uploads must remain bounded")
			budget = time.Until(deadline)
		}
		return transport.RoundTrip(r)
	})
	_, err := executeAttachmentCLI(t, env.URL, "add", ref, file)
	require.NoError(t, err)
	// A maximum upload crosses the client-to-daemon and daemon-to-Docbank legs
	// sequentially. The CLI budget must cover both transfer windows plus overhead.
	require.GreaterOrEqual(t, budget, 74*time.Minute)
}

func TestAttachmentCLIPartialFailure(t *testing.T) {
	env, _, ref := attachmentCLIEnv(t)
	file := filepath.Join(t.TempDir(), "file.txt")
	require.NoError(t, os.WriteFile(file, []byte("ok"), 0600))
	out, err := executeAttachmentCLI(t, env.URL, "add", ref, file, file+"-missing")
	require.Error(t, err)
	require.Contains(t, out, "file.txt")
	require.Contains(t, out, "missing")
}
func TestAttachmentCLIIntegrityLeavesNoFile(t *testing.T) {
	env, f, ref := attachmentCLIEnv(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(file, bytes.Repeat([]byte("a"), 100000), 0600))
	out, err := executeAttachmentCLI(t, env.URL, "add", ref, file, "--json")
	require.NoError(t, err)
	var r []struct {
		AttachmentID string `json:"attachment_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &struct {
		Results any `json:"results"`
	}{Results: &r}))
	f.Fault = "trailer"
	dest := filepath.Join(dir, "download")
	_, err = executeAttachmentCLI(t, env.URL, "get", ref, r[0].AttachmentID, "-o", dest)
	require.Error(t, err)
	_, err = os.Lstat(dest)
	require.True(t, os.IsNotExist(err))
	files, _ := filepath.Glob(filepath.Join(dir, ".kata-attachment-*"))
	require.Empty(t, files)
}

func TestAttachmentCLIGetRejectsUnexpectedNonErrorStatus(t *testing.T) {
	env, _, ref := attachmentCLIEnv(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(file, []byte("ok"), 0600))
	out, err := executeAttachmentCLI(t, env.URL, "add", ref, file, "--json")
	require.NoError(t, err, out)
	var added struct {
		Results []struct {
			AttachmentID string `json:"attachment_id"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &added))
	require.Len(t, added.Results, 1)

	transport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = transport })
	intercepted := false
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/content") {
			intercepted = true
			return &http.Response{
				StatusCode:    http.StatusPartialContent,
				Header:        make(http.Header),
				Body:          io.NopCloser(strings.NewReader("ok")),
				ContentLength: 2,
				Request:       r,
			}, nil
		}
		return transport.RoundTrip(r)
	})

	dest := filepath.Join(dir, "download.txt")
	stdout, stderr, err := executeAttachmentCLIWithErrorOutput(t, env.URL, []string{"--json"}, "get", ref, added.Results[0].AttachmentID, "-o", dest)
	require.True(t, intercepted, "test must replace the attachment content response")
	require.Error(t, err, "get must reject a 206 response even when it carries complete verified content")
	require.Empty(t, stdout)
	cli := requireCLIError(t, err, ExitInternal)
	require.Equal(t, http.StatusPartialContent, cli.HTTPStatus)
	require.Equal(t, kindInternal, cli.Kind)
	envelope := parseErrorEnvelope(t, []byte(stderr))
	require.Equal(t, string(kindInternal), envelope.Error.Kind)
	require.Equal(t, ExitInternal, envelope.Error.ExitCode)
	_, err = os.Lstat(dest)
	require.True(t, os.IsNotExist(err), "unexpected response must not be reported as a completed download")
}

func TestAttachmentCLIStableRetry(t *testing.T) {
	env, _, ref := attachmentCLIEnv(t)
	file := filepath.Join(t.TempDir(), "file.txt")
	require.NoError(t, os.WriteFile(file, []byte("ok"), 0600))
	var ids []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/attachments") {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(b))
			require.NoError(t, r.ParseMultipartForm(1<<20))
			require.Equal(t, "operator", r.URL.Query().Get("actor"))
			require.Empty(t, r.MultipartForm.Value["actor"])
			ids = append(ids, r.FormValue("attachment_id"))
			r.Body = io.NopCloser(bytes.NewReader(b))
			if len(ids) == 1 {
				http.Error(w, "retry", http.StatusServiceUnavailable)
				return
			}
		}
		r.RequestURI = ""
		r.URL.Scheme = "http"
		r.URL.Host = strings.TrimPrefix(env.URL, "http://")
		resp, err := http.DefaultTransport.RoundTrip(r)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		maps.Copy(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer proxy.Close()
	out, err := executeAttachmentCLI(t, proxy.URL, "add", ref, file, "--json")
	require.NoError(t, err, out)
	require.Len(t, ids, 2)
	require.Equal(t, ids[0], ids[1])
}

func TestShowAttachmentSection(t *testing.T) {
	env, _, ref := attachmentCLIEnv(t)
	file := filepath.Join(t.TempDir(), "screen.png")
	require.NoError(t, os.WriteFile(file, []byte("ok"), 0600))
	_, err := executeAttachmentCLI(t, env.URL, "add", ref, file)
	require.NoError(t, err)
	resetFlags(t)
	cmd := newRootCmd()
	var b bytes.Buffer
	cmd.SetOut(&b)
	cmd.SetErr(&b)
	cmd.SetContext(contextWithBaseURL(t.Context(), env.URL))
	cmd.SetArgs([]string{"--project", "spoke-project", "show", ref})
	require.NoError(t, cmd.Execute())
	require.Contains(t, b.String(), "--- attachments ---")
	require.Contains(t, b.String(), "2 bytes")
}

func TestAttachmentReadersSkipInvalidPeerEntries(t *testing.T) {
	const id = "01JB3Z6V2K8M4Q9T1X5W7Y0RNA"
	m := metadata.Attachments{}
	require.NoError(t, m.Add(id, metadata.Attachment{Name: "screen.png", MediaType: "image/png", Size: 3, Docbank: metadata.DocbankReference{VaultUID: docbanktest.VaultUID, NodeID: 42, ContentVersionID: "6f1c2e8a-2222-4222-8222-222222222222", ContentSHA256: strings.Repeat("a", 64)}}))
	m["01JB3Z6V2K8M4Q9T1X5W7Y0RNB"] = jsontext.Value(`{}`)
	for _, mode := range []string{"human", "json", "agent", "show"} {
		t.Run(mode, func(t *testing.T) {
			var out bytes.Buffer
			switch mode {
			case "human":
				require.NoError(t, printAttachmentsCLI(&out, m, false))
			case "json":
				require.NoError(t, printAttachmentsCLI(&out, m, true))
				var result struct {
					Attachments map[string]json.RawMessage `json:"attachments"`
				}
				require.NoError(t, json.Unmarshal(out.Bytes(), &result))
				require.Len(t, result.Attachments, 1)
			case "agent":
				require.NoError(t, printAttachmentsAgentCLI(&out, m))
				require.Contains(t, out.String(), "count=1")
			case "show":
				require.NoError(t, printIssueAttachments(&out, m.Encode()))
			}
			require.Contains(t, out.String(), "screen.png")
			require.NotContains(t, out.String(), "01JB3Z6V2K8M4Q9T1X5W7Y0RNB")
			require.NotContains(t, out.String(), "invalid")
		})
	}
	m.Remove(id)
	var out bytes.Buffer
	require.NoError(t, printIssueAttachments(&out, m.Encode()))
	require.Empty(t, out.String())
}
