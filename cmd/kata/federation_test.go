package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
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

func TestFederationQuarantineListHumanAndShow(t *testing.T) {
	env, project := setupFederationStatusCLIState(t)
	q, err := env.DB.ActiveFederationQuarantine(
		context.Background(), project.ID, db.FederationQuarantineDirectionPush)
	require.NoError(t, err)

	list := requireCmdOutput(t, env, "federation", "quarantine", "list")
	assert.Contains(t, list, "spoke-cli")
	assert.Contains(t, list, fmt.Sprintf("quarantine #%d", q.ID))
	assert.Contains(t, list, "push events 3-5")
	assert.Contains(t, list, "3 events")
	assert.Contains(t, list, "2026-05-23T12:08:00Z")
	assert.Contains(t, list, "hub rejected batch")

	show := requireCmdOutput(t, env, "federation", "quarantine", "show", strconv.FormatInt(q.ID, 10))
	assert.Contains(t, show, "spoke-cli")
	assert.Contains(t, show, fmt.Sprintf("quarantine #%d", q.ID))
	for _, uid := range []string{"evt-3", "evt-4", "evt-5"} {
		assert.Contains(t, show, uid)
	}
}

func TestFederationQuarantineListAgentAndJSON(t *testing.T) {
	env, project := setupFederationStatusCLIState(t)
	q, err := env.DB.ActiveFederationQuarantine(
		context.Background(), project.ID, db.FederationQuarantineDirectionPush)
	require.NoError(t, err)

	agent := requireCmdOutput(t, env, "--agent", "federation", "quarantine", "list")
	assert.Contains(t, agent, "OK federation-quarantine-list count=1")
	assert.Contains(t, agent,
		fmt.Sprintf("project=spoke-cli project_id=%d quarantine_id=%d direction=push first_event=3 last_event=5 event_count=3", project.ID, q.ID))
	assert.Contains(t, agent, "error=\"hub rejected batch\"")

	showAgent := requireCmdOutput(t, env, "--agent", "federation", "quarantine", "show", strconv.FormatInt(q.ID, 10))
	assert.Contains(t, showAgent, fmt.Sprintf("OK federation-quarantine-show quarantine_id=%d", q.ID))
	assert.Contains(t, showAgent, "event_uid=evt-3")
	assert.Contains(t, showAgent, "event_uid=evt-4")
	assert.Contains(t, showAgent, "event_uid=evt-5")

	jsonOut := requireCmdOutput(t, env, "--json", "federation", "quarantine", "list")
	var got struct {
		KataAPIVersion int `json:"kata_api_version"`
		Quarantines    []struct {
			ProjectID   int64    `json:"project_id"`
			ProjectName string   `json:"project_name"`
			ID          int64    `json:"id"`
			EventUIDs   []string `json:"event_uids"`
			Error       string   `json:"error"`
		} `json:"quarantines"`
	}
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &got))
	assert.Equal(t, 1, got.KataAPIVersion)
	require.Len(t, got.Quarantines, 1)
	assert.Equal(t, project.ID, got.Quarantines[0].ProjectID)
	assert.Equal(t, "spoke-cli", got.Quarantines[0].ProjectName)
	assert.Equal(t, q.ID, got.Quarantines[0].ID)
	assert.Equal(t, []string{"evt-3", "evt-4", "evt-5"}, got.Quarantines[0].EventUIDs)
	assert.Equal(t, "hub rejected batch", got.Quarantines[0].Error)
}

func TestFederationQuarantineListEmptyAndShowMissing(t *testing.T) {
	env := testenv.New(t)

	human := requireCmdOutput(t, env, "federation", "quarantine", "list")
	assert.Contains(t, human, "no active federation quarantines")
	agent := requireCmdOutput(t, env, "--agent", "federation", "quarantine", "list")
	assert.Equal(t, "OK federation-quarantine-list count=0\n", agent)

	_, err := runCmdOutput(t, env, "federation", "quarantine", "show", "99")
	ce := requireCLIError(t, err, ExitNotFound)
	assert.Equal(t, "federation_quarantine_not_found", ce.Code)
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

func TestFederationQuarantineRetryCLI(t *testing.T) {
	env, project := setupFederationStatusCLIState(t)
	ctx := context.Background()
	q, err := env.DB.ActiveFederationQuarantine(ctx, project.ID, db.FederationQuarantineDirectionPush)
	require.NoError(t, err)

	out := requireCmdOutput(t, env, "federation", "quarantine", "retry", strconv.FormatInt(q.ID, 10),
		"--confirm", "RETRY FEDERATION BATCH "+strconv.FormatInt(q.ID, 10),
		"--reason", "hub upgraded")

	assert.Contains(t, out, fmt.Sprintf("quarantine #%d released for retry", q.ID))
	binding, err := env.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), binding.PushCursorEventID)
	_, err = env.DB.ActiveFederationQuarantine(ctx, project.ID, db.FederationQuarantineDirectionPush)
	assert.ErrorIs(t, err, db.ErrNotFound)
	var skipReason string
	require.NoError(t, env.DB.QueryRow(`
		SELECT skip_reason
		  FROM federation_quarantine
		 WHERE id = ?`,
		q.ID).Scan(&skipReason))
	assert.Equal(t, "retry: hub upgraded", skipReason)
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
	assert.Contains(t, out, "leave")
	assert.Contains(t, out, "revoke")
	assert.NotContains(t, out, "rewrite-author")
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

