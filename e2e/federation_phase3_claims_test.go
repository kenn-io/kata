//go:build !windows

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestFederationClaimsTwoSpokeLifecycle(t *testing.T) {
	ctx := context.Background()
	fx := setupFederationClaimE2E(t, "phase3-claims-lifecycle")

	issueA := waitForFederatedIssue(t, fx.spokeA.db, fx.hubIssue.UID, fx.spokeA.stderr)
	issueB := waitForFederatedIssue(t, fx.spokeB.db, fx.hubIssue.UID, fx.spokeB.stderr)

	aliceClaim := runKataJSON[e2eClaimActionBody](t, fx.bin, fx.spokeA.dirs,
		"--project", fx.hubProject.Name, "federation", "lease", "acquire", issueA.ShortID, "--as", "alice")
	require.True(t, aliceClaim.Granted)
	assert.False(t, aliceClaim.Pending)
	assert.Equal(t, "alice", aliceClaim.Holder.Holder)
	require.NotNil(t, aliceClaim.Claim)
	assert.Equal(t, "hard", aliceClaim.Claim.ClaimKind)

	bobDenied := runKataJSONWantExit[e2eClaimActionBody](t, fx.bin, fx.spokeB.dirs, 5,
		"--project", fx.hubProject.Name, "federation", "lease", "acquire", issueB.ShortID, "--as", "bob")
	assert.False(t, bobDenied.body.Granted)
	require.NotNil(t, bobDenied.body.Claim)
	assert.Equal(t, "alice", bobDenied.body.Claim.Holder)
	assert.Contains(t, bobDenied.stderr, `"code":"claim_denied"`)

	showB := runKataOK(t, fx.bin, fx.spokeB.dirs,
		"--project", fx.hubProject.Name, "show", issueB.ShortID)
	assert.Contains(t, showB.stdout, "lease: alice from instance ")
	assert.Contains(t, showB.stdout, "(hard)")

	runKataOK(t, fx.bin, fx.spokeA.dirs,
		"--project", fx.hubProject.Name, "edit", issueA.ShortID, "--title", "alice edit", "--as", "alice")

	bobEdit := runKataWantExit(t, fx.bin, fx.spokeB.dirs, 5,
		"--json", "--project", fx.hubProject.Name, "edit", issueB.ShortID, "--title", "bob rejected", "--as", "bob")
	assert.Contains(t, bobEdit.stderr, `"code":"claim_denied"`)

	released := runKataJSON[e2eClaimActionBody](t, fx.bin, fx.spokeA.dirs,
		"--project", fx.hubProject.Name, "federation", "lease", "release", issueA.ShortID, "--as", "alice")
	require.True(t, released.Granted)
	require.NotNil(t, released.Claim)
	require.NotNil(t, released.Claim.ReleasedAt)

	bobClaim := runKataJSON[e2eClaimActionBody](t, fx.bin, fx.spokeB.dirs,
		"--project", fx.hubProject.Name, "federation", "lease", "acquire", issueB.ShortID, "--as", "bob")
	require.True(t, bobClaim.Granted)
	assert.Equal(t, "bob", bobClaim.Holder.Holder)

	runKataOK(t, fx.bin, fx.spokeB.dirs,
		"--project", fx.hubProject.Name, "edit", issueB.ShortID, "--title", "bob edit", "--as", "bob")

	bobReleased := runKataJSON[e2eClaimActionBody](t, fx.bin, fx.spokeB.dirs,
		"--project", fx.hubProject.Name, "federation", "lease", "release", issueB.ShortID, "--as", "bob")
	require.True(t, bobReleased.Granted)

	runKataOK(t, fx.bin, fx.spokeB.dirs,
		"--project", fx.hubProject.Name, "edit", issueB.ShortID, "--title", "charlie edit", "--as", "charlie")

	waitForFoldedProjectionMatch(t, fx.hub.DB, fx.spokeA.db,
		fx.hubProject.ID, fx.spokeA.replica.Project.ID, fx.replayAfterID, fx.spokeA.stderr)
	waitForFoldedProjectionMatch(t, fx.hub.DB, fx.spokeB.db,
		fx.hubProject.ID, fx.spokeB.replica.Project.ID, fx.replayAfterID, fx.spokeB.stderr)

	finalHubIssue, err := fx.hub.DB.IssueByUID(ctx, fx.hubIssue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, "charlie edit", finalHubIssue.Title)
}

