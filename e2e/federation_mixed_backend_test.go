//go:build !windows

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

type federationMatrixBackend string

const (
	federationMatrixSQLite   federationMatrixBackend = "sqlite"
	federationMatrixPostgres federationMatrixBackend = "postgres"
	federationMatrixProject                          = "mixed-federation"
)

type federationMatrixNode struct {
	bin     string
	dirs    e2eDirs
	env     []string
	url     string
	stderr  *safeBuffer
	backend federationMatrixBackend
	cmd     *exec.Cmd
}

type federationMatrixMutation struct {
	Issue   db.Issue `json:"issue"`
	Changed bool     `json:"changed"`
	Reused  bool     `json:"reused"`
}

type federationMatrixShow struct {
	Issue    db.Issue `json:"issue"`
	Comments []struct {
		Author string `json:"author"`
		Body   string `json:"body"`
	} `json:"comments"`
	Labels []struct {
		Label string `json:"label"`
	} `json:"labels"`
}

type federationMatrixList struct {
	Issues []api.IssueOut `json:"issues"`
}

type federationMatrixEnrollment struct {
	Enrollment api.FederationEnrollmentOut `json:"enrollment"`
	Join       struct {
		HubURL                 string `json:"hub_url"`
		HubProjectID           int64  `json:"hub_project_id"`
		HubProjectUID          string `json:"hub_project_uid"`
		ProjectName            string `json:"project_name"`
		ReplayHorizonEventID   int64  `json:"replay_horizon_event_id"`
		BaselineThroughEventID int64  `json:"baseline_through_event_id"`
		Token                  string `json:"token"`
		Actor                  string `json:"actor"`
	} `json:"join"`
}

func TestFederationMixedBackendMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("mixed-backend federation requires PostgreSQL")
	}

	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	bin := buildKataBinary(t)

	tests := []struct {
		name         string
		hubBackend   federationMatrixBackend
		spokeBackend federationMatrixBackend
	}{
		{name: "sqlite-to-sqlite", hubBackend: federationMatrixSQLite, spokeBackend: federationMatrixSQLite},
		{name: "sqlite-to-postgres", hubBackend: federationMatrixSQLite, spokeBackend: federationMatrixPostgres},
		{name: "postgres-to-sqlite", hubBackend: federationMatrixPostgres, spokeBackend: federationMatrixSQLite},
		{name: "postgres-to-postgres", hubBackend: federationMatrixPostgres, spokeBackend: federationMatrixPostgres},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hub := startFederationMatrixNode(t, bin, tc.hubBackend, dsn,
				fmt.Sprintf("matrix_%d_hub", i))
			spoke := startFederationMatrixNode(t, bin, tc.spokeBackend, dsn,
				fmt.Sprintf("matrix_%d_spoke", i))

			federationMatrixRunOK(t, hub, "--project", federationMatrixProject, "init")
			baseline := federationMatrixRunJSON[federationMatrixMutation](t, hub,
				"--project", federationMatrixProject, "--as", "hub-agent", "create", "hub baseline",
				"--body", "created on the hub", "--label", "baseline")
			require.NotEmpty(t, baseline.Issue.UID)

			identity := federationMatrixRunJSON[struct {
				InstanceUID string `json:"instance_uid"`
			}](t, spoke, "federation", "identity")
			require.NotEmpty(t, identity.InstanceUID)

			enrollment := federationMatrixRunJSON[federationMatrixEnrollment](t, hub,
				"federation", "enroll", federationMatrixProject,
				"--spoke-instance", identity.InstanceUID,
				"--hub-url", hub.url,
				"--allow-insecure",
				"--capabilities", "pull,push,lease",
				"--actor", "spoke-agent")
			require.NotEmpty(t, enrollment.Join.Token)
			require.Equal(t, hub.url, enrollment.Join.HubURL)

			federationMatrixRunOK(t, spoke,
				"--project", federationMatrixProject,
				"federation", "join",
				"--hub-url", enrollment.Join.HubURL,
				"--hub-project-id", strconv.FormatInt(enrollment.Join.HubProjectID, 10),
				"--hub-project-uid", enrollment.Join.HubProjectUID,
				"--replay-horizon", strconv.FormatInt(enrollment.Join.ReplayHorizonEventID, 10),
				"--baseline-through", strconv.FormatInt(enrollment.Join.BaselineThroughEventID, 10),
				"--token", enrollment.Join.Token,
				"--capabilities", "pull,push,lease",
				"--actor", enrollment.Join.Actor,
				"--allow-insecure",
				"--push")

			spokeBaseline := waitForFederationMatrixIssue(t, spoke, baseline.Issue.UID,
				func(show federationMatrixShow) bool { return show.Issue.Title == "hub baseline" })
			assert.Equal(t, "created on the hub", spokeBaseline.Issue.Body)
			assert.Equal(t, []string{"baseline"}, federationMatrixLabelNames(spokeBaseline))

			federationMatrixRunOK(t, hub,
				"--project", federationMatrixProject, "--as", "hub-agent",
				"edit", baseline.Issue.UID,
				"--title", "hub updated after join",
				"--comment", "pulled after the initial snapshot")
			spokeBaseline = waitForFederationMatrixIssue(t, spoke, baseline.Issue.UID,
				func(show federationMatrixShow) bool {
					return show.Issue.Title == "hub updated after join" &&
						federationMatrixHasComment(show, "pulled after the initial snapshot")
				})
			assert.Equal(t, "hub-agent", federationMatrixCommentAuthor(spokeBaseline,
				"pulled after the initial snapshot"))

			pushed := federationMatrixRunJSON[federationMatrixMutation](t, spoke,
				"--project", federationMatrixProject, "--as", "spoke-agent", "create", "spoke pushed issue",
				"--body", "created on the spoke", "--owner", "spoke-owner", "--label", "mixed",
				"--meta", "origin=spoke", "--idempotency-key", tc.name+"-create")
			replayed := federationMatrixRunJSON[federationMatrixMutation](t, spoke,
				"--project", federationMatrixProject, "--as", "spoke-agent", "create", "spoke pushed issue",
				"--body", "created on the spoke", "--owner", "spoke-owner", "--label", "mixed",
				"--meta", "origin=spoke", "--idempotency-key", tc.name+"-create")
			assert.Equal(t, pushed.Issue.UID, replayed.Issue.UID)
			assert.True(t, replayed.Reused)

			hubPushed := waitForFederationMatrixIssue(t, hub, pushed.Issue.UID,
				func(show federationMatrixShow) bool { return show.Issue.Title == "spoke pushed issue" })
			assert.Equal(t, "created on the spoke", hubPushed.Issue.Body)
			require.NotNil(t, hubPushed.Issue.Owner)
			assert.Equal(t, "spoke-owner", *hubPushed.Issue.Owner)
			assert.JSONEq(t, `{"origin":"spoke"}`, string(hubPushed.Issue.Metadata))
			assert.Equal(t, []string{"mixed"}, federationMatrixLabelNames(hubPushed))

			spoke = restartFederationMatrixNode(t, spoke)
			federationMatrixRunOK(t, spoke,
				"--project", federationMatrixProject, "--as", "spoke-agent",
				"federation", "lease", "acquire", baseline.Issue.UID)
			federationMatrixRunOK(t, spoke,
				"--project", federationMatrixProject, "--as", "spoke-agent",
				"edit", baseline.Issue.UID, "--body", "edited on the restarted spoke under a lease")
			waitForFederationMatrixIssue(t, hub, baseline.Issue.UID,
				func(show federationMatrixShow) bool {
					return show.Issue.Body == "edited on the restarted spoke under a lease"
				})
			federationMatrixRunOK(t, spoke,
				"--project", federationMatrixProject, "--as", "spoke-agent",
				"federation", "lease", "release", baseline.Issue.UID)

			federationMatrixRunOK(t, hub,
				"--project", federationMatrixProject, "--as", "hub-agent",
				"comment", pushed.Issue.UID, "--body", "hub reply on spoke issue")
			waitForFederationMatrixIssue(t, spoke, pushed.Issue.UID,
				func(show federationMatrixShow) bool {
					return federationMatrixHasComment(show, "hub reply on spoke issue")
				})
			afterRestart := federationMatrixRunJSON[federationMatrixMutation](t, spoke,
				"--project", federationMatrixProject, "--as", "spoke-agent", "create", "pushed after restart",
				"--body", "cursor and credential state survived restart")
			waitForFederationMatrixIssue(t, hub, afterRestart.Issue.UID,
				func(show federationMatrixShow) bool { return show.Issue.Title == "pushed after restart" })

			hubList := federationMatrixRunJSON[federationMatrixList](t, hub,
				"--project", federationMatrixProject, "list", "--status", "all")
			spokeList := federationMatrixRunJSON[federationMatrixList](t, spoke,
				"--project", federationMatrixProject, "list", "--status", "all")
			assert.Equal(t, federationMatrixIssueSummary(hubList), federationMatrixIssueSummary(spokeList))
			assert.NotContains(t, strings.ToLower(hub.stderr.String()), "panic")
			assert.NotContains(t, strings.ToLower(spoke.stderr.String()), "panic")
		})
	}
}