func TestFederationEnableCLIRequiresExactProjectFlagName(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, "team/hub-project")
	require.NoError(t, err)

	_, _, err = runCmdCapture(t, env, "federation", "enable", "--project", "hub-project")

	ce := requireCLIError(t, err, ExitNotFound)
	assert.Contains(t, ce.Message, "project hub-project is not registered")
	_, err = env.DB.FederationBindingByProject(ctx, project.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestFederationEnableCLIDoesNotCreateProjectFromProjectFlag(t *testing.T) {
	env := testenv.New(t)

	_, _, err := runCmdCapture(t, env, "federation", "enable", "--project", "missing-project")

	ce := requireCLIError(t, err, ExitNotFound)
	assert.Contains(t, ce.Message, "project missing-project is not registered")
	_, err = env.DB.ProjectByName(context.Background(), "missing-project")
	assert.ErrorIs(t, err, db.ErrNotFound)
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
	spokeUID := env.DB.InstanceUID()
	savedArgs := os.Args
	os.Args = []string{"/opt/kata-fedlab"}
	t.Cleanup(func() { os.Args = savedArgs })

	out := runCLI(t, env, dir, "federation", "enroll",
		"--spoke-instance", spokeUID,
		"--hub-url", env.URL,
		"--actor", "wesm")

	assert.Contains(t, out, "enrolled "+spokeUID+" for kata")
	assert.Contains(t, out, "kata-fedlab federation join")
	assert.NotContains(t, out, "/opt/kata-fedlab federation join")
	assert.NotContains(t, out, "join: kata federation join")
	assert.Contains(t, out, "--hub-url "+env.URL)
	assert.Contains(t, out, "--hub-project-id "+strconv.FormatInt(pid, 10))
	assert.Contains(t, out, "--project kata")
	assert.Contains(t, out, "--actor wesm")
	assert.NotContains(t, out, "--hub-project-uid")
	assert.NotContains(t, out, "--replay-horizon")
	assert.NotContains(t, out, "--baseline-through")
	assert.Contains(t, out, "--push")
	// The single-daemon setup makes the spoke project the hub project itself
	// (same UID), which is the rejoin shape: no adoption is auto-marked.
	assert.NotContains(t, out, "--adopt-existing")
	assert.Contains(t, out, "--token ")
}

func TestFederationEnrollCLIUsesHubURLForEnrollmentAndDefaultDaemonForAdoption(t *testing.T) {
	resetFlags(t)
	hub := testenv.New(t, testenv.WithAuthToken("hub-token"))
	spoke := testenv.New(t)
	ctx := context.Background()
	spokeProject, err := spoke.DB.CreateProject(ctx, "fedlab")
	require.NoError(t, err)
	spokeUID := spoke.DB.InstanceUID()

	cmd := newRootCmd()
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{
		"--project", "fedlab",
		"federation", "enroll",
		"--spoke-instance", spokeUID,
		"--hub-url", hub.URL,
		"--actor", "wesm",
	})
	cmd.SetContext(contextWithBaseURL(ctx, spoke.URL))

	require.NoError(t, cmd.Execute())
	out := buf.String()
	assert.Contains(t, out, "--hub-url "+hub.URL)
	assert.Contains(t, out, "--adopt-existing")

	hubProject, err := hub.DB.ProjectByName(ctx, "fedlab")
	require.NoError(t, err)
	hubBinding, err := hub.DB.FederationBindingByProject(ctx, hubProject.ID)
	require.NoError(t, err)
	assert.Equal(t, db.FederationRoleHub, hubBinding.Role)
	enrollments, err := hub.DB.ListFederationEnrollments(ctx)
	require.NoError(t, err)
	require.Len(t, enrollments, 1)
	require.NotNil(t, enrollments[0].ProjectID)
	assert.Equal(t, hubProject.ID, *enrollments[0].ProjectID)
	assert.True(t, enrollments[0].AllowAdoptionSnapshotAuthors)

	_, err = spoke.DB.FederationBindingByProject(ctx, spokeProject.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestFederationEnrollCLIUsesKATAServerAsSpokeForAdoption(t *testing.T) {
	resetFlags(t)
	hub := testenv.New(t, testenv.WithAuthToken("hub-token"))
	spoke := testenv.New(t)
	t.Setenv("KATA_SERVER", spoke.URL)
	ctx := context.Background()
	_, err := spoke.DB.CreateProject(ctx, "fedlab")
	require.NoError(t, err)

	cmd := newRootCmd()
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{
		"--project", "fedlab",
		"federation", "enroll",
		"--spoke-instance", spoke.DB.InstanceUID(),
		"--hub-url", hub.URL,
		"--actor", "wesm",
	})

	require.NoError(t, cmd.Execute())
	out := buf.String()
	assert.Contains(t, out, "--hub-url "+hub.URL)
	assert.Contains(t, out, "--adopt-existing")

	hubProject, err := hub.DB.ProjectByName(ctx, "fedlab")
	require.NoError(t, err)
	enrollments, err := hub.DB.ListFederationEnrollments(ctx)
	require.NoError(t, err)
	require.Len(t, enrollments, 1)
	require.NotNil(t, enrollments[0].ProjectID)
	assert.Equal(t, hubProject.ID, *enrollments[0].ProjectID)
	assert.True(t, enrollments[0].AllowAdoptionSnapshotAuthors)
}

func TestFederationEnrollCLIUsesNamedSpokeCatalogAuthForAdoption(t *testing.T) {
	resetFlags(t)
	spoke := testenv.New(t, testenv.WithAuthToken("spoke-token"))
	hub := testenv.New(t, testenv.WithAuthToken("hub-token"))
	ctx := context.Background()
	_, err := spoke.DB.CreateProject(ctx, "fedlab")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(hub.Home, "config.toml"), []byte(`
[[daemon]]
name = "spoke"
url = "`+spoke.URL+`"
token = "spoke-token"
`), 0o600))

	cmd := newRootCmd()
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{
		"--daemon", "spoke",
		"--project", "fedlab",
		"federation", "enroll",
		"--spoke-instance", spoke.DB.InstanceUID(),
		"--hub-url", hub.URL,
		"--actor", "wesm",
	})

	require.NoError(t, cmd.Execute())
	out := buf.String()
	assert.Contains(t, out, "--hub-url "+hub.URL)
	assert.Contains(t, out, "--adopt-existing")

	enrollments, err := hub.DB.ListFederationEnrollments(ctx)
	require.NoError(t, err)
	require.Len(t, enrollments, 1)
	assert.True(t, enrollments[0].AllowAdoptionSnapshotAuthors)
}

func TestFederationEnrollCLIExplicitDaemonResolutionFailureErrors(t *testing.T) {
	resetFlags(t)
	hub := testenv.New(t, testenv.WithAuthToken("hub-token"))
	ctx := context.Background()
	t.Setenv("KATA_AUTH_TOKEN", "hub-token")

	cmd := newRootCmd()
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{
		"--daemon", "missing-spoke",
		"--project", "fedlab",
		"federation", "enroll",
		"--spoke-instance", "01HZNQ7VFPK1XGD8R5MABCD4EF",
		"--hub-url", hub.URL,
		"--actor", "operator",
	})

	err := cmd.Execute()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing-spoke")
	enrollments, listErr := hub.DB.ListFederationEnrollments(ctx)
	require.NoError(t, listErr)
	assert.Empty(t, enrollments)
}

func TestFederationEnrollCLIKATAServerSpokeAuthFailureErrors(t *testing.T) {
	resetFlags(t)
	spoke := testenv.New(t, testenv.WithAuthToken("spoke-token"))
	hub := testenv.New(t, testenv.WithAuthToken("hub-token"))
	t.Setenv("KATA_SERVER", spoke.URL)
	t.Setenv("KATA_AUTH_TOKEN", "hub-token")
	ctx := context.Background()
	_, err := spoke.DB.CreateProject(ctx, "fedlab")
	require.NoError(t, err)

	cmd := newRootCmd()
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{
		"--project", "fedlab",
		"federation", "enroll",
		"--spoke-instance", spoke.DB.InstanceUID(),
		"--hub-url", hub.URL,
		"--actor", "operator",
	})

	err = cmd.Execute()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Authorization bearer required")
	_, projectErr := hub.DB.ProjectByName(ctx, "fedlab")
	assert.ErrorIs(t, projectErr, db.ErrNotFound)
	enrollments, listErr := hub.DB.ListFederationEnrollments(ctx)
	require.NoError(t, listErr)
	assert.Empty(t, enrollments)
}

func TestFederationEnrollCLISameNameAutoAdoptionRequiresMatchingSpokeInstance(t *testing.T) {
	resetFlags(t)
	hub := testenv.New(t, testenv.WithAuthToken("hub-token"))
	spoke := testenv.New(t)
	t.Setenv("KATA_SERVER", spoke.URL)
	ctx := context.Background()
	_, err := spoke.DB.CreateProject(ctx, "fedlab")
	require.NoError(t, err)
	otherSpokeUID, err := katauid.New()
	require.NoError(t, err)
	require.NotEqual(t, spoke.DB.InstanceUID(), otherSpokeUID)

	cmd := newRootCmd()
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{
		"--project", "fedlab",
		"federation", "enroll",
		"--spoke-instance", otherSpokeUID,
		"--hub-url", hub.URL,
		"--actor", "operator",
	})

	require.NoError(t, cmd.Execute())
	out := buf.String()
	assert.NotContains(t, out, "--adopt-existing")

	enrollments, err := hub.DB.ListFederationEnrollments(ctx)
	require.NoError(t, err)
	require.Len(t, enrollments, 1)
	assert.False(t, enrollments[0].AllowAdoptionSnapshotAuthors)
}