func TestFederationTimedClaimExpires(t *testing.T) {
	ctx := context.Background()
	fx := setupFederationClaimE2E(t, "phase3-claims-expiry")

	issueA := waitForFederatedIssue(t, fx.spokeA.db, fx.hubIssue.UID, fx.spokeA.stderr)
	issueB := waitForFederatedIssue(t, fx.spokeB.db, fx.hubIssue.UID, fx.spokeB.stderr)

	aliceClaim := runKataJSON[e2eClaimActionBody](t, fx.bin, fx.spokeA.dirs,
		"--project", fx.hubProject.Name, "federation", "lease", "acquire", issueA.ShortID, "--as", "alice", "--ttl", "60s")
	require.True(t, aliceClaim.Granted)
	require.NotNil(t, aliceClaim.Claim)
	require.NotNil(t, aliceClaim.Claim.ExpiresAt)
	assert.Equal(t, "timed", aliceClaim.Claim.ClaimKind)

	// Phase 3 has no automatic renewer. Drive the hub sweeper deterministically
	// past the CLI's minimum 60s TTL instead of sleeping a full minute.
	events, err := fx.hub.DB.ExpireTimedClaims(ctx, aliceClaim.Claim.ExpiresAt.Add(time.Second), 100)
	require.NoError(t, err)
	require.NotEmpty(t, events)

	bobClaim := runKataJSON[e2eClaimActionBody](t, fx.bin, fx.spokeB.dirs,
		"--project", fx.hubProject.Name, "federation", "lease", "acquire", issueB.ShortID, "--as", "bob")
	require.True(t, bobClaim.Granted)
	assert.Equal(t, "bob", bobClaim.Holder.Holder)
	require.NotNil(t, bobClaim.Claim)
	assert.Equal(t, "hard", bobClaim.Claim.ClaimKind)
}

func TestFederationAdminForceReleaseAndStealCLI(t *testing.T) {
	fx := setupFederationClaimE2E(t, "phase4b-admin-claim")

	issueA := waitForFederatedIssue(t, fx.spokeA.db, fx.hubIssue.UID, fx.spokeA.stderr)
	issueB := waitForFederatedIssue(t, fx.spokeB.db, fx.hubIssue.UID, fx.spokeB.stderr)
	hubDirs := hubClaimCLIDirs(t, fx)

	aliceClaim := runKataJSON[e2eClaimActionBody](t, fx.bin, fx.spokeA.dirs,
		"--project", fx.hubProject.Name, "federation", "lease", "acquire", issueA.ShortID, "--as", "alice")
	require.True(t, aliceClaim.Granted)

	spokeForceRelease := runKataWantExit(t, fx.bin, fx.spokeA.dirs, 5,
		"--json", "--project", fx.hubProject.Name, "federation", "lease", "force-release", issueA.ShortID,
		"--as", "admin", "--reason", "stale holder")
	assert.Contains(t, spokeForceRelease.stderr, "federated_read_only")

	released := runKataJSON[e2eClaimActionBody](t, fx.bin, hubDirs,
		"--project", fx.hubProject.Name, "federation", "lease", "force-release", fx.hubIssue.ShortID,
		"--as", "admin", "--reason", "stale holder")
	require.True(t, released.Granted)
	require.NotNil(t, released.Claim)
	assert.Equal(t, "alice", released.Claim.Holder)
	require.NotNil(t, released.Claim.ReleasedAt)

	bobClaim := runKataJSON[e2eClaimActionBody](t, fx.bin, fx.spokeB.dirs,
		"--project", fx.hubProject.Name, "federation", "lease", "acquire", issueB.ShortID, "--as", "bob")
	require.True(t, bobClaim.Granted)

	stolen := runKataJSON[e2eClaimStealBody](t, fx.bin, hubDirs,
		"--project", fx.hubProject.Name, "federation", "lease", "steal", fx.hubIssue.ShortID,
		"--as", "carol", "--reason", "operator handoff")
	assert.Equal(t, "bob", stolen.ReleasedHolder)
	assert.Equal(t, "carol", stolen.NewHolder)
	require.NotNil(t, stolen.Released.Claim)
	assert.Equal(t, "bob", stolen.Released.Claim.Holder)
	require.NotNil(t, stolen.Claimed.Claim)
	assert.Equal(t, "carol", stolen.Claimed.Claim.Holder)
}

