package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	katauid "go.kenn.io/kata/internal/uid"
)

func TestFederationStatusJSONOutput(t *testing.T) {
	env, project := setupFederationStatusCLIState(t)

	out := requireCmdOutput(t, env, "--json", "federation", "status")

	var got struct {
		KataAPIVersion int `json:"kata_api_version"`
		Statuses       []struct {
			ProjectID                int64   `json:"project_id"`
			ProjectName              string  `json:"project_name"`
			Role                     string  `json:"role"`
			Enabled                  bool    `json:"enabled"`
			PushEnabled              bool    `json:"push_enabled"`
			PullCursorEventID        int64   `json:"pull_cursor_event_id"`
			PushCursorEventID        int64   `json:"push_cursor_event_id"`
			PendingPushCount         int64   `json:"pending_push_count"`
			PendingClaimCount        int64   `json:"pending_claim_count"`
			LiveClaimCount           int64   `json:"live_claim_count"`
			ActiveQuarantineCount    int64   `json:"active_quarantine_count"`
			ResetBlocker             string  `json:"reset_blocker,omitempty"`
			UnresolvedViolationCount int64   `json:"unresolved_violation_count"`
			RecentViolationCount     int64   `json:"recent_violation_count"`
			LastSuccessfulSyncAt     *string `json:"last_successful_sync_at,omitempty"`
			LastError                *string `json:"last_error,omitempty"`
		} `json:"statuses"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Equal(t, 1, got.KataAPIVersion)
	require.Len(t, got.Statuses, 1)
	status := got.Statuses[0]
	assert.Equal(t, project.ID, status.ProjectID)
	assert.Equal(t, "spoke-cli", status.ProjectName)
	assert.Equal(t, "spoke", status.Role)
	assert.True(t, status.Enabled)
	assert.True(t, status.PushEnabled)
	assert.Equal(t, int64(12), status.PullCursorEventID)
	assert.Equal(t, int64(0), status.PushCursorEventID)
	assert.Equal(t, int64(1), status.PendingPushCount)
	assert.Equal(t, int64(1), status.PendingClaimCount)
	assert.Equal(t, int64(0), status.LiveClaimCount)
	assert.Equal(t, int64(1), status.ActiveQuarantineCount)
	assert.Equal(t, "quarantine", status.ResetBlocker)
	assert.Equal(t, int64(0), status.UnresolvedViolationCount)
	assert.Equal(t, int64(0), status.RecentViolationCount)
	require.NotNil(t, status.LastSuccessfulSyncAt)
	assert.Contains(t, *status.LastSuccessfulSyncAt, "2026-05-23T12:05:00")
	require.NotNil(t, status.LastError)
	assert.Equal(t, "hub offline", *status.LastError)
}

func TestFederationStatusTextOutputIncludesOperatorFields(t *testing.T) {
	env, _ := setupFederationStatusCLIState(t)

	out := requireCmdOutput(t, env, "federation", "status")

	for _, want := range []string{
		"spoke-cli",
		"role: spoke",
		"enabled: true",
		"push-enabled: true",
		"pull cursor: 12",
		"push cursor: 0",
		"pending push: 1",
		"last successful sync: 2026-05-23T12:05:00Z",
		"last error: 2026-05-23T12:07:00Z hub offline",
		"live leases: 0",
		"pending leases: 1",
		"active quarantine: 1",
		"reset blocker: quarantine",
		"quarantine #",
		"unresolved violations: 0",
		"recent violations: 0",
	} {
		assert.Contains(t, out, want)
	}
}

func TestFederationStatusIncludesRecentClaimViolations(t *testing.T) {
	env, _, pid, ref := setupFederatedHubIssue(t, "status violation")
	ctx := context.Background()
	issue, err := env.DB.IssueByShortID(ctx, pid, ref, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = env.DB.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: pid,
		IssueRef:  ref,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: cliViolationSpokeUID,
			Holder:            "holder",
			ClientKind:        "cli",
		},
		ClaimKind: "hard",
		Now:       time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	ingestCLIClaimViolation(t, env, pid, issue, "bob", "issue.updated", 30)

	out := requireCmdOutput(t, env, "--json", "federation", "status")

	var got struct {
		Statuses []struct {
			UnresolvedViolationCount int64 `json:"unresolved_violation_count"`
			RecentViolationCount     int64 `json:"recent_violation_count"`
			RecentViolations         []struct {
				ShortID                    string    `json:"short_id"`
				OffendingEventType         string    `json:"offending_event_type"`
				OffendingOriginInstanceUID string    `json:"offending_origin_instance_uid"`
				Actor                      string    `json:"actor"`
				Reason                     string    `json:"reason"`
				At                         time.Time `json:"at"`
			} `json:"recent_violations"`
		} `json:"statuses"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got.Statuses, 1)
	status := got.Statuses[0]
	assert.Equal(t, int64(1), status.UnresolvedViolationCount)
	assert.Equal(t, int64(1), status.RecentViolationCount)
	require.Len(t, status.RecentViolations, 1)
	assert.Equal(t, ref, status.RecentViolations[0].ShortID)
	assert.Equal(t, "issue.updated", status.RecentViolations[0].OffendingEventType)
	assert.Equal(t, cliViolationSpokeUID, status.RecentViolations[0].OffendingOriginInstanceUID)
	assert.Equal(t, "bob", status.RecentViolations[0].Actor)
	assert.Equal(t, "uncovered_work", status.RecentViolations[0].Reason)
	assert.False(t, status.RecentViolations[0].At.IsZero())

	text := requireCmdOutput(t, env, "federation", "status")
	assert.Contains(t, text, "unresolved violations: 1")
	assert.Contains(t, text, "recent violations: 1")
	assert.Contains(t, text, ref+" issue.updated by bob on spoke "+cliViolationSpokeUID)
}