func TestFederationEnrollCLIAutoAdoptionRequiresExactSpokeProjectName(t *testing.T) {
	resetFlags(t)
	hub := testenv.New(t, testenv.WithAuthToken("hub-token"))
	spoke := testenv.New(t)
	t.Setenv("KATA_SERVER", spoke.URL)
	ctx := context.Background()
	_, err := spoke.DB.CreateProject(ctx, "workspace:fedlab")
	require.NoError(t, err)

	cmd := newRootCmd()
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{
		"--project", "fedlab",
		"federation", "enroll",
		"--spoke-instance", spoke.DB.InstanceUID(),
		"--hub-url", hub.URL,
		"--actor", "operator",
	})

	require.NoError(t, cmd.Execute())
	out := buf.String()
	assert.NotContains(t, out, "--adopt-existing")

	enrollments, err := hub.DB.ListFederationEnrollments(ctx)
	require.NoError(t, err)
	require.Len(t, enrollments, 1)
	assert.False(t, enrollments[0].AllowAdoptionSnapshotAuthors)
}

func TestFederationEnrollCLIExplicitAdoptExistingMarksEnrollmentWithoutSameNameSpokeProject(t *testing.T) {
	resetFlags(t)
	hub := testenv.New(t, testenv.WithAuthToken("hub-token"))
	spoke := testenv.New(t)
	t.Setenv("KATA_SERVER", spoke.URL)
	ctx := context.Background()
	_, err := spoke.DB.CreateProject(ctx, "local-project")
	require.NoError(t, err)

	cmd := newRootCmd()
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{
		"--project", "hub-project",
		"federation", "enroll",
		"--spoke-instance", spoke.DB.InstanceUID(),
		"--hub-url", hub.URL,
		"--actor", "wesm",
		"--adopt-existing",
	})

	require.NoError(t, cmd.Execute())
	out := buf.String()
	assert.Contains(t, out, "--project hub-project")
	assert.Contains(t, out, "--adopt-existing")

	hubProject, err := hub.DB.ProjectByName(ctx, "hub-project")
	require.NoError(t, err)
	enrollments, err := hub.DB.ListFederationEnrollments(ctx)
	require.NoError(t, err)
	require.Len(t, enrollments, 1)
	require.NotNil(t, enrollments[0].ProjectID)
	assert.Equal(t, hubProject.ID, *enrollments[0].ProjectID)
	assert.True(t, enrollments[0].AllowAdoptionSnapshotAuthors)
}

func TestFederationEnrollCLIAdoptExistingRequiresPushCapability(t *testing.T) {
	resetFlags(t)
	hub := testenv.New(t, testenv.WithAuthToken("hub-token"))
	spoke := testenv.New(t)
	t.Setenv("KATA_SERVER", spoke.URL)
	cmd := newRootCmd()
	cmd.SetArgs([]string{
		"--project", "hub-project",
		"federation", "enroll",
		"--spoke-instance", spoke.DB.InstanceUID(),
		"--hub-url", hub.URL,
		"--actor", "wesm",
		"--capabilities", "pull",
		"--adopt-existing",
	})

	err := cmd.Execute()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--adopt-existing requires push capability")
}

func TestFederationEnrollCLICreatesMissingProjectFromProjectFlag(t *testing.T) {
	env := testenv.New(t)
	dir := t.TempDir()
	spokeUID := "01HZNQ7VFPK1XGD8R5MABCD4EF"

	out := runCLI(t, env, dir,
		"--project", "new-hub-project",
		"federation", "enroll",
		"--spoke-instance", spokeUID,
		"--hub-url", env.URL,
		"--actor", "wesm")

	assert.Contains(t, out, "enrolled "+spokeUID+" for new-hub-project")
	assert.Contains(t, out, "--project new-hub-project")
	project, err := env.DB.ProjectByName(context.Background(), "new-hub-project")
	require.NoError(t, err)
	binding, err := env.DB.FederationBindingByProject(context.Background(), project.ID)
	require.NoError(t, err)
	assert.Equal(t, db.FederationRoleHub, binding.Role)
	enrollments, err := env.DB.ListFederationEnrollments(context.Background())
	require.NoError(t, err)
	require.Len(t, enrollments, 1)
	require.NotNil(t, enrollments[0].ProjectID)
	assert.Equal(t, project.ID, *enrollments[0].ProjectID)
	assert.Equal(t, "wesm", enrollments[0].Actor)
}

func TestFederationEnrollHTTPClientRequiresExplicitAllowInsecureForPlaintextHostname(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_AUTH_TOKEN", "hub-token")
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")

	client, err := federationEnrollHTTPClient(context.Background(), "http://hub.internal:7787", false)

	require.Error(t, err)
	assert.Nil(t, client)
	assert.Contains(t, err.Error(), "refusing to attach bearer token")
}

func TestFederationEnrollCLIExplicitAllowInsecurePrintsJoinFlag(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)
	spokeUID := "01HZNQ7VFPK1XGD8R5MABCD4EF"

	out := runCLI(t, env, dir,
		"federation", "enroll",
		"--spoke-instance", spokeUID,
		"--hub-url", env.URL,
		"--actor", "wesm",
		"--allow-insecure")

	assert.Contains(t, out, "--allow-insecure")
}

func TestFederationEnrollCLIPlaintextBearerErrorMentionsAllowInsecure(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)
	t.Setenv("KATA_AUTH_TOKEN", "hub-token")
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")

	_, err := runCLICapture(t, env, dir,
		"--project", "fedlab",
		"federation", "enroll",
		"--spoke-instance", "01HZNQ7VFPK1XGD8R5MABCD4EF",
		"--hub-url", "http://8.8.8.8:7787",
		"--actor", "wesm")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to attach bearer token")
	assert.Contains(t, err.Error(), "--allow-insecure")
}

func TestFederationEnrollHTTPClientAllowsExplicitInsecurePlaintext(t *testing.T) {
	t.Setenv("KATA_AUTH_TOKEN", "hub-token")

	client, err := federationEnrollHTTPClient(context.Background(), "http://8.8.8.8:7787", true)

	require.NoError(t, err)
	require.NotNil(t, client)
}

func TestResolveFederationProjectUsesProvidedClientForWorkspaceResolution(t *testing.T) {
	resetFlags(t)
	t.Setenv("KATA_AUTH_TOKEN", "hub-token")
	flags.Workspace = t.TempDir()
	baseURL := "http://hub.internal:7777"
	called := false
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		assert.Equal(t, http.MethodPost, req.Method)
		assert.Equal(t, baseURL+"/api/v1/projects/resolve", req.URL.String())
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"project":{"id":42,"name":"spoke-project"},"workspace_root":""}`)),
			Request: req,
		}, nil
	})}

	project, err := resolveFederationProject(context.Background(), client, baseURL, nil, false)

	require.NoError(t, err)
	assert.True(t, called)
	assert.Equal(t, projectRef{ID: 42, Name: "spoke-project"}, project)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestFederationSpokeProjectExistsDoesNotAttachHubTokenToSpokeProbe(t *testing.T) {
	t.Setenv("KATA_AUTH_TOKEN", "hub-token")
	var seenAuth []string
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/ping":
			_, _ = w.Write([]byte(`{"ok":true,"service":"kata","version":"test"}`))
		case "/api/v1/projects":
			seenAuth = append(seenAuth, r.Header.Get("Authorization"))
			_, _ = w.Write([]byte(`{"projects":[{"id":1,"name":"fedlab"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(spoke.Close)

	exists := federationSpokeProjectExists(contextWithBaseURL(context.Background(), spoke.URL), "fedlab", "")

	require.True(t, exists)
	require.NotEmpty(t, seenAuth)
	assert.Equal(t, []string{""}, seenAuth)
}

func TestFederationSpokeHTTPClientDoesNotUseKATAServerGlobalAuth(t *testing.T) {
	resetFlags(t)
	t.Setenv("KATA_AUTH_TOKEN", "hub-token")
	var gotAuth string
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/ping":
			_, _ = w.Write([]byte(`{"ok":true,"service":"kata","version":"test"}`))
		case "/probe":
			gotAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(spoke.Close)
	t.Setenv("KATA_SERVER", spoke.URL)

	hc, err := federationSpokeHTTPClient(context.Background(), spoke.URL)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, spoke.URL+"/probe", nil)
	require.NoError(t, err)
	resp, err := hc.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Empty(t, gotAuth)
}

func TestFederationSpokeHTTPClientDoesNotUseNamedDaemonGlobalAuthFallback(t *testing.T) {
	resetFlags(t)
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_AUTH_TOKEN", "env-token")
	flags.Daemon = "spoke"
	var gotAuth string
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(spoke.Close)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[[daemon]]
name = "spoke"
url = "`+spoke.URL+`"
`), 0o600))

	hc, err := federationSpokeHTTPClient(context.Background(), spoke.URL)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, spoke.URL+"/probe", nil)
	require.NoError(t, err)
	resp, err := hc.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Empty(t, gotAuth)
}

func TestFederationSpokeHTTPClientNamedDaemonTokenEnvHonorsTrustPrivateNetwork(t *testing.T) {
	resetFlags(t)
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_SPOKE_TOKEN", "spoke-token")
	flags.Daemon = "spoke"
	baseURL := "http://100.64.0.5:7373"
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[auth]
trust_private_network = true

[[daemon]]
name = "spoke"
url = "`+baseURL+`"
token_env = "KATA_SPOKE_TOKEN"
`), 0o600))

	hc, err := federationSpokeHTTPClient(context.Background(), baseURL)

	require.NoError(t, err)
	assert.NotNil(t, hc)
}