type federationClaimFixture struct {
	bin           string
	hub           *testenv.Env
	hubProject    db.Project
	hubIssue      db.Issue
	replayAfterID int64
	spokeA        federationClaimSpoke
	spokeB        federationClaimSpoke
}

type federationClaimSpoke struct {
	dirs    e2eDirs
	url     string
	http    *http.Client
	db      *db.DB
	stderr  *safeBuffer
	replica api.CreateFederationReplicaBody
}

func setupFederationClaimE2E(t *testing.T, name string) federationClaimFixture {
	t.Helper()
	ctx := context.Background()
	hub := testenv.New(t)
	bin := buildKataBinary(t)

	spokeA := startFederationClaimSpoke(ctx, t, bin)
	spokeB := startFederationClaimSpoke(ctx, t, bin)

	hubProject, err := hub.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	hubIssue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: hubProject.ID,
		Title:     name,
		Body:      "shared claim lifecycle target",
		Author:    "agent",
	})
	require.NoError(t, err)

	var meta api.ProjectFederationBody
	decodePOST(t, hub.HTTP, hub.URL+"/api/v1/projects/"+strconv.FormatInt(hubProject.ID, 10)+"/federation/enable",
		map[string]any{"actor": "agent"}, &meta)

	spokeA.replica = enrollFederationClaimSpoke(ctx, t, hub, hubProject, meta, spokeA, name+"-a")
	spokeB.replica = enrollFederationClaimSpoke(ctx, t, hub, hubProject, meta, spokeB, name+"-b")

	return federationClaimFixture{
		bin:           bin,
		hub:           hub,
		hubProject:    hubProject,
		hubIssue:      hubIssue,
		replayAfterID: meta.ReplayHorizonEventID - 1,
		spokeA:        spokeA,
		spokeB:        spokeB,
	}
}

func startFederationClaimSpoke(ctx context.Context, t *testing.T, bin string) federationClaimSpoke {
	t.Helper()
	dirs := newE2EDirs(t)
	stderr := startDaemon(t, bin, append(dirs.env(), "KATA_FEDERATION_PULL_INTERVAL_MS=25"))
	url, client := connectDaemon(t, dirs, stderr)
	store, err := db.Open(ctx, dirs.dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return federationClaimSpoke{dirs: dirs, url: url, http: client, db: store, stderr: stderr}
}

func enrollFederationClaimSpoke(
	ctx context.Context,
	t *testing.T,
	hub *testenv.Env,
	hubProject db.Project,
	meta api.ProjectFederationBody,
	spoke federationClaimSpoke,
	token string,
) api.CreateFederationReplicaBody {
	t.Helper()
	created, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token:            token,
		SpokeInstanceUID: spoke.db.InstanceUID(),
		ProjectID:        &hubProject.ID,
		Capabilities:     "pull,push,claim",
	})
	require.NoError(t, err)

	var replica api.CreateFederationReplicaBody
	decodePOST(t, spoke.http, spoke.url+"/api/v1/federation/replicas", map[string]any{
		"hub_url":                 hub.URL,
		"hub_project_id":          hubProject.ID,
		"hub_project_uid":         meta.ProjectUID,
		"project_name":            meta.ProjectName,
		"replay_horizon_event_id": meta.ReplayHorizonEventID,
		"token":                   created.Token,
		"capabilities":            "pull,push,claim",
		"push_enabled":            true,
	}, &replica)
	require.True(t, replica.Binding.PushEnabled)
	return replica
}

type e2eClaimRun struct {
	stdout string
	stderr string
}

type e2eClaimActionBody struct {
	Granted bool `json:"granted"`
	Pending bool `json:"pending"`
	Holder  struct {
		Holder            string `json:"holder"`
		HolderInstanceUID string `json:"holder_instance_uid"`
		ClientKind        string `json:"client_kind"`
	} `json:"holder"`
	Claim *struct {
		Holder            string     `json:"holder"`
		HolderInstanceUID string     `json:"holder_instance_uid"`
		ClaimKind         string     `json:"claim_kind"`
		ExpiresAt         *time.Time `json:"expires_at"`
		ReleasedAt        *time.Time `json:"released_at"`
	} `json:"claim"`
}

type e2eClaimStealBody struct {
	ReleasedHolder string             `json:"released_holder"`
	NewHolder      string             `json:"new_holder"`
	Released       e2eClaimActionBody `json:"released"`
	Claimed        e2eClaimActionBody `json:"claimed"`
}