func TestFederationQuarantineSkipCLI(t *testing.T) {
	env, project := setupFederationStatusCLIState(t)
	ctx := context.Background()
	q, err := env.DB.ActiveFederationQuarantine(ctx, project.ID, db.FederationQuarantineDirectionPush)
	require.NoError(t, err)

	out := requireCmdOutput(t, env, "federation", "quarantine", "skip", strconv.FormatInt(q.ID, 10),
		"--confirm", "SKIP FEDERATION BATCH "+strconv.FormatInt(q.ID, 10),
		"--reason", "operator accepted skip")

	assert.Contains(t, out, fmt.Sprintf("quarantine #%d skipped", q.ID))
	binding, err := env.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, q.LastEventID, binding.PushCursorEventID)
}

func TestFederationHelpIsVisible(t *testing.T) {
	rootHelp := string(executeRoot(t, newRootCmd(), "--help"))
	assert.Contains(t, strings.ToLower(rootHelp), "federation")

	out, err := runCmdOutput(t, nil, "federation", "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "status")
	assert.Contains(t, out, "identity")
	assert.Contains(t, out, "enable")
	assert.Contains(t, out, "enroll")
	assert.Contains(t, out, "enrollments")
	assert.Contains(t, out, "join")
	assert.Contains(t, out, "revoke")
}

func TestFederationStatusInvisibilityNonFederatedShowUnchanged(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	short := createIssue(t, env, pid, "ordinary issue")

	out := runCLI(t, env, dir, "show", short)

	assert.Contains(t, out, short+"  ordinary issue  [open]  by tester")
	assertNoFederationInternals(t, out)
}

func TestFederationIdentityCLIShowsInstanceUID(t *testing.T) {
	env := testenv.New(t)

	out := requireCmdOutput(t, env, "federation", "identity")

	assert.Contains(t, out, "instance: "+env.DB.InstanceUID())
}

func TestFederationEnableCLIEnablesWorkspaceProject(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)

	out := runCLI(t, env, dir, "federation", "enable")

	assert.Contains(t, out, "enabled federation for kata")
	binding, err := env.DB.FederationBindingByProject(context.Background(), pid)
	require.NoError(t, err)
	assert.Equal(t, db.FederationRoleHub, binding.Role)
}