func TestFederationSpokeProjectExistsUsesReadonlyGETProbe(t *testing.T) {
	spokeUID := "01HZNQ7VFPK1XGD8R5MABCD4EF"
	var seenMethods []string
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMethods = append(seenMethods, r.Method+" "+r.URL.Path)
		if r.Method != http.MethodGet {
			http.Error(w, "readonly", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/ping":
			_, _ = w.Write([]byte(`{"ok":true,"service":"kata","version":"test"}`))
		case "/api/v1/instance":
			_, _ = w.Write([]byte(`{"instance_uid":"` + spokeUID + `"}`))
		case "/api/v1/projects":
			_, _ = w.Write([]byte(`{"projects":[{"id":1,"name":"fedlab"},{"id":2,"name":"workspace:other"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(spoke.Close)

	exists := federationSpokeProjectExists(contextWithBaseURL(context.Background(), spoke.URL), "fedlab", spokeUID)

	require.True(t, exists)
	assert.Equal(t, []string{
		"GET /api/v1/instance",
		"GET /api/v1/projects",
	}, seenMethods)
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
		"--hub-url", env.URL,
		"--actor", "alice")

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
		"--actor", "tester",
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
		"--actor", "tester",
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
		"--actor", "tester",
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
		"--actor", "tester",
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
		"--actor", "wesm",
		"--push")

	assert.Contains(t, out, "joined federation project fedlab")
	project, err := env.DB.ProjectByUID(context.Background(), hubProjectUID)
	require.NoError(t, err)
	assert.Equal(t, "fedlab", project.Name)
	binding, err := env.DB.FederationBindingByProject(context.Background(), project.ID)
	require.NoError(t, err)
	assert.Equal(t, db.FederationRoleSpoke, binding.Role)
	assert.True(t, binding.PushEnabled)
	assert.Equal(t, "wesm", binding.Actor)
	assert.Equal(t, int64(42), binding.HubProjectID)
	creds, err := config.ReadFederationCredentials()
	require.NoError(t, err)
	assert.Equal(t, "join-token", creds.Projects[project.UID].Token)
	assert.Equal(t, "claim,pull,push", creds.Projects[project.UID].Capabilities)
	assert.Equal(t, "wesm", creds.Projects[project.UID].Actor)
}

func TestFederationJoinCLIPersistsAllowInsecureCredential(t *testing.T) {
	env := testenv.New(t)
	hubProjectUID := "01HZNQ7VFPK1XGD8R5MABCD4EG"

	out := requireCmdOutput(t, env, "federation", "join",
		"--project", "fedlab",
		"--hub-url", "http://tailnet-hub.internal:7787",
		"--hub-project-id", "42",
		"--hub-project-uid", hubProjectUID,
		"--replay-horizon", "7",
		"--token", "join-token",
		"--actor", "wesm",
		"--allow-insecure")

	assert.Contains(t, out, "joined federation project fedlab")
	creds, err := config.ReadFederationCredentials()
	require.NoError(t, err)
	got := creds.Projects[hubProjectUID]
	assert.Equal(t, "http://tailnet-hub.internal:7787", got.HubURL)
	assert.True(t, got.AllowInsecure)
}

func TestHydrateFederationJoinMetadataAllowsPlaintextHostnameWithOptIn(t *testing.T) {
	orig := fetchFederationJoinMetadata
	t.Cleanup(func() { fetchFederationJoinMetadata = orig })
	fetchFederationJoinMetadata = func(_ context.Context, bundle federationJoinBundle) (api.ProjectFederationBody, error) {
		assert.Equal(t, "http://tailnet-hub.internal:7787", bundle.HubURL)
		assert.Equal(t, int64(42), bundle.HubProjectID)
		assert.Equal(t, "join-token", bundle.Token)
		assert.True(t, bundle.AllowInsecure)
		return api.ProjectFederationBody{
			ProjectID:              42,
			ProjectUID:             "01HZNQ7VFPK1XGD8R5MABCD4EG",
			ProjectName:            "fedlab",
			ReplayHorizonEventID:   7,
			BaselineThroughEventID: 9,
		}, nil
	}

	bundle := federationJoinBundle{
		HubURL:        "http://tailnet-hub.internal:7787",
		HubProjectID:  42,
		Token:         "join-token",
		AllowInsecure: true,
	}
	err := hydrateFederationJoinMetadata(context.Background(), &bundle)
	require.NoError(t, err)
	assert.Equal(t, "01HZNQ7VFPK1XGD8R5MABCD4EG", bundle.HubProjectUID)
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
		"--actor", "tester",
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
		"--actor", "tester",
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
		Actor:            "tester",
	})
	require.NoError(t, err)

	out := requireCmdOutput(t, spoke, "federation", "join",
		"--project", "fedlab",
		"--hub-url", hub.URL,
		"--hub-project-id", strconv.FormatInt(hubProject.ID, 10),
		"--token", created.Token,
		"--actor", "tester",
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
		"--actor", "tester",
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
		Actor:            "tester",
	})
	require.NoError(t, err)

	out := requireCmdOutput(t, env, "federation", "enrollments", "list")

	assert.Contains(t, out, "01HZNQ7VFPK1XGD8R5MABCD4EF")
	assert.Contains(t, out, "project: "+strconv.FormatInt(project.ID, 10))
	assert.Contains(t, out, "capabilities: pull,push,lease")
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
		Actor:            "tester",
	})
	require.NoError(t, err)

	out := requireCmdOutput(t, env, "federation", "revoke", strconv.FormatInt(created.Enrollment.ID, 10))

	assert.Contains(t, out, "revoked federation enrollment #"+strconv.FormatInt(created.Enrollment.ID, 10))
	_, err = env.DB.AuthorizeFederationToken(ctx, "revoke-token", 1, "pull")
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestResolveHubAdminAuthPrecedence(t *testing.T) {
	cat := &config.DaemonConfig{Daemons: []config.CatalogDaemonConfig{
		{Name: "hub-daemon", URL: "http://hub.example:7777", Token: "catalog-tok", AllowInsecure: true},
	}}
	got, err := resolveHubAdminAuth(cat, hubAuthInputs{hubURL: "http://hub.example:7777", hubName: "hub-daemon", hubToken: "explicit"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.token != "explicit" {
		t.Fatalf("explicit token should win, got %q", got.token)
	}
	got, err = resolveHubAdminAuth(cat, hubAuthInputs{hubURL: "http://hub.example:7777", hubName: "hub-daemon"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.token != "catalog-tok" {
		t.Fatalf("catalog token expected, got %q", got.token)
	}
	// allow_insecure unions the binding flag with the SAME-ORIGIN catalog
	// entry's: the entry is the operator's own opt-in for this exact origin
	// and restores the flag when it was lost with the credential.
	if !got.allowInsecure {
		t.Fatalf("same-origin catalog allow_insecure should union in, got %+v", got)
	}
	got, err = resolveHubAdminAuth(cat, hubAuthInputs{hubURL: "http://hub.example:7777"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.token != "catalog-tok" {
		t.Fatalf("url-matched catalog token expected, got %q", got.token)
	}
}

// fakeLeaveHub is a minimal hub stub for federation-leave CLI tests. It serves
// the enrollment list (one active enrollment matching the spoke instance and
// hub project) and records revoke calls so a test can assert revoke-first
// behavior. spokeInstanceUID and hubProjectID are matched by the command.
type fakeLeaveHub struct {
	spokeInstanceUID string
	hubProjectID     int64
	enrollmentID     int64
	// globalEnrollmentID, when nonzero, is served as an active enrollment with
	// nil project scope (a global grant for the same spoke instance).
	globalEnrollmentID int64
	revokedIDs         []int64
}

func newFakeLeaveHub(t *testing.T, spokeInstanceUID string, hubProjectID int64) (*fakeLeaveHub, *httptest.Server) {
	t.Helper()
	h := &fakeLeaveHub{spokeInstanceUID: spokeInstanceUID, hubProjectID: hubProjectID, enrollmentID: 7}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/ping":
			_, _ = w.Write([]byte(`{"ok":true,"service":"kata","version":"test"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/federation/enrollments":
			pid := h.hubProjectID
			out := api.ListFederationEnrollmentsBody{Enrollments: []api.FederationEnrollmentOut{{
				ID:               h.enrollmentID,
				SpokeInstanceUID: h.spokeInstanceUID,
				ProjectID:        &pid,
				Capabilities:     "pull,push,claim",
				Actor:            "wesm",
			}}}
			if h.globalEnrollmentID != 0 {
				out.Enrollments = append(out.Enrollments, api.FederationEnrollmentOut{
					ID:               h.globalEnrollmentID,
					SpokeInstanceUID: h.spokeInstanceUID,
					Capabilities:     "pull,push,claim",
					Actor:            "wesm",
				})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/federation/enrollments/") && strings.HasSuffix(r.URL.Path, "/revoke"):
			idStr := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/federation/enrollments/"), "/revoke")
			id, _ := strconv.ParseInt(idStr, 10, 64)
			h.revokedIDs = append(h.revokedIDs, id)
			_ = json.NewEncoder(w).Encode(api.RevokeFederationEnrollmentBody{ID: id, Revoked: true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return h, srv
}

// seedLeaveSpoke creates a push-enabled spoke project on env's daemon bound to
// hubURL, with a stored transport credential. Mirrors the daemon leave-route
// test fixtures but points hub_url at a caller-controlled fake hub.
func seedLeaveSpoke(t *testing.T, env *testenv.Env, name, hubURL string, hubProjectID int64) db.Project {
	t.Helper()
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, name)
	require.NoError(t, err)
	_, err = env.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               hubURL,
		HubProjectID:         hubProjectID,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 9,
		PullCursorEventID:    8,
		PushEnabled:          true,
		Actor:                "wesm",
		Enabled:              true,
	})
	require.NoError(t, err)
	require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{
		HubURL:       hubURL,
		HubProjectID: hubProjectID,
		Token:        "spoke-token",
		Actor:        "wesm",
	}))
	return project
}

func TestFederationLeaveDetachRevokesThenTearsDown(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t)
	ctx := context.Background()
	const hubProjectID int64 = 42
	hub, hubSrv := newFakeLeaveHub(t, env.DB.InstanceUID(), hubProjectID)
	project := seedLeaveSpoke(t, env, "spoke-project", hubSrv.URL, hubProjectID)

	out := requireCmdOutput(t, env, "federation", "leave", "--project", "spoke-project", "--yes")

	// Hub revoke was called for the matching enrollment before teardown.
	require.Equal(t, []int64{hub.enrollmentID}, hub.revokedIDs)
	// Local binding is gone afterward: the project is standalone.
	_, err := env.DB.FederationBindingByProject(ctx, project.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
	assert.Contains(t, out, "standalone")
}

// TestFederationLeaveAbortsOnEnrollmentUIDMismatch: when no active enrollment
// matches this spoke's instance UID but project-scoped enrollment(s) for the
// hub project are still active, "zero matches is success" would silently
// detach and delete the credential while a live token keeps hub access — the
// instance UID can drift from the enrollment's (clone/import refresh, or an
// enroll created with an explicit --spoke-instance). Leave must abort with
// the surviving IDs; --local-only stays the explicit local-teardown path.
func TestFederationLeaveAbortsOnEnrollmentUIDMismatch(t *testing.T) {
	t.Run("aborts before any teardown", func(t *testing.T) {
		resetFlags(t)
		env := testenv.New(t)
		ctx := context.Background()
		const hubProjectID int64 = 42
		// The hub's only active enrollment is for a different spoke instance.
		hub, hubSrv := newFakeLeaveHub(t, "01HZNQ7VFPK1XGD8R5MABCD4FF", hubProjectID)
		project := seedLeaveSpoke(t, env, "spoke-project", hubSrv.URL, hubProjectID)

		_, err := runCmdOutput(t, env, "federation", "leave",
			"--project", "spoke-project", "--yes")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "#7", "the surviving enrollment ID must be named")
		assert.Contains(t, err.Error(), "--local-only")
		assert.Empty(t, hub.revokedIDs, "a foreign-instance enrollment must not be auto-revoked")
		_, bindErr := env.DB.FederationBindingByProject(ctx, project.ID)
		require.NoError(t, bindErr, "binding must stay intact after the abort")
		assert.Equal(t, "present", config.FederationCredentialMetadataFor(project.UID).Status)
	})

	t.Run("--local-only remains the explicit escape", func(t *testing.T) {
		resetFlags(t)
		env := testenv.New(t)
		ctx := context.Background()
		const hubProjectID int64 = 42
		_, hubSrv := newFakeLeaveHub(t, "01HZNQ7VFPK1XGD8R5MABCD4FF", hubProjectID)
		project := seedLeaveSpoke(t, env, "spoke-project", hubSrv.URL, hubProjectID)

		_, _, err := runCmdCapture(t, env, "federation", "leave",
			"--project", "spoke-project", "--local-only", "--yes")
		require.NoError(t, err)
		_, bindErr := env.DB.FederationBindingByProject(ctx, project.ID)
		assert.ErrorIs(t, bindErr, db.ErrNotFound)
	})
}

func TestFederationLeaveLocalOnlySkipsRevoke(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t)
	ctx := context.Background()
	const hubProjectID int64 = 42
	hub, hubSrv := newFakeLeaveHub(t, env.DB.InstanceUID(), hubProjectID)
	project := seedLeaveSpoke(t, env, "spoke-project", hubSrv.URL, hubProjectID)

	stdout, stderr, err := runCmdCapture(t, env, "federation", "leave",
		"--project", "spoke-project", "--local-only", "--yes")
	require.NoError(t, err)

	// --local-only must not touch the hub.
	require.Empty(t, hub.revokedIDs)
	// Local teardown still happened.
	_, err = env.DB.FederationBindingByProject(ctx, project.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
	assert.Contains(t, stdout, "standalone")
	assert.Contains(t, stderr, "token remains valid")
}

func TestFederationLeaveDeleteArchivesReplica(t *testing.T) {
	t.Run("no open issues", func(t *testing.T) {
		resetFlags(t)
		env := testenv.New(t)
		ctx := context.Background()
		const hubProjectID int64 = 42
		hub, hubSrv := newFakeLeaveHub(t, env.DB.InstanceUID(), hubProjectID)
		project := seedLeaveSpoke(t, env, "spoke-project", hubSrv.URL, hubProjectID)

		out := requireCmdOutput(t, env, "federation", "leave",
			"--project", "spoke-project", "--delete", "--yes")

		// Hub revoke happened before archive.
		require.Equal(t, []int64{hub.enrollmentID}, hub.revokedIDs)
		// Project is now archived (not resolving as active).
		_, err := env.DB.ProjectByName(ctx, project.Name)
		assert.ErrorIs(t, err, db.ErrNotFound)
		// But it exists in the archive.
		archived, err := env.DB.ProjectByNameIncludingArchived(ctx, project.Name)
		require.NoError(t, err)
		require.NotNil(t, archived.DeletedAt, "project should be archived")
		assert.Contains(t, out, "archived")
	})

	t.Run("open issue without force returns error", func(t *testing.T) {
		resetFlags(t)
		env := testenv.New(t)
		ctx := context.Background()
		const hubProjectID int64 = 42
		_, hubSrv := newFakeLeaveHub(t, env.DB.InstanceUID(), hubProjectID)
		project := seedLeaveSpoke(t, env, "spoke-project", hubSrv.URL, hubProjectID)
		_, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID,
			Title:     "open issue",
			Author:    "tester",
		})
		require.NoError(t, err)

		_, err = runCmdOutput(t, env, "federation", "leave",
			"--project", "spoke-project", "--delete", "--yes")

		require.Error(t, err)
		ce := requireCLIError(t, err, ExitConflict)
		assert.Contains(t, ce.Message, "open issues")
		// The archive is refused BEFORE the local detach (preflight), so the
		// binding is still present and the project is still active — no
		// "detached-but-not-archived" partial state.
		_, bindErr := env.DB.FederationBindingByProject(ctx, project.ID)
		require.NoError(t, bindErr, "binding should still be present after refused archive")
		alive, dbErr := env.DB.ProjectByName(ctx, project.Name)
		require.NoError(t, dbErr, "project should still be active (not archived)")
		assert.Nil(t, alive.DeletedAt)
	})

	t.Run("open issue with force archives", func(t *testing.T) {
		resetFlags(t)
		env := testenv.New(t)
		ctx := context.Background()
		const hubProjectID int64 = 42
		hub, hubSrv := newFakeLeaveHub(t, env.DB.InstanceUID(), hubProjectID)
		project := seedLeaveSpoke(t, env, "spoke-project", hubSrv.URL, hubProjectID)
		_, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID,
			Title:     "open issue",
			Author:    "tester",
		})
		require.NoError(t, err)

		out := requireCmdOutput(t, env, "federation", "leave",
			"--project", "spoke-project", "--delete", "--force", "--yes")

		require.Equal(t, []int64{hub.enrollmentID}, hub.revokedIDs)
		_, err = env.DB.ProjectByName(ctx, project.Name)
		assert.ErrorIs(t, err, db.ErrNotFound)
		assert.Contains(t, out, "archived")
	})
}

