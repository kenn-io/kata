package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/githubsync"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/tracker"
)

// A completed control UID is immutable even if a later operation changes the
// binding. Retrying it cannot re-run a disable or acquire another account.
func TestTrackerControlReplayAndConflict(t *testing.T) {
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "tracker.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "tracker-project")
	require.NoError(t, err)
	p := tracker.Principal{AuthorityInstanceUID: store.InstanceUID(), Kind: tracker.SubjectAccount, Subject: "member"}
	c := config.TrackerConnectionConfig{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "one", Provider: "notion", Label: "Example account", Notion: &config.NotionSyncConfig{TokenEnv: "EXAMPLE_NOTION_TOKEN"}, Grants: []tracker.Grant{{RootInstanceUID: store.InstanceUID(), ProjectUID: project.UID, Principal: p, Actions: []tracker.Action{tracker.ActionDisable}, Sources: []string{"notion:source"}}}}
	r, err := NewTrackerRuntime(ctx, store, []config.TrackerConnectionConfig{c}, []config.TrackerAccessMapping{{ProjectUID: project.UID, Principal: p, Actor: "member"}})
	require.NoError(t, err)
	selection := c.Descriptor(store.InstanceUID()).Selection
	_, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: project.ID, Provider: "notion", SourceKey: "notion:source", RemoteID: "source", DisplayName: "Example source", Config: []byte(`{}`), IntervalSeconds: 300, Connection: &selection})
	require.NoError(t, err)
	body := api.TrackerOperationBody{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", Kind: tracker.ControlDisable, Provider: "notion", Resource: "notion:source"}
	cfg := ServerConfig{DB: store, TrackerRuntime: r}
	first, err := runTrackerOperation(ctx, cfg, project, p, body)
	require.NoError(t, err)
	require.Equal(t, tracker.ControlComplete, first.State)
	require.False(t, first.Result.Enabled)
	binding, err := store.IssueSyncBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	require.False(t, binding.Enabled)
	again, err := runTrackerOperation(ctx, cfg, project, p, body)
	require.NoError(t, err)
	require.Equal(t, first, again)
	after, err := store.IssueSyncBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	require.True(t, binding.UpdatedAt.Equal(after.UpdatedAt), "no repeated mutation")
	body.Resource = "notion:other"
	_, err = runTrackerOperation(ctx, cfg, project, p, body)
	require.ErrorIs(t, err, tracker.ErrControlConflict)
	body.Resource = "notion:source"
	other := p
	other.Subject = "other-member"
	_, err = runTrackerOperation(ctx, cfg, project, other, body)
	require.ErrorIs(t, err, tracker.ErrDenied)
}