func TestFederationEnableCLIResolvesExplicitProjectFlag(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(context.Background(), "fedlab")
	require.NoError(t, err)

	out := requireCmdOutput(t, env, "federation", "enable", "--project", "fedlab")

	assert.Contains(t, out, "enabled federation for fedlab")
	binding, err := env.DB.FederationBindingByProject(context.Background(), project.ID)
	require.NoError(t, err)
	assert.Equal(t, db.FederationRoleHub, binding.Role)
}

func TestFederationEnableCLIRejectsSpokeProject(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, "spoke")
	require.NoError(t, err)
	_, err = env.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7787",
		HubProjectID:         42,
		HubProjectUID:        "01HZNQ7VFPK1XGD8R5MABCD4EG",
		ReplayHorizonEventID: 7,
		Enabled:              true,
	})
	require.NoError(t, err)

	_, err = runCmdOutput(t, env, "federation", "enable", "--project", "spoke")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "spoke")
}

func TestFederationEnrollCLIPrintsJoinCommand(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	runCLI(t, env, dir, "federation", "enable")
	spokeUID := "01HZNQ7VFPK1XGD8R5MABCD4EF"
	savedArgs := os.Args
	os.Args = []string{"/opt/kata-fedlab"}
	t.Cleanup(func() { os.Args = savedArgs })

	out := runCLI(t, env, dir, "federation", "enroll",
		"--spoke-instance", spokeUID,
		"--hub-url", "http://100.64.0.5:7787")

	assert.Contains(t, out, "enrolled "+spokeUID+" for kata")
	assert.Contains(t, out, "kata-fedlab federation join")
	assert.NotContains(t, out, "/opt/kata-fedlab federation join")
	assert.NotContains(t, out, "join: kata federation join")
	assert.Contains(t, out, "--hub-url http://100.64.0.5:7787")
	assert.Contains(t, out, "--hub-project-id "+strconv.FormatInt(pid, 10))
	assert.Contains(t, out, "--project kata")
	assert.NotContains(t, out, "--hub-project-uid")
	assert.NotContains(t, out, "--replay-horizon")
	assert.NotContains(t, out, "--baseline-through")
	assert.Contains(t, out, "--push")
	assert.Contains(t, out, "--token ")
}

func TestFederationEnrollCLIRequiresPullCapabilityForJoinCommand(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)

	_, err := runCLICapture(t, env, dir, "federation", "enroll",
		"--spoke-instance", "01HZNQ7VFPK1XGD8R5MABCD4EF",
		"--hub-url", "http://127.0.0.1:7787",
		"--capabilities", "lease")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "pull")
}

func TestFederationEnrollCLIUsesResolvedActorWhenAutoEnabling(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)

	runCLI(t, env, dir, "--as", "alice", "federation", "enroll",
		"--spoke-instance", "01HZNQ7VFPK1XGD8R5MABCD4EF",
		"--hub-url", "http://100.64.0.5:7787")

	events, err := env.DB.EventsAfter(context.Background(), db.EventsAfterParams{
		ProjectID: pid,
		Limit:     100,
	})
	require.NoError(t, err)
	require.NotEmpty(t, events)
	assert.Equal(t, "project.federation_enabled", events[0].Type)
	assert.Equal(t, "alice", events[0].Actor)
}

func TestFederationJoinCLIRequiresPullCapability(t *testing.T) {
	env := testenv.New(t)

	_, err := runCmdOutput(t, env, "federation", "join",
		"--project", "fedlab",
		"--hub-url", "http://127.0.0.1:7787",
		"--hub-project-id", "42",
		"--hub-project-uid", "01HZNQ7VFPK1XGD8R5MABCD4EG",
		"--replay-horizon", "7",
		"--token", "join-token",
		"--capabilities", "lease")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "pull")
}