func TestFederationLeaveNotASpoke(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t)
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	// EnableProjectFederation creates a proper hub binding using the project's UID.
	_, err = env.DB.EnableProjectFederation(ctx, project.ID, "tester")
	require.NoError(t, err)
	// Stand up a fake hub to confirm it gets zero calls.
	hub, hubSrv := newFakeLeaveHub(t, env.DB.InstanceUID(), project.ID)
	_ = hubSrv

	_, err = runCmdOutput(t, env, "federation", "leave", "--project", "hub-project", "--yes")

	require.Error(t, err)
	ce := requireCLIError(t, err, ExitValidation)
	assert.Equal(t, "not_a_spoke", ce.Code)
	// No hub calls made.
	require.Empty(t, hub.revokedIDs)
}

func TestFederationLeaveHubUnreachableAbortsWithoutLocalOnly(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t)
	ctx := context.Background()
	const hubProjectID int64 = 42

	// Start a server then immediately close it so the URL is syntactically valid
	// but the port is closed when the command runs.
	deadSrv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := deadSrv.URL
	deadSrv.Close()

	project := seedLeaveSpoke(t, env, "spoke-project", deadURL, hubProjectID)

	// Without --local-only: should fail because hub is unreachable.
	_, err := runCmdOutput(t, env, "federation", "leave",
		"--project", "spoke-project", "--yes")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "hub revoke failed")
	// Local binding must still be present (teardown was not run).
	_, bindErr := env.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, bindErr, "binding should still be present after hub-unreachable abort")

	// With --local-only: should succeed and tear down the local binding.
	_, _, err = runCmdCapture(t, env, "federation", "leave",
		"--project", "spoke-project", "--local-only", "--yes")
	require.NoError(t, err)
	_, bindErr = env.DB.FederationBindingByProject(ctx, project.ID)
	assert.ErrorIs(t, bindErr, db.ErrNotFound)
}