func startFederationMatrixNode(
	t *testing.T,
	bin string,
	backend federationMatrixBackend,
	dsn string,
	schema string,
) federationMatrixNode {
	t.Helper()
	dirs := newE2EDirs(t)
	overrides := map[string]string{
		"KATA_FEDERATION_PULL_INTERVAL_MS": "25",
		"KATA_HTTP_TIMEOUT":                "15s",
	}
	switch backend {
	case federationMatrixSQLite:
		overrides["KATA_DB"] = dirs.dbPath
		overrides["KATA_DSN"] = ""
		overrides["KATA_POSTGRES_SCHEMA"] = ""
		overrides["KATA_POSTGRES_SCHEMA_MODE"] = ""
	case federationMatrixPostgres:
		overrides["KATA_DB"] = ""
		overrides["KATA_DSN"] = dsn
		overrides["KATA_POSTGRES_SCHEMA"] = schema
		overrides["KATA_POSTGRES_SCHEMA_MODE"] = "bootstrap"
	default:
		require.FailNow(t, "unsupported federation matrix backend", string(backend))
	}
	env := federationMatrixOverlayEnv(dirs.env(), overrides)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	node := federationMatrixNode{
		bin: bin, dirs: dirs, env: env, url: "http://" + addr, stderr: &safeBuffer{}, backend: backend,
	}
	return launchFederationMatrixNode(t, node)
}

func restartFederationMatrixNode(t *testing.T, node federationMatrixNode) federationMatrixNode {
	t.Helper()
	stopDaemon(node.cmd)
	return launchFederationMatrixNode(t, node)
}

func launchFederationMatrixNode(t *testing.T, node federationMatrixNode) federationMatrixNode {
	t.Helper()
	addr := strings.TrimPrefix(node.url, "http://")
	cmd := exec.Command(node.bin, "daemon", "start", "--foreground", "--listen", addr) //nolint:gosec // test-built binary and loopback address
	cmd.Env = node.env
	cmd.Dir = node.dirs.repoDir
	cmd.Stdout = io.Discard
	cmd.Stderr = node.stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())
	node.cmd = cmd
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("%s daemon stderr:\n%s", node.backend, node.stderr.String())
		}
		stopDaemon(cmd)
	})

	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, requestErr := client.Get(node.url + "/api/v1/ping") //nolint:gosec,noctx // loopback test daemon
		if requestErr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return node
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.FailNowf(t, "daemon did not become ready", "%s backend stderr:\n%s", node.backend, node.stderr.String())
	return federationMatrixNode{}
}