func TestFederationJoinCLIRequiresPushCapabilityWhenPushEnabled(t *testing.T) {
	env := testenv.New(t)

	_, err := runCmdOutput(t, env, "federation", "join",
		"--project", "fedlab",
		"--hub-url", "http://127.0.0.1:7787",
		"--hub-project-id", "42",
		"--hub-project-uid", "01HZNQ7VFPK1XGD8R5MABCD4EG",
		"--replay-horizon", "7",
		"--token", "join-token",
		"--capabilities", "pull,lease",
		"--push")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "push")
}

func TestFederationJoinCLIAdoptExistingRequiresPush(t *testing.T) {
	env := testenv.New(t)

	_, err := runCmdOutput(t, env, "federation", "join",
		"--project", "fedlab",
		"--hub-url", "http://127.0.0.1:7787",
		"--hub-project-id", "42",
		"--hub-project-uid", "01HZNQ7VFPK1XGD8R5MABCD4EG",
		"--replay-horizon", "7",
		"--token", "join-token",
		"--adopt-existing")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--adopt-existing requires --push")
}

func TestFederationJoinCLIAdoptExistingRequiresPushCapability(t *testing.T) {
	env := testenv.New(t)

	_, err := runCmdOutput(t, env, "federation", "join",
		"--project", "fedlab",
		"--hub-url", "http://127.0.0.1:7787",
		"--hub-project-id", "42",
		"--hub-project-uid", "01HZNQ7VFPK1XGD8R5MABCD4EG",
		"--replay-horizon", "7",
		"--token", "join-token",
		"--adopt-existing",
		"--push",
		"--capabilities", "pull,lease")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "push")
}

func TestFederationJoinCLICreatesPushEnabledReplicaAndCredential(t *testing.T) {
	env := testenv.New(t)
	hubProjectUID := "01HZNQ7VFPK1XGD8R5MABCD4EG"

	out := requireCmdOutput(t, env, "federation", "join",
		"--project", "fedlab",
		"--hub-url", "http://100.64.0.5:7787",
		"--hub-project-id", "42",
		"--hub-project-uid", hubProjectUID,
		"--replay-horizon", "7",
		"--baseline-through", "9",
		"--token", "join-token",
		"--push")

	assert.Contains(t, out, "joined federation project fedlab")
	project, err := env.DB.ProjectByUID(context.Background(), hubProjectUID)
	require.NoError(t, err)
	assert.Equal(t, "fedlab", project.Name)
	binding, err := env.DB.FederationBindingByProject(context.Background(), project.ID)
	require.NoError(t, err)
	assert.Equal(t, db.FederationRoleSpoke, binding.Role)
	assert.True(t, binding.PushEnabled)
	assert.Equal(t, int64(42), binding.HubProjectID)
	creds, err := config.ReadFederationCredentials()
	require.NoError(t, err)
	assert.Equal(t, "join-token", creds.Projects[project.UID].Token)
	assert.Equal(t, "claim,pull,push", creds.Projects[project.UID].Capabilities)
}

func TestFederationJoinCLIAdoptExistingOutput(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, "fedlab")
	require.NoError(t, err)
	_, _, err = env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "local issue",
		Author:    "tester",
	})
	require.NoError(t, err)
	hubProjectUID := "01HZNQ7VFPK1XGD8R5MABCD4EG"

	out := requireCmdOutput(t, env, "federation", "join",
		"--project", "fedlab",
		"--hub-url", "http://100.64.0.5:7787",
		"--hub-project-id", "42",
		"--hub-project-uid", hubProjectUID,
		"--replay-horizon", "7",
		"--baseline-through", "9",
		"--token", "join-token",
		"--push",
		"--adopt-existing")

	assert.Contains(t, out, "adopted existing project fedlab into federation")
	assert.Contains(t, out, "queued 1 issue snapshots for hub push; pre-adoption local event history was removed")
	assert.Contains(t, out, "future edits remain local-first; acquire leases only for exclusive coordination")
	assert.NotContains(t, out, "require hub leases before edits")
}