// TestFederationLeaveResolvesArchivedProject: an archive-leave retry must be
// able to reach the daemon's idempotent resume by name even though the
// project is archived — active-only resolution would report "not found"
// while detach/credential cleanup is still pending.
func TestFederationLeaveResolvesArchivedProject(t *testing.T) {
	t.Run("stale credential cleaned through the archived project", func(t *testing.T) {
		resetFlags(t)
		env := testenv.New(t)
		ctx := context.Background()
		project, err := env.DB.CreateProject(ctx, "spoke-project")
		require.NoError(t, err)
		// Partial archive-leave: archive committed, credential delete failed.
		require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{
			HubURL:       "http://hub.internal:7373",
			HubProjectID: 42,
			Token:        "stale-token",
		}))
		_, _, err = env.DB.RemoveProject(ctx, db.RemoveProjectParams{
			ProjectID: project.ID, Actor: "tester",
		})
		require.NoError(t, err)

		out := requireCmdOutput(t, env, "federation", "leave",
			"--project", "spoke-project", "--yes")

		assert.Contains(t, out, "already standalone")
		assert.Equal(t, "missing", config.FederationCredentialMetadataFor(project.UID).Status,
			"resume cleanup must be reachable for archived projects from the CLI")
	})

	t.Run("surviving binding detached through the archived project", func(t *testing.T) {
		resetFlags(t)
		env := testenv.New(t)
		ctx := context.Background()
		project, err := env.DB.CreateProject(ctx, "spoke-project")
		require.NoError(t, err)
		_, err = env.DB.UpsertFederationBinding(ctx, db.FederationBinding{
			ProjectID:            project.ID,
			Role:                 db.FederationRoleSpoke,
			HubURL:               "http://hub.internal:7373",
			HubProjectID:         42,
			HubProjectUID:        project.UID,
			ReplayHorizonEventID: 9,
			Enabled:              true,
		})
		require.NoError(t, err)
		// Partial archive-leave: archive committed, detach never ran. The
		// surviving binding is visible to leave (include=archived), so the
		// retry runs the normal bound path; with the hub unreachable,
		// --local-only completes the local teardown like any bound leave.
		_, _, err = env.DB.RemoveProject(ctx, db.RemoveProjectParams{
			ProjectID: project.ID, Actor: "tester",
		})
		require.NoError(t, err)

		_, _, err = runCmdCapture(t, env, "federation", "leave",
			"--project", "spoke-project", "--local-only", "--yes")
		require.NoError(t, err)

		_, bindErr := env.DB.FederationBindingByProject(ctx, project.ID)
		assert.ErrorIs(t, bindErr, db.ErrNotFound,
			"the surviving binding must be detached on the archived-project retry")
	})
}