func hubClaimCLIDirs(t *testing.T, fx federationClaimFixture) e2eDirs {
	t.Helper()
	dirs := newE2EDirs(t)
	localConfig := fmt.Sprintf("version = 1\n\n[server]\nurl = %q\nallow_insecure = true\n", fx.hub.URL)
	require.NoError(t, os.WriteFile(filepath.Join(dirs.repoDir, ".kata.local.toml"), []byte(localConfig), 0o600))
	return dirs
}

func runKataJSON[T any](t *testing.T, bin string, dirs e2eDirs, args ...string) T {
	t.Helper()
	res := runKataOK(t, bin, dirs, append([]string{"--json"}, args...)...)
	var out T
	require.NoErrorf(t, json.Unmarshal([]byte(res.stdout), &out), "stdout: %s\nstderr: %s", res.stdout, res.stderr)
	return out
}

func runKataJSONWantExit[T any](t *testing.T, bin string, dirs e2eDirs, wantExit int, args ...string) struct {
	body   T
	stderr string
} {
	t.Helper()
	res := runKataWantExit(t, bin, dirs, wantExit, append([]string{"--json"}, args...)...)
	var out T
	require.NoErrorf(t, json.Unmarshal([]byte(res.stdout), &out), "stdout: %s\nstderr: %s", res.stdout, res.stderr)
	return struct {
		body   T
		stderr string
	}{body: out, stderr: res.stderr}
}

func runKataOK(t *testing.T, bin string, dirs e2eDirs, args ...string) e2eClaimRun {
	t.Helper()
	res, exitCode := runKataWithSQLiteBusyRetry(t, bin, dirs, args...)
	require.Equalf(t, 0, exitCode, "kata %s\nstdout: %s\nstderr: %s", strings.Join(args, " "), res.stdout, res.stderr)
	return res
}

func runKataWantExit(t *testing.T, bin string, dirs e2eDirs, wantExit int, args ...string) e2eClaimRun {
	t.Helper()
	res, exitCode := runKata(t, bin, dirs, args...)
	require.Equalf(t, wantExit, exitCode, "kata %s\nstdout: %s\nstderr: %s", strings.Join(args, " "), res.stdout, res.stderr)
	return res
}

func runKataWithSQLiteBusyRetry(t *testing.T, bin string, dirs e2eDirs, args ...string) (e2eClaimRun, int) {
	t.Helper()
	var res e2eClaimRun
	var exitCode int
	deadline := time.Now().Add(5 * time.Second)
	for attempt := 0; ; attempt++ {
		res, exitCode = runKata(t, bin, dirs, args...)
		if exitCode == 0 || !claimE2ESQLiteBusy(res) || time.Now().After(deadline) {
			return res, exitCode
		}
		// These e2e tests run foreground CLI commands against the same daemon DB
		// that background federation runners are polling. CI can occasionally hit
		// SQLite's short busy window; retry only success-expected commands.
		time.Sleep(time.Duration(attempt+1) * 50 * time.Millisecond)
	}
}

func claimE2ESQLiteBusy(res e2eClaimRun) bool {
	return isSQLiteBusyMessage(res.stdout + "\n" + res.stderr)
}

func runKata(t *testing.T, bin string, dirs e2eDirs, args ...string) (e2eClaimRun, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // test-built binary and test-controlled args
	cmd.Dir = dirs.repoDir
	cmd.Env = append(dirs.env(), "KATA_HTTP_TIMEOUT=10s")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := e2eClaimRun{stdout: stdout.String(), stderr: stderr.String()}
	if ctx.Err() != nil {
		t.Fatalf("kata %s timed out\nstdout: %s\nstderr: %s", strings.Join(args, " "), out.stdout, out.stderr)
	}
	if err == nil {
		return out, 0
	}
	var exitErr *exec.ExitError
	if !assert.ErrorAs(t, err, &exitErr) {
		t.Fatalf("kata %s failed to start: %v\nstdout: %s\nstderr: %s",
			strings.Join(args, " "), err, out.stdout, out.stderr)
	}
	return out, exitErr.ExitCode()
}

func (r e2eClaimRun) String() string {
	return fmt.Sprintf("stdout: %s\nstderr: %s", r.stdout, r.stderr)
}