func TestFederationJoinCLIAgentOutputIncludesAdoptionFields(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, "fedlab")
	require.NoError(t, err)
	_, _, err = env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "local issue",
		Author:    "tester",
	})
	require.NoError(t, err)
	hubProjectUID := "01HZNQ7VFPK1XGD8R5MABCD4EG"

	out := requireCmdOutput(t, env, "--agent", "federation", "join",
		"--project", "fedlab",
		"--hub-url", "http://100.64.0.5:7787",
		"--hub-project-id", "42",
		"--hub-project-uid", hubProjectUID,
		"--replay-horizon", "7",
		"--baseline-through", "9",
		"--token", "join-token",
		"--push",
		"--adopt-existing")

	assert.Contains(t, out, "adopted=true")
	assert.Contains(t, out, "adoption_snapshots=1")
}

func TestFederationJoinCLIFetchesMissingHubMetadata(t *testing.T) {
	hub := testenv.New(t)
	spoke := testenv.New(t)
	ctx := context.Background()
	hubProject, err := hub.DB.CreateProject(ctx, "fedlab")
	require.NoError(t, err)
	_, err = hub.DB.EnableProjectFederation(ctx, hubProject.ID, "tester")
	require.NoError(t, err)
	created, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "metadata-token",
		SpokeInstanceUID: spoke.DB.InstanceUID(),
		ProjectID:        &hubProject.ID,
		Capabilities:     "pull,push,claim",
	})
	require.NoError(t, err)

	out := requireCmdOutput(t, spoke, "federation", "join",
		"--project", "fedlab",
		"--hub-url", hub.URL,
		"--hub-project-id", strconv.FormatInt(hubProject.ID, 10),
		"--token", created.Token,
		"--push")

	assert.Contains(t, out, "joined federation project fedlab")
	project, err := spoke.DB.ProjectByUID(ctx, hubProject.UID)
	require.NoError(t, err)
	binding, err := spoke.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, hubProject.ID, binding.HubProjectID)
	assert.Equal(t, db.FederationRoleSpoke, binding.Role)
	assert.True(t, binding.PushEnabled)
}

func TestFederationJoinCLIWarnsWhenPushCapabilityIsNotEnabledLocally(t *testing.T) {
	env := testenv.New(t)

	stdout, stderr, err := runCmdCapture(t, env, "federation", "join",
		"--project", "fedlab",
		"--hub-url", "http://127.0.0.1:7787",
		"--hub-project-id", "42",
		"--hub-project-uid", "01HZNQ7VFPK1XGD8R5MABCD4EG",
		"--replay-horizon", "7",
		"--baseline-through", "9",
		"--token", "join-token",
		"--capabilities", "pull,push,lease")

	require.NoError(t, err)
	assert.Contains(t, stdout, "joined federation project fedlab")
	assert.Contains(t, stderr, "warning:")
	assert.Contains(t, stderr, "push capability is present but local push is disabled")
}

func TestFederationEnrollmentsListCLIShowsHubEnrollments(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, "fedlab")
	require.NoError(t, err)
	_, err = env.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "list-token",
		SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EF",
		ProjectID:        &project.ID,
		Capabilities:     "pull,push,claim",
	})
	require.NoError(t, err)

	out := requireCmdOutput(t, env, "federation", "enrollments", "list")

	assert.Contains(t, out, "01HZNQ7VFPK1XGD8R5MABCD4EF")
	assert.Contains(t, out, "project: "+strconv.FormatInt(project.ID, 10))
	assert.Contains(t, out, "capabilities: lease,pull,push")
	assert.Contains(t, out, "active")
	assert.NotContains(t, out, "list-token")
}