// TestFederationLeaveDeletePreflightsArchiveBeforeRevoke: leave --delete must
// validate archive eligibility BEFORE the irreversible hub revoke. A
// predictable open-issue refusal after the revoke would leave the spoke
// locally bound with a revoked hub token, breaking sync until manual
// recovery.
func TestFederationLeaveDeletePreflightsArchiveBeforeRevoke(t *testing.T) {
	seed := func(t *testing.T, env *testenv.Env, hubURL string, hubProjectID int64) db.Project {
		t.Helper()
		ctx := context.Background()
		project, err := env.DB.CreateProject(ctx, "spoke-project")
		require.NoError(t, err)
		// Open issue created before the binding so the spoke read-only guard
		// does not block it.
		_, _, err = env.DB.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: "open issue", Author: "tester",
		})
		require.NoError(t, err)
		_, err = env.DB.UpsertFederationBinding(ctx, db.FederationBinding{
			ProjectID:            project.ID,
			Role:                 db.FederationRoleSpoke,
			HubURL:               hubURL,
			HubProjectID:         hubProjectID,
			HubProjectUID:        project.UID,
			ReplayHorizonEventID: 9,
			Enabled:              true,
		})
		require.NoError(t, err)
		require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{
			HubURL:       hubURL,
			HubProjectID: hubProjectID,
			Token:        "spoke-token",
		}))
		return project
	}

	t.Run("open-issue refusal happens before any revoke", func(t *testing.T) {
		resetFlags(t)
		env := testenv.New(t)
		ctx := context.Background()
		const hubProjectID int64 = 42
		hub, hubSrv := newFakeLeaveHub(t, env.DB.InstanceUID(), hubProjectID)
		project := seed(t, env, hubSrv.URL, hubProjectID)

		_, err := runCmdOutput(t, env, "federation", "leave",
			"--project", "spoke-project", "--delete", "--yes")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "open issues")
		assert.Empty(t, hub.revokedIDs,
			"the hub enrollment must not be revoked when the archive would be refused")
		_, bindErr := env.DB.FederationBindingByProject(ctx, project.ID)
		require.NoError(t, bindErr, "binding must stay intact after the preflight refusal")
		alive, dbErr := env.DB.ProjectByName(ctx, project.Name)
		require.NoError(t, dbErr)
		assert.Nil(t, alive.DeletedAt)
	})

	t.Run("--force skips the refusal and completes the leave", func(t *testing.T) {
		resetFlags(t)
		env := testenv.New(t)
		ctx := context.Background()
		const hubProjectID int64 = 42
		hub, hubSrv := newFakeLeaveHub(t, env.DB.InstanceUID(), hubProjectID)
		project := seed(t, env, hubSrv.URL, hubProjectID)

		_ = requireCmdOutput(t, env, "federation", "leave",
			"--project", "spoke-project", "--delete", "--force", "--yes")

		assert.Equal(t, []int64{hub.enrollmentID}, hub.revokedIDs)
		_, bindErr := env.DB.FederationBindingByProject(ctx, project.ID)
		assert.ErrorIs(t, bindErr, db.ErrNotFound)
		archived, dbErr := env.DB.ProjectByNameIncludingArchived(ctx, project.Name)
		require.NoError(t, dbErr)
		assert.NotNil(t, archived.DeletedAt, "forced archive-leave must archive")
	})
}

// TestFederationLeaveRevokesAfterProjectsRemoveArchive: `kata projects remove`
// archives a federated spoke without revoking its hub enrollment (the remove
// route has no federation guard). Leave on that archived project must still
// run the bound path — revoke the enrollment, then detach — instead of
// classifying the hidden archived binding as standalone and silently
// stranding an active enrollment on the hub. For the archive-leave retry,
// where the enrollment was already revoked, the same pass is an idempotent
// no-op (zero active matches is success).
func TestFederationLeaveRevokesAfterProjectsRemoveArchive(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t)
	ctx := context.Background()
	const hubProjectID int64 = 42
	hub, hubSrv := newFakeLeaveHub(t, env.DB.InstanceUID(), hubProjectID)
	project := seedLeaveSpoke(t, env, "spoke-project", hubSrv.URL, hubProjectID)
	// Archive the bound spoke directly — the kata projects remove path.
	_, _, err := env.DB.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: project.ID, Actor: "tester",
	})
	require.NoError(t, err)

	_ = requireCmdOutput(t, env, "federation", "leave",
		"--project", "spoke-project", "--yes")

	require.Equal(t, []int64{hub.enrollmentID}, hub.revokedIDs,
		"leave on an archived bound spoke must still revoke the hub enrollment")
	_, bindErr := env.DB.FederationBindingByProject(ctx, project.ID)
	assert.ErrorIs(t, bindErr, db.ErrNotFound)
	assert.Equal(t, "missing", config.FederationCredentialMetadataFor(project.UID).Status)
}

// TestFederationLeaveAllowInsecureFlag covers the partial-leave recovery state
// where the credential (and with it the recorded allow_insecure opt-in) is
// gone but the binding to a plaintext-hostname overlay hub remains. Without a
// restored opt-in the bearer transport refuses --hub-token before any I/O;
// --allow-insecure is the explicit leave-time escape hatch.
func TestFederationLeaveAllowInsecureFlag(t *testing.T) {
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")

	seed := func(t *testing.T, env *testenv.Env) {
		t.Helper()
		ctx := context.Background()
		project, err := env.DB.CreateProject(ctx, "spoke-project")
		require.NoError(t, err)
		_, err = env.DB.UpsertFederationBinding(ctx, db.FederationBinding{
			ProjectID:            project.ID,
			Role:                 db.FederationRoleSpoke,
			HubURL:               "http://hub.invalid:7373",
			HubProjectID:         42,
			HubProjectUID:        project.UID,
			ReplayHorizonEventID: 9,
			Enabled:              true,
		})
		require.NoError(t, err)
		// No credential on disk: the opt-in recorded at join time is lost.
	}

	t.Run("hub token to a plaintext hostname is refused without the opt-in", func(t *testing.T) {
		resetFlags(t)
		env := testenv.New(t)
		seed(t, env)

		_, err := runCmdOutput(t, env, "federation", "leave",
			"--project", "spoke-project", "--hub-token", "admin-token", "--yes")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "refusing to attach bearer token",
			"plaintext hostname + bearer token must be refused without allow_insecure")
	})

	t.Run("--allow-insecure restores the transport opt-in", func(t *testing.T) {
		resetFlags(t)
		env := testenv.New(t)
		seed(t, env)

		_, err := runCmdOutput(t, env, "federation", "leave",
			"--project", "spoke-project", "--hub-token", "admin-token", "--allow-insecure", "--yes")

		// hub.invalid never resolves, so the revoke still fails — but at the
		// network layer, past the bearer-transport refusal.
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "refusing to attach bearer token",
			"--allow-insecure must get past the plaintext bearer refusal")
	})
}