func TestTrackerControlRejectsCredentialOptionsBeforeDigest(t *testing.T) {
	body := api.TrackerOperationBody{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", Kind: tracker.ControlBind, Provider: "notion", Resource: "notion:source", Binding: &api.EnableIssueSyncRequestBody{Config: api.JSONMap{"token_env": "FORBIDDEN_SELECTOR"}}}
	_, err := trackerOperationDigest(body)
	require.ErrorIs(t, err, tracker.ErrInvalid)
	body.Kind = tracker.ControlRun
	_, err = trackerOperationDigest(body)
	require.ErrorIs(t, err, tracker.ErrInvalid, "non-bind operations cannot carry provider settings")
}

type trackerControlGitHub struct {
	githubsync.Fetcher
	calls int
}

func (f *trackerControlGitHub) Repository(_ context.Context, host, owner, repo string) (githubsync.Repository, error) {
	f.calls++
	return githubsync.Repository{NodeID: "R_example", ID: 123, FullName: owner + "/" + repo}, nil
}

func TestTrackerBindUsesSelectedClientAndRequiresServiceAuthority(t *testing.T) {
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "tracker.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "tracker-project")
	require.NoError(t, err)
	p := tracker.Principal{AuthorityInstanceUID: store.InstanceUID(), Kind: tracker.SubjectAccount, Subject: "member"}
	service := tracker.Principal{AuthorityInstanceUID: store.InstanceUID(), Kind: tracker.SubjectService, Subject: "tracker-service"}
	resource := "github:repository:github.com/example-owner/example-repo"
	c := config.TrackerConnectionConfig{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "one", Provider: "github", Label: "Example account", GitHub: &config.GitHubSyncConfig{TokenEnv: "EXAMPLE_GITHUB_TOKEN"}, Grants: []tracker.Grant{{RootInstanceUID: store.InstanceUID(), ProjectUID: project.UID, Principal: p, Actions: []tracker.Action{tracker.ActionBind}, Sources: []string{resource}}, {RootInstanceUID: store.InstanceUID(), ProjectUID: project.UID, Principal: service, Actions: []tracker.Action{tracker.ActionRead}, Sources: []string{resource}}}}
	r, err := NewTrackerRuntime(ctx, store, []config.TrackerConnectionConfig{c}, []config.TrackerAccessMapping{{ProjectUID: project.UID, Principal: p, Actor: "member"}, {ProjectUID: project.UID, Principal: service, Actor: "tracker-service"}})
	require.NoError(t, err)
	client := &trackerControlGitHub{}
	snapshot, err := r.current()
	require.NoError(t, err)
	selected := snapshot.connections[c.UID]
	selected.clients.github = client
	snapshot.connections[c.UID] = selected
	selection := selected.descriptor.Selection
	body := api.TrackerOperationBody{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", Kind: tracker.ControlBind, Provider: "github", Resource: resource, Selection: &selection, Binding: &api.EnableIssueSyncRequestBody{Config: api.JSONMap{"owner": "example-owner", "repo": "example-repo"}}}
	cfg := ServerConfig{DB: store, TrackerRuntime: r}
	result, err := runTrackerOperation(ctx, cfg, project, p, body)
	require.NoError(t, err)
	require.Equal(t, tracker.ControlComplete, result.State)
	require.Equal(t, 1, client.calls)
	binding, err := store.IssueSyncBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	require.Equal(t, &c.UID, binding.ConnectionUID)
	require.NotContains(t, string(binding.Config), "EXAMPLE_GITHUB_TOKEN")
	_, err = runTrackerOperation(ctx, cfg, project, p, body)
	require.NoError(t, err)
	require.Equal(t, 1, client.calls, "completed replay makes no provider call")
}

func TestUnsupportedTrackerControlDoesNotConsumeUIDAndStateErrorsAreSpecific(t *testing.T) {
	body := api.TrackerOperationBody{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", Kind: tracker.ControlAcceptRemote, Provider: "notion", Resource: "notion:source"}
	_, err := trackerOperationDigest(body)
	require.ErrorIs(t, err, errTrackerOperationUnsupported, "unsupported effect must fail before native receipt admission")
	for _, c := range []struct {
		err  error
		code string
	}{
		{db.ErrIssueSyncNotEnabled, "tracker_binding_disabled"},
		{db.ErrIssueSyncAlreadyRunning, "tracker_running"},
		{db.ErrIssueSyncProjectAlreadyBound, "tracker_project_already_bound"},
		{context.DeadlineExceeded, "tracker_timeout"},
		{fmt.Errorf("fixture private: %w", context.DeadlineExceeded), "tracker_timeout"},
		{db.ErrNotFound, "not_found"},
		{errors.Join(db.ErrTrackerOutcomeUnknown, context.DeadlineExceeded), "tracker_outcome_unknown"},
		{errors.Join(db.ErrTrackerOutcomeUnknown, tracker.ErrDenied), "tracker_outcome_unknown"},
		{errors.Join(db.ErrTrackerOutcomeUnknown, tracker.ErrInvalid), "tracker_outcome_unknown"},
		{errors.Join(db.ErrTrackerOutcomeUnknown, db.ErrIssueSyncNotEnabled), "tracker_outcome_unknown"},
	} {
		code := trackerControlErrorCode(c.err)
		require.Equal(t, c.code, code)
		require.NoError(t, (tracker.ControlResult{Code: code}).Validate())
	}
}

// Abandoning a caller's pending control only records unresolved completion and
// releases control capacity. It cannot erase the underlying provider hazard.
func TestTrackerControlSettlementRefusesLiveAttemptAndRetainsUnknown(t *testing.T) {
	f := newTrackerCoordinatorFixture(t)
	_, err := f.runtime.WriteNamedStatus(f.ctx, f.binding, f.mapping, "closed", f.guard, func(_ context.Context, admit func() error) (issuesync.StatusObservation, error) {
		if err := admit(); err != nil {
			return issuesync.StatusObservation{}, err
		}
		return issuesync.StatusObservation{}, errors.New("fixture lost response")
	}, func(context.Context) error { return nil })
	require.Error(t, err)
	project, err := f.store.ProjectByID(t.Context(), f.binding.ProjectID)
	require.NoError(t, err)
	before, found, err := f.store.LoadTrackerExecutionFence(t.Context(), f.store.InstanceUID(), project.UID, "coordinator")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, db.TrackerWriteUnknown, before.Phase)
	creator := tracker.Principal{AuthorityInstanceUID: f.store.InstanceUID(), Kind: tracker.SubjectAccount, Subject: "member"}
	selection, err := db.IssueSyncConnectionFromConfig(f.binding.Config)
	require.NoError(t, err)
	original := tracker.ControlReceipt{Version: 1, RootInstanceUID: f.store.InstanceUID(), ProjectUID: project.UID, UID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", Principal: creator, Selection: *selection, Kind: tracker.ControlRun, SourceKey: f.binding.SourceKey, RequestDigest: selectionDigestForControlTest(), State: tracker.ControlPending, ProcessEpoch: f.runtime.epoch}
	_, fresh, err := f.store.BeginTrackerControl(t.Context(), original)
	require.NoError(t, err)
	require.True(t, fresh)
	finish, err := f.runtime.beginControlAttempt(project.UID, original.UID)
	require.NoError(t, err)
	operator := tracker.Principal{AuthorityInstanceUID: f.store.InstanceUID(), Kind: tracker.SubjectStaticOwner, Subject: f.store.InstanceUID()}
	_, err = f.runtime.settleTrackerControl(t.Context(), project.UID, original.UID, operator)
	require.ErrorIs(t, err, tracker.ErrControlPending)
	finish()
	settled, err := f.runtime.settleTrackerControl(t.Context(), project.UID, original.UID, operator)
	require.NoError(t, err)
	require.Equal(t, tracker.ControlFailed, settled.State)
	require.Equal(t, "tracker_outcome_unknown", settled.Result.Code)
	require.NotNil(t, settled.Settlement)
	require.Equal(t, operator, settled.Settlement.Operator)
	require.Equal(t, original.RequestDigest, settled.RequestDigest)
	again, err := f.runtime.settleTrackerControl(t.Context(), project.UID, original.UID, operator)
	require.NoError(t, err)
	require.Equal(t, settled, again)
	after, found, err := f.store.LoadTrackerExecutionFence(t.Context(), f.store.InstanceUID(), project.UID, "coordinator")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, before, after, "settling the caller control never erases provider uncertainty")
}
func selectionDigestForControlTest() string {
	return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}

func TestRebindRequiresExplicitSelectionAndBindingRevisionBeforeProviderUse(t *testing.T) {
	body := api.TrackerOperationBody{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", Kind: tracker.ControlRebind, Provider: "notion", Resource: "notion:source", Binding: &api.EnableIssueSyncRequestBody{Config: api.JSONMap{}}}
	_, err := trackerOperationDigest(body)
	require.ErrorIs(t, err, tracker.ErrInvalid)
	body.Selection = &tracker.Selection{OwnerInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "second"}
	_, err = trackerOperationDigest(body)
	require.ErrorIs(t, err, tracker.ErrInvalid)
	at := time.Now().UTC()
	body.ExpectedUpdatedAt = &at
	_, err = trackerOperationDigest(body)
	require.NoError(t, err)
}

type trackerTimeoutRunner struct{ calls int }

func (r *trackerTimeoutRunner) RunOnce(context.Context, int64) (githubsync.RunResult, error) {
	r.calls++
	return githubsync.RunResult{}, fmt.Errorf("fixture private diagnostic: %w", context.DeadlineExceeded)
}

// A provider/control deadline must retain an exact classified receipt. Replaying
// that UID reads its terminal result instead of starting another pass.
func TestTrackerControlRetainsTimeoutWithoutReexecution(t *testing.T) {
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "timeout.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "timeout-project")
	require.NoError(t, err)
	principal := tracker.Principal{AuthorityInstanceUID: store.InstanceUID(), Kind: tracker.SubjectAccount, Subject: "member"}
	connection := config.TrackerConnectionConfig{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "one", Provider: "github", Label: "Example account", GitHub: &config.GitHubSyncConfig{TokenEnv: "EXAMPLE_GITHUB_TOKEN"}, Grants: []tracker.Grant{{RootInstanceUID: store.InstanceUID(), ProjectUID: project.UID, Principal: principal, Actions: []tracker.Action{tracker.ActionRead}, Sources: []string{"github:source"}}}}
	runtime, err := NewTrackerRuntime(ctx, store, []config.TrackerConnectionConfig{connection}, []config.TrackerAccessMapping{{ProjectUID: project.UID, Principal: principal, Actor: "member"}})
	require.NoError(t, err)
	selection := connection.Descriptor(store.InstanceUID()).Selection
	_, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: project.ID, Provider: "github", SourceKey: "github:source", RemoteID: "source", DisplayName: "Example source", Config: []byte(`{}`), IntervalSeconds: 300, Connection: &selection})
	require.NoError(t, err)
	runner := &trackerTimeoutRunner{}
	cfg := ServerConfig{DB: store, TrackerRuntime: runtime, GitHubSyncRunnerFactory: func(GitHubSyncRunnerConfig) GitHubSyncRunner { return runner }}
	body := api.TrackerOperationBody{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", Kind: tracker.ControlRun, Provider: "github", Resource: "github:source"}
	first, err := runTrackerOperation(ctx, cfg, project, principal, body)
	require.NoError(t, err)
	require.Equal(t, tracker.ControlFailed, first.State)
	require.Equal(t, "tracker_timeout", first.Result.Code)
	again, err := runTrackerOperation(ctx, cfg, project, principal, body)
	require.NoError(t, err)
	require.Equal(t, first, again)
	require.Equal(t, 1, runner.calls)
}

func TestTrackerControlPreflightTimeoutClassesStaySpecific(t *testing.T) {
	for _, code := range []string{"github_timeout", "notion_timeout", "linear_timeout", "plane_timeout", "ticktick_timeout", "todoist_timeout", "twenty_timeout", "tracker_timeout"} {
		err := api.NewError(http.StatusGatewayTimeout, code, "fixture private diagnostic", "", nil)
		require.Equal(t, "tracker_timeout", trackerControlErrorCode(err), code)
	}
	require.Equal(t, "tracker_unavailable", trackerControlErrorCode(api.NewError(http.StatusBadGateway, "fixture_timeout", "fixture private", "", nil)))
}

type trackerDeadlineGitHub struct{ githubsync.Fetcher }

func (trackerDeadlineGitHub) Repository(context.Context, string, string, string) (githubsync.Repository, error) {
	return githubsync.Repository{}, context.DeadlineExceeded
}
func TestTrackerGitHubPreflightPreservesDeadlineBeforeValidation(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "deadline.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(t.Context(), "deadline-project")
	require.NoError(t, err)
	in := &api.EnableIssueSyncRequest{ProjectID: project.ID, Body: api.EnableIssueSyncRequestBody{Config: api.JSONMap{"owner": "example-owner", "repo": "example-repo"}}}
	_, err = githubSyncEnableParams(t.Context(), ServerConfig{DB: store, GitHubSyncFetcher: trackerDeadlineGitHub{}}, in)
	require.Equal(t, "tracker_timeout", trackerControlErrorCode(err))
}

func TestTrackerUnknownAPIFailureCannotExposeJoinedDiagnostics(t *testing.T) {
	cause := errors.Join(api.NewError(http.StatusBadRequest, "validation", "fixture-private-diagnostic", "", nil), db.ErrTrackerOutcomeUnknown)
	var failure *api.APIError
	require.ErrorAs(t, trackerAPIError(cause), &failure)
	require.Equal(t, "tracker_outcome_unknown", failure.Code)
	require.NotContains(t, failure.Message, "fixture-private-diagnostic")
}