func TestFederationRevokeCLIRevokesEnrollment(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	created, err := env.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "revoke-token",
		SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EF",
		Capabilities:     "pull",
	})
	require.NoError(t, err)

	out := requireCmdOutput(t, env, "federation", "revoke", strconv.FormatInt(created.Enrollment.ID, 10))

	assert.Contains(t, out, "revoked federation enrollment #"+strconv.FormatInt(created.Enrollment.ID, 10))
	_, err = env.DB.AuthorizeFederationToken(ctx, "revoke-token", 1, "pull")
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func setupFederationStatusCLIState(t *testing.T) (*testenv.Env, db.Project) {
	t.Helper()
	resetFlags(t)
	env := testenv.New(t)
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, "spoke-cli")
	require.NoError(t, err)
	_, err = env.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 9,
		PullCursorEventID:    12,
		PushEnabled:          true,
		PushCursorEventID:    0,
		Enabled:              true,
	})
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "pending local push",
		Author:    "tester",
	})
	require.NoError(t, err)
	lastPull := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	lastPush := time.Date(2026, 5, 23, 12, 5, 0, 0, time.UTC)
	lastErrorAt := time.Date(2026, 5, 23, 12, 7, 0, 0, time.UTC)
	require.NoError(t, env.DB.RecordFederationSyncPullSuccess(ctx, project.ID, lastPull))
	require.NoError(t, env.DB.RecordFederationSyncPushSuccess(ctx, project.ID, lastPush))
	require.NoError(t, env.DB.RecordFederationSyncError(ctx, project.ID, errors.New("hub offline"), lastErrorAt))
	_, err = env.DB.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID:    project.ID,
		Direction:    db.FederationQuarantineDirectionPush,
		FirstEventID: 3,
		LastEventID:  5,
		EventUIDs:    []string{"evt-3", "evt-4", "evt-5"},
		Error:        "hub rejected batch",
		CreatedAt:    lastErrorAt.Add(time.Minute),
	})
	require.NoError(t, err)
	_, err = env.DB.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: project.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
			Holder:            "agent-a",
			ClientKind:        "cli",
		},
		ClaimKind: "hard",
		Purpose:   "edit",
		Now:       lastPull,
	})
	require.NoError(t, err)
	return env, project
}

const cliViolationSpokeUID = "01HZNQ7VFPK1XGD8R5MABCD4FF"

func ingestCLIClaimViolation(
	t *testing.T,
	env *testenv.Env,
	projectID int64,
	issue db.Issue,
	actor string,
	eventType string,
	sourceEventID int64,
) db.RemoteEvent {
	t.Helper()
	ctx := context.Background()
	project, err := env.DB.ProjectByID(ctx, projectID)
	require.NoError(t, err)
	eventUID, err := katauid.New()
	require.NoError(t, err)
	payload := json.RawMessage(`{"issue_uid":"` + issue.UID + `","title":"remote update"}`)
	createdAt := time.Date(2026, 5, 24, 12, int(sourceEventID), 0, 0, time.UTC)
	ev := db.RemoteEvent{
		EventUID:          eventUID,
		OriginInstanceUID: cliViolationSpokeUID,
		ProjectUID:        project.UID,
		ProjectName:       project.Name,
		IssueUID:          &issue.UID,
		Type:              eventType,
		Actor:             actor,
		HLCPhysicalMS:     createdAt.UnixMilli(),
		HLCCounter:        0,
		Payload:           payload,
		CreatedAt:         createdAt,
	}
	hash, err := db.EventContentHash(db.EventHashInput{
		UID:               ev.EventUID,
		OriginInstanceUID: ev.OriginInstanceUID,
		ProjectUID:        ev.ProjectUID,
		ProjectName:       ev.ProjectName,
		IssueUID:          ev.IssueUID,
		Type:              ev.Type,
		Actor:             ev.Actor,
		HLCPhysicalMS:     ev.HLCPhysicalMS,
		HLCCounter:        ev.HLCCounter,
		CreatedAt:         ev.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		Payload:           ev.Payload,
	})
	require.NoError(t, err)
	ev.ContentHash = hash
	_, err = env.DB.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID:        projectID,
		SpokeInstanceUID: cliViolationSpokeUID,
		Events: []db.FederationIngestEvent{{
			SourceEventID: sourceEventID,
			Event:         ev,
		}},
	})
	require.NoError(t, err)
	return ev
}