func TestFederationLeaveResumeWhenAlreadyStandalone(t *testing.T) {
	t.Run("delete on no-binding project archives it", func(t *testing.T) {
		resetFlags(t)
		env := testenv.New(t)
		ctx := context.Background()
		project, err := env.DB.CreateProject(ctx, "standalone-project")
		require.NoError(t, err)

		// No hub server needed: no revoke should be attempted.
		out := requireCmdOutput(t, env, "federation", "leave",
			"--project", "standalone-project", "--delete", "--yes")

		// Project is archived (no hub contact needed).
		_, err = env.DB.ProjectByName(ctx, project.Name)
		assert.ErrorIs(t, err, db.ErrNotFound)
		archived, err := env.DB.ProjectByNameIncludingArchived(ctx, project.Name)
		require.NoError(t, err)
		require.NotNil(t, archived.DeletedAt, "project should be archived")
		assert.Contains(t, out, "archived")
	})

	t.Run("plain leave on no-binding project prints already standalone", func(t *testing.T) {
		resetFlags(t)
		env := testenv.New(t)
		ctx := context.Background()
		project, err := env.DB.CreateProject(ctx, "standalone-project")
		require.NoError(t, err)

		out := requireCmdOutput(t, env, "federation", "leave",
			"--project", "standalone-project", "--yes")

		assert.Contains(t, out, "already standalone")
		// Project is still active (not archived).
		alive, err := env.DB.ProjectByName(ctx, project.Name)
		require.NoError(t, err)
		assert.Nil(t, alive.DeletedAt)
	})

	t.Run("plain leave on no-binding project deletes a stale credential", func(t *testing.T) {
		resetFlags(t)
		env := testenv.New(t)
		ctx := context.Background()
		project, err := env.DB.CreateProject(ctx, "standalone-project")
		require.NoError(t, err)
		// A partially failed leave deletes the binding but can leave the hub
		// credential behind; the no-op retry must still complete that cleanup
		// instead of reporting success around it.
		require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{
			HubURL:       "http://hub.example:7777",
			HubProjectID: 42,
			Token:        "stale-token",
		}))

		out := requireCmdOutput(t, env, "federation", "leave",
			"--project", "standalone-project", "--yes")

		assert.Contains(t, out, "already standalone")
		assert.Equal(t, "missing", config.FederationCredentialMetadataFor(project.UID).Status,
			"stale hub credential must be deleted by the resume path")
	})

	t.Run("plain leave on no-binding project honors --json", func(t *testing.T) {
		resetFlags(t)
		env := testenv.New(t)
		ctx := context.Background()
		_, err := env.DB.CreateProject(ctx, "standalone-project")
		require.NoError(t, err)

		out := requireCmdOutput(t, env, "--json", "federation", "leave",
			"--project", "standalone-project", "--yes")

		// Output must be machine-readable JSON, not the human one-liner.
		var body map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &body),
			"standalone no-op must honor --json, got: %s", out)
		assert.Equal(t, false, body["detached"], "no-op did not detach")
		assert.Equal(t, "detach", body["disposition"])
	})

	t.Run("delete on no-binding project requires confirmation without --yes", func(t *testing.T) {
		resetFlags(t)
		env := testenv.New(t)
		ctx := context.Background()
		project, err := env.DB.CreateProject(ctx, "standalone-project")
		require.NoError(t, err)

		// No --yes and no TTY: the standalone --delete archive must be
		// confirm-gated, not silently archived.
		_, err = runCmdOutput(t, env, "federation", "leave",
			"--project", "standalone-project", "--delete")
		require.Error(t, err)
		_ = requireCLIError(t, err, ExitConfirm)

		// The project must still be active (the archive was gated, not run).
		alive, dbErr := env.DB.ProjectByName(ctx, project.Name)
		require.NoError(t, dbErr)
		assert.Nil(t, alive.DeletedAt, "archive must not run without confirmation")
	})
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
		Actor:                "tester",
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

// TestFederationJoinLeaveJoinRoundTrip is the CLI round-trip contract: a spoke
// that joins, leaves, and joins the same hub project again must come back as a
// working replica. The leave keeps the local project's shared hub UID, so the
// second join exercises the daemon's rejoin path.
func TestFederationJoinLeaveJoinRoundTrip(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t)
	ctx := context.Background()
	const hubProjectID int64 = 42
	hubProjectUID := "01HZNQ7VFPK1XGD8R5MABCD4EG"
	_, hubSrv := newFakeLeaveHub(t, env.DB.InstanceUID(), hubProjectID)

	join := func(token string) string {
		return requireCmdOutput(t, env, "federation", "join",
			"--project", "fedlab",
			"--hub-url", hubSrv.URL,
			"--hub-project-id", "42",
			"--hub-project-uid", hubProjectUID,
			"--replay-horizon", "7",
			"--token", token,
			"--actor", "wesm",
			"--push")
	}

	join("join-token")
	project, err := env.DB.ProjectByUID(ctx, hubProjectUID)
	require.NoError(t, err)

	requireCmdOutput(t, env, "federation", "leave", "--project", "fedlab", "--yes")
	_, err = env.DB.FederationBindingByProject(ctx, project.ID)
	require.ErrorIs(t, err, db.ErrNotFound, "leave must remove the binding")

	out := join("rejoin-token")
	assert.Contains(t, out, "joined federation project fedlab")
	binding, err := env.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.True(t, binding.PushEnabled, "rejoin must honor --push")
	assert.Equal(t, int64(0), binding.PushCursorEventID,
		"rejoin re-offers local-origin events from 0 for hub-side dedup")
	creds, err := config.ReadFederationCredentials()
	require.NoError(t, err)
	assert.Equal(t, "rejoin-token", creds.Projects[project.UID].Token,
		"rejoin must store the fresh enrollment token")
}

// TestFederationEnrollCLISameNameUIDHolderPrintsRejoinJoin: when the spoke's
// same-name project already shares the hub project's UID (it previously left
// this federation), enroll must not auto-mark adoption — the printed join is
// a plain rejoin that rebinds without rewriting local event history.
func TestFederationEnrollCLISameNameUIDHolderPrintsRejoinJoin(t *testing.T) {
	resetFlags(t)
	hub := testenv.New(t, testenv.WithAuthToken("hub-token"))
	spoke := testenv.New(t)
	t.Setenv("KATA_SERVER", spoke.URL)
	ctx := context.Background()
	hubProject, err := hub.DB.CreateProject(ctx, "fedlab")
	require.NoError(t, err)
	_, err = spoke.DB.CreateProjectWithUID(ctx, "fedlab", hubProject.UID)
	require.NoError(t, err)

	cmd := newRootCmd()
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{
		"--project", "fedlab",
		"federation", "enroll",
		"--spoke-instance", spoke.DB.InstanceUID(),
		"--hub-url", hub.URL,
		"--actor", "operator",
	})

	require.NoError(t, cmd.Execute())
	out := buf.String()
	assert.Contains(t, out, "federation join")
	assert.NotContains(t, out, "--adopt-existing",
		"a UID-holder rejoin must not be railroaded into adoption")

	enrollments, err := hub.DB.ListFederationEnrollments(ctx)
	require.NoError(t, err)
	require.Len(t, enrollments, 1)
	assert.False(t, enrollments[0].AllowAdoptionSnapshotAuthors)
}

// TestFederationLeaveWarnsAboutActiveGlobalEnrollment: leave revokes the
// project-scoped enrollment but must not silently ignore a matching GLOBAL
// enrollment — it still authorizes the project, yet may serve the spoke's
// other projects, so it is surfaced as a warning instead of auto-revoked.
func TestFederationLeaveWarnsAboutActiveGlobalEnrollment(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t)
	const hubProjectID int64 = 42
	hub, hubSrv := newFakeLeaveHub(t, env.DB.InstanceUID(), hubProjectID)
	hub.globalEnrollmentID = 11
	seedLeaveSpoke(t, env, "spoke-project", hubSrv.URL, hubProjectID)

	stdout, stderr, err := runCmdCapture(t, env, "federation", "leave",
		"--project", "spoke-project", "--yes")
	require.NoError(t, err)

	require.Equal(t, []int64{hub.enrollmentID}, hub.revokedIDs,
		"only the project-scoped enrollment is revoked")
	assert.Contains(t, stdout, "standalone")
	assert.Contains(t, stderr, "global enrollment(s) #11")
	assert.Contains(t, stderr, "remain active")
}