func federationMatrixOverlayEnv(base []string, overrides map[string]string) []string {
	out := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[key]; !replaced {
			out = append(out, entry)
		}
	}
	for key, value := range overrides {
		out = append(out, key+"="+value)
	}
	return out
}

type federationMatrixCommandResult struct {
	stdout   string
	stderr   string
	exitCode int
}

func federationMatrixRunOK(t *testing.T, node federationMatrixNode, args ...string) federationMatrixCommandResult {
	t.Helper()
	result := federationMatrixRunCommand(t, node, args...)
	require.Equalf(t, 0, result.exitCode, "%s kata %s\nstdout: %s\nstderr: %s",
		node.backend, strings.Join(args, " "), result.stdout, result.stderr)
	return result
}

func federationMatrixRunJSON[T any](t *testing.T, node federationMatrixNode, args ...string) T {
	t.Helper()
	result := federationMatrixRunOK(t, node, append([]string{"--json"}, args...)...)
	var value T
	require.NoErrorf(t, json.Unmarshal([]byte(result.stdout), &value),
		"%s kata %s\nstdout: %s\nstderr: %s", node.backend, strings.Join(args, " "), result.stdout, result.stderr)
	return value
}

func federationMatrixRunCommand(t *testing.T, node federationMatrixNode, args ...string) federationMatrixCommandResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node.bin, args...) //nolint:gosec // test-built binary and fixed test arguments
	cmd.Dir = node.dirs.repoDir
	cmd.Env = node.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		require.FailNowf(t, "kata command timed out", "%s kata %s\nstdout: %s\nstderr: %s",
			node.backend, strings.Join(args, " "), stdout.String(), stderr.String())
	}
	result := federationMatrixCommandResult{stdout: stdout.String(), stderr: stderr.String()}
	if err == nil {
		return result
	}
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	result.exitCode = exitErr.ExitCode()
	return result
}

func waitForFederationMatrixIssue(
	t *testing.T,
	node federationMatrixNode,
	ref string,
	ready func(federationMatrixShow) bool,
) federationMatrixShow {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last federationMatrixCommandResult
	for time.Now().Before(deadline) {
		last = federationMatrixRunCommand(t, node,
			"--json", "--project", federationMatrixProject, "show", ref)
		if last.exitCode == 0 {
			var show federationMatrixShow
			if err := json.Unmarshal([]byte(last.stdout), &show); err == nil && ready(show) {
				return show
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.FailNowf(t, "federated issue did not converge",
		"%s ref=%s\nstdout: %s\nstderr: %s\ndaemon stderr: %s",
		node.backend, ref, last.stdout, last.stderr, node.stderr.String())
	return federationMatrixShow{}
}

func federationMatrixHasComment(show federationMatrixShow, body string) bool {
	return federationMatrixCommentAuthor(show, body) != ""
}

func federationMatrixCommentAuthor(show federationMatrixShow, body string) string {
	for _, comment := range show.Comments {
		if comment.Body == body {
			return comment.Author
		}
	}
	return ""
}

func federationMatrixLabelNames(show federationMatrixShow) []string {
	labels := make([]string, 0, len(show.Labels))
	for _, label := range show.Labels {
		labels = append(labels, label.Label)
	}
	return labels
}

type federationMatrixIssueState struct {
	Title  string
	Status string
	Owner  string
	Labels string
}

func federationMatrixIssueSummary(list federationMatrixList) map[string]federationMatrixIssueState {
	summary := make(map[string]federationMatrixIssueState, len(list.Issues))
	for _, issue := range list.Issues {
		owner := ""
		if issue.Owner != nil {
			owner = *issue.Owner
		}
		labels := append([]string(nil), issue.Labels...)
		slices.Sort(labels)
		summary[issue.UID] = federationMatrixIssueState{
			Title: issue.Title, Status: issue.Status, Owner: owner, Labels: strings.Join(labels, ","),
		}
	}
	return summary
}
