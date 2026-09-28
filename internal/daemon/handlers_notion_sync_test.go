package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/githubsync"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/notionsync"
	"go.kenn.io/kata/internal/testenv"
)

const notionSourceID = "11111111-1111-1111-1111-111111111111"
const notionDatabaseID = "22222222-2222-2222-2222-222222222222"

type lifecycleNotionFetcher struct {
	source                                  notionsync.DataSource
	database                                notionsync.Database
	credentialErr, databaseErr, schemaErr   error
	beforeSource                            func()
	forRunCalls, databaseCalls, sourceCalls atomic.Int32
}

func (f *lifecycleNotionFetcher) ForRun(context.Context) (notionsync.Session, error) {
	f.forRunCalls.Add(1)
	return f, f.credentialErr
}
func (f *lifecycleNotionFetcher) Database(context.Context, string) (notionsync.Database, error) {
	f.databaseCalls.Add(1)
	return f.database, f.databaseErr
}
func (f *lifecycleNotionFetcher) DataSource(context.Context, string) (notionsync.DataSource, error) {
	f.sourceCalls.Add(1)
	if f.beforeSource != nil {
		f.beforeSource()
	}
	return f.source, f.schemaErr
}
func (*lifecycleNotionFetcher) Pages(context.Context, notionsync.Config, *time.Time) ([]notionsync.Page, error) {
	return nil, nil
}
func (*lifecycleNotionFetcher) Content(context.Context, notionsync.Config, notionsync.Page) (notionsync.PageContent, error) {
	return notionsync.PageContent{}, errors.New("unexpected content request")
}
func newLifecycleNotionFetcher() *lifecycleNotionFetcher {
	return &lifecycleNotionFetcher{
		source: notionsync.DataSource{ID: notionSourceID, DatabaseID: notionDatabaseID, Name: "Tasks", Properties: []notionsync.Property{
			{ID: "title", Name: "Task", Type: "title"}, {ID: "status", Name: "State", Type: "status", Options: []notionsync.Option{{ID: "done", Name: "Delivered"}, {ID: "closed", Name: "Closed"}}}, {ID: "people", Name: "Owner", Type: "people"},
		}}, database: notionsync.Database{ID: notionDatabaseID, DataSources: []notionsync.Option{{ID: notionSourceID, Name: "Tasks"}}},
	}
}

type notionLifecycleHarness struct {
	store              db.Storage
	project            db.Project
	server             *httptest.Server
	fetcher            *lifecycleNotionFetcher
	wakes, githubWakes atomic.Int32
}

func newNotionLifecycleHarness(t *testing.T, customize func(*daemon.ServerConfig)) *notionLifecycleHarness {
	t.Helper()
	d := openTestDB(t)
	project, err := d.db.CreateProject(context.Background(), "example-project")
	require.NoError(t, err)
	h := &notionLifecycleHarness{store: d.db, project: project, fetcher: newLifecycleNotionFetcher()}
	cfg := daemon.ServerConfig{DB: d.db, StartedAt: d.now, NotionSyncFetcher: h.fetcher, NotionSyncWake: func() { h.wakes.Add(1) }, GitHubSyncWake: func() { h.githubWakes.Add(1) }}
	if customize != nil {
		customize(&cfg)
	}
	h.server = startTestServer(t, cfg)
	return h
}
func notionEnableBody() map[string]any {
	return map[string]any{"config": map[string]any{"data_source_id": notionSourceID, "done_statuses": []string{"Delivered"}}}
}
func (h *notionLifecycleHarness) enable(t *testing.T, body map[string]any, want int) issueSyncResponseBody {
	t.Helper()
	resp, raw := postJSON(t, h.server, issueSyncEndpoint(h.project.ID, "notion", "enable"), body)
	require.Equal(t, want, resp.StatusCode, string(raw))
	require.NotContains(t, string(raw), "private-token")
	require.NotContains(t, string(raw), "private-response")
	var out issueSyncResponseBody
	if want == http.StatusOK {
		decodeJSON(t, raw, &out)
	}
	return out
}
func TestNotionEnableLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*notionLifecycleHarness, map[string]any)
		want   int
	}{
		{"source", func(*notionLifecycleHarness, map[string]any) {}, 200},
		{"database", func(_ *notionLifecycleHarness, b map[string]any) {
			c := b["config"].(map[string]any)
			delete(c, "data_source_id")
			c["database"] = notionDatabaseID
		}, 200},
		{"multiple sources", func(h *notionLifecycleHarness, b map[string]any) {
			c := b["config"].(map[string]any)
			delete(c, "data_source_id")
			c["database"] = notionDatabaseID
			h.fetcher.database.DataSources = append(h.fetcher.database.DataSources, notionsync.Option{ID: notionDatabaseID, Name: "Other"})
		}, 400},
		{"credentials", func(h *notionLifecycleHarness, _ map[string]any) {
			h.fetcher.credentialErr = errors.New("private-token")
		}, 400},
		{"schema inaccessible", func(h *notionLifecycleHarness, _ map[string]any) {
			h.fetcher.schemaErr = errors.New("private-response")
		}, 400},
		{"custom selectors", func(h *notionLifecycleHarness, b map[string]any) {
			c := b["config"].(map[string]any)
			c["status_property"] = "State"
			c["assignee_property"] = "Owner"
			h.fetcher.source.Properties = append(h.fetcher.source.Properties, notionsync.Property{ID: "other-status", Name: "Alternate state", Type: "status"}, notionsync.Property{ID: "other-people", Name: "Alternate owner", Type: "people"})
		}, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newNotionLifecycleHarness(t, nil)
			body := notionEnableBody()
			tc.change(h, body)
			out := h.enable(t, body, tc.want)
			stored, err := h.store.IssueSyncBindingByProject(context.Background(), h.project.ID)
			if tc.want != 200 {
				require.ErrorIs(t, err, db.ErrNotFound)
				require.Zero(t, h.wakes.Load())
				return
			}
			require.NoError(t, err)
			require.Equal(t, "notion:"+notionSourceID, stored.SourceKey)
			require.Equal(t, 300, stored.IntervalSeconds)
			require.Equal(t, "status", out.Binding.Config["status_property_id"])
			require.Equal(t, []any{"done"}, out.Binding.Config["done_status_ids"])
			require.Equal(t, int32(1), h.wakes.Load())
			require.Zero(t, h.githubWakes.Load())
			h.enable(t, map[string]any{}, 200)
			resp, raw := postJSON(t, h.server, issueSyncEndpoint(h.project.ID, "notion", "once"), map[string]any{})
			require.Equal(t, 200, resp.StatusCode, string(raw))
			resp, raw = postJSON(t, h.server, issueSyncEndpoint(h.project.ID, "notion", "disable"), map[string]any{})
			require.Equal(t, 200, resp.StatusCode, string(raw))
			resp, raw = postJSON(t, h.server, issueSyncEndpoint(h.project.ID, "notion", "once"), map[string]any{})
			require.Equal(t, 400, resp.StatusCode, string(raw))
		})
	}
}

func TestNotionEnablePreservesContextErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       map[string]any
		setFailure func(*lifecycleNotionFetcher)
		want       error
	}{
		{
			name: "credential lookup canceled",
			body: notionEnableBody(),
			setFailure: func(fetcher *lifecycleNotionFetcher) {
				fetcher.credentialErr = fmt.Errorf("private credential detail: %w", context.Canceled)
			},
			want: context.Canceled,
		},
		{
			name: "database read timed out",
			body: map[string]any{"config": map[string]any{"database": notionDatabaseID, "done_statuses": []string{"Delivered"}}},
			setFailure: func(fetcher *lifecycleNotionFetcher) {
				fetcher.databaseErr = fmt.Errorf("private database detail: %w", context.DeadlineExceeded)
			},
			want: context.DeadlineExceeded,
		},
		{
			name: "schema read canceled",
			body: notionEnableBody(),
			setFailure: func(fetcher *lifecycleNotionFetcher) {
				fetcher.schemaErr = fmt.Errorf("private schema detail: %w", context.Canceled)
			},
			want: context.Canceled,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newNotionLifecycleHarness(t, nil)
			tc.setFailure(h.fetcher)
			resp, raw := postJSON(t, h.server, issueSyncEndpoint(h.project.ID, "notion", "enable"), tc.body)
			status := http.StatusInternalServerError
			if errors.Is(tc.want, context.DeadlineExceeded) {
				status = http.StatusGatewayTimeout
			}
			require.Equal(t, status, resp.StatusCode, string(raw))
			require.Contains(t, string(raw), tc.want.Error())
			require.NotContains(t, string(raw), "private")
			require.NotContains(t, string(raw), "credentials are unavailable")
			require.NotContains(t, string(raw), "cannot access Notion")
			require.Zero(t, h.wakes.Load())
		})
	}
}

func TestNotionEnableUntitledDataSourceUsesFallbackDisplayName(t *testing.T) {
	h := newNotionLifecycleHarness(t, nil)
	h.fetcher.source.Name = ""
	out := h.enable(t, notionEnableBody(), http.StatusOK)
	want := "Notion data source " + notionSourceID
	require.Equal(t, want, out.Binding.DisplayName)
	stored, err := h.store.IssueSyncBindingByProject(context.Background(), h.project.ID)
	require.NoError(t, err)
	require.Equal(t, want, stored.DisplayName)
}

func setNotionTestFederationBinding(t *testing.T, h *notionLifecycleHarness, role db.FederationRole, enabled bool) {
	t.Helper()
	_, err := h.store.UpsertFederationBinding(context.Background(), db.FederationBinding{
		ProjectID:            h.project.ID,
		Role:                 role,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        h.project.UID,
		ReplayHorizonEventID: 1,
		Enabled:              enabled,
	})
	require.NoError(t, err)
}

func TestNotionEnableRejectsFederationSpokeWithNotionGuidance(t *testing.T) {
	for _, tc := range []struct {
		name          string
		credentialErr error
	}{
		{"credentials unavailable", errors.New("credentials unavailable")},
		{"credentials available", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newNotionLifecycleHarness(t, nil)
			setNotionTestFederationBinding(t, h, db.FederationRoleSpoke, true)
			h.fetcher.credentialErr = tc.credentialErr

			resp, body := postJSON(t, h.server, issueSyncEndpoint(h.project.ID, "notion", "enable"), notionEnableBody())
			assertAPIError(t, resp.StatusCode, body, http.StatusConflict, "issue_sync_federation_conflict")
			require.Contains(t, string(body), "enable Notion sync on the hub project")
			require.Contains(t, string(body), "replicate Notion issues to spokes")
			require.NotContains(t, string(body), "GitHub")
			require.Zero(t, h.fetcher.forRunCalls.Load(), "local rejection must precede credential resolution")
			require.Zero(t, h.fetcher.databaseCalls.Load())
			require.Zero(t, h.fetcher.sourceCalls.Load())
			_, err := h.store.IssueSyncBindingByProject(context.Background(), h.project.ID)
			require.ErrorIs(t, err, db.ErrNotFound)
			require.Zero(t, h.wakes.Load())
			require.Zero(t, h.githubWakes.Load())
		})
	}
}

func TestNotionEnableFederationPreflightAllowsEligibleProjects(t *testing.T) {
	for _, tc := range []struct {
		name    string
		role    db.FederationRole
		enabled bool
	}{
		{"plain", "", false},
		{"disabled spoke", db.FederationRoleSpoke, false},
		{"enabled hub", db.FederationRoleHub, true},
		{"disabled hub", db.FederationRoleHub, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newNotionLifecycleHarness(t, nil)
			if tc.role != "" {
				setNotionTestFederationBinding(t, h, tc.role, tc.enabled)
			}
			out := h.enable(t, notionEnableBody(), http.StatusOK)
			require.True(t, out.Binding.Enabled)
			require.Equal(t, "notion", out.Binding.Provider)
			require.Equal(t, int32(1), h.wakes.Load())
			require.Zero(t, h.githubWakes.Load())
		})
	}
}

func TestNotionEnableForeignBindingPrecedesCredentialResolution(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			h := newNotionLifecycleHarness(t, nil)
			binding, err := h.store.UpsertIssueSyncBinding(context.Background(), db.UpsertIssueSyncBindingParams{
				ProjectID: h.project.ID, Provider: "github", SourceKey: "github:R_exampleNode",
				RemoteID: "R_exampleNode", DisplayName: "example-owner/example-repo",
				Config: mustDaemonGitHubSyncConfig(t, "github.com", "example-owner", "example-repo", 12345), IntervalSeconds: 300,
			})
			require.NoError(t, err)
			if !enabled {
				_, err = h.store.DisableIssueSyncBinding(context.Background(), h.project.ID)
				require.NoError(t, err)
			}
			h.fetcher.credentialErr = errors.New("credentials unavailable")
			resp, body := postJSON(t, h.server, issueSyncEndpoint(h.project.ID, "notion", "enable"), notionEnableBody())
			assertAPIError(t, resp.StatusCode, body, http.StatusBadRequest, "validation")
			require.NotContains(t, string(body), "credentials are unavailable")
			require.Zero(t, h.fetcher.forRunCalls.Load())
			require.Zero(t, h.fetcher.databaseCalls.Load())
			require.Zero(t, h.fetcher.sourceCalls.Load())
			stored, err := h.store.IssueSyncBindingByProject(context.Background(), h.project.ID)
			require.NoError(t, err)
			require.Equal(t, binding.ID, stored.ID)
			require.Equal(t, "github", stored.Provider)
			require.Equal(t, enabled, stored.Enabled)
			require.Zero(t, h.wakes.Load())
			require.Zero(t, h.githubWakes.Load())
		})
	}
}

func TestNotionEnableFederationChangeDuringValidationRemainsFenced(t *testing.T) {
	h := newNotionLifecycleHarness(t, nil)
	h.fetcher.beforeSource = func() {
		setNotionTestFederationBinding(t, h, db.FederationRoleSpoke, true)
	}
	resp, body := postJSON(t, h.server, issueSyncEndpoint(h.project.ID, "notion", "enable"), notionEnableBody())
	assertAPIError(t, resp.StatusCode, body, http.StatusConflict, "issue_sync_federation_conflict")
	require.Contains(t, string(body), "enable Notion sync on the hub project")
	_, err := h.store.IssueSyncBindingByProject(context.Background(), h.project.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
	require.Zero(t, h.wakes.Load())
	require.Zero(t, h.githubWakes.Load())
}

func TestNotionEnableIntervalPresence(t *testing.T) {
	for _, tc := range []struct {
		name          string
		fields        map[string]any
		want, seconds int
	}{
		{"omitted", nil, 200, 300}, {"seconds", map[string]any{"interval_seconds": 120}, 200, 120}, {"duration", map[string]any{"interval": "2m"}, 200, 120},
		{"zero", map[string]any{"interval_seconds": 0}, 400, 0}, {"negative", map[string]any{"interval_seconds": -1}, 400, 0}, {"conflict zero", map[string]any{"interval_seconds": 0, "interval": "2m"}, 400, 0},
		{"conflict", map[string]any{"interval_seconds": 120, "interval": "2m"}, 400, 0}, {"subsecond", map[string]any{"interval": "999ms"}, 400, 0},
		{"unknown input", map[string]any{"token": "secret"}, 400, 0},
		{"null", map[string]any{"interval_seconds": nil}, 400, 0}, {"string", map[string]any{"interval_seconds": "120"}, 400, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newNotionLifecycleHarness(t, nil)
			body := notionEnableBody()
			maps.Copy(body, tc.fields)
			out := h.enable(t, body, tc.want)
			if tc.want == 200 {
				require.Equal(t, tc.seconds, out.Binding.IntervalSeconds)
				out = h.enable(t, map[string]any{}, 200)
				require.Equal(t, tc.seconds, out.Binding.IntervalSeconds)
			}
		})
	}
}
func TestNotionEnableSincePresence(t *testing.T) {
	h := newNotionLifecycleHarness(t, nil)
	body := notionEnableBody()
	body["config"].(map[string]any)["since"] = "2026-01-01"
	h.enable(t, body, 200)
	binding, err := h.store.IssueSyncBindingByProject(context.Background(), h.project.ID)
	require.NoError(t, err)
	started := time.Now().UTC().Truncate(time.Second)
	_, claimed, err := h.store.ClaimIssueSyncBinding(context.Background(), binding.ID, "notion", started, started.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, claimed)
	_, err = h.store.RecordIssueSyncSuccess(context.Background(), db.IssueSyncSuccessParams{BindingID: binding.ID, StartedAt: started, At: started, CursorAt: started})
	require.NoError(t, err)
	out := h.enable(t, map[string]any{}, 200)
	require.Equal(t, "2026-01-01T00:00:00Z", out.Binding.Config["since"])
	binding, err = h.store.IssueSyncBindingByProject(context.Background(), h.project.ID)
	require.NoError(t, err)
	require.NotNil(t, binding.LastCursorAt)
	out = h.enable(t, map[string]any{"config": map[string]any{"since": ""}}, 200)
	require.Equal(t, "", out.Binding.Config["since"])
	binding, err = h.store.IssueSyncBindingByProject(context.Background(), h.project.ID)
	require.NoError(t, err)
	require.Nil(t, binding.LastCursorAt)
	_, claimed, err = h.store.ClaimIssueSyncBinding(context.Background(), binding.ID, "notion", started, started.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, claimed)
	_, err = h.store.RecordIssueSyncSuccess(context.Background(), db.IssueSyncSuccessParams{BindingID: binding.ID, StartedAt: started, At: started, CursorAt: started})
	require.NoError(t, err)
	out = h.enable(t, map[string]any{"config": map[string]any{"since": "2026-02-01T01:00:00+01:00"}}, 200)
	require.Equal(t, "2026-02-01T00:00:00Z", out.Binding.Config["since"])
	binding, err = h.store.IssueSyncBindingByProject(context.Background(), h.project.ID)
	require.NoError(t, err)
	require.Nil(t, binding.LastCursorAt)
}
func TestNotionEnableImmutableMapping(t *testing.T) {
	h := newNotionLifecycleHarness(t, nil)
	body := notionEnableBody()
	body["config"].(map[string]any)["done_statuses"] = []string{"Delivered", "Closed"}
	h.enable(t, body, 200)
	h.enable(t, map[string]any{"config": map[string]any{"done_statuses": []string{"closed", "done"}}}, 200)
	h.fetcher.source.Properties = append(h.fetcher.source.Properties, notionsync.Property{ID: "other", Name: "Other", Type: "people"})
	for _, config := range []map[string]any{{"done_statuses": []string{"done"}}, {"assignee_property": "other"}, {"data_source_id": notionDatabaseID}, {"since": 3}, {"done_statuses": "done"}, {"token_env": "OTHER_TOKEN"}, {"token": "secret"}, {"api_url": "https://example.com"}} {
		h.enable(t, map[string]any{"config": config}, 400)
	}
	h.fetcher.source.ID = notionDatabaseID
	h.fetcher.database.DataSources = []notionsync.Option{{ID: notionDatabaseID, Name: "Other source"}}
	h.enable(t, map[string]any{"config": map[string]any{"data_source_id": notionDatabaseID}}, 400)
	binding, err := h.store.IssueSyncBindingByProject(context.Background(), h.project.ID)
	require.NoError(t, err)
	require.Equal(t, notionSourceID, binding.RemoteID)
}

func TestNotionEnableConcurrentConfig(t *testing.T) {
	for _, scenario := range []string{"absent", "since", "interval"} {
		t.Run(scenario, func(t *testing.T) {
			h := newNotionLifecycleHarness(t, nil)
			if scenario != "absent" {
				h.enable(t, notionEnableBody(), 200)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			var calls atomic.Int32
			h.fetcher.beforeSource = func() {
				if calls.Add(1) == 1 {
					close(entered)
					<-release
				}
			}
			stale := notionEnableBody()
			stale["config"].(map[string]any)["since"] = "2026-01-01"
			response := make(chan int, 1)
			go func() {
				resp, _ := postJSON(t, h.server, issueSyncEndpoint(h.project.ID, "notion", "enable"), stale)
				response <- resp.StatusCode
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("enable did not reach validation")
			}
			accepted := notionEnableBody()
			if scenario == "interval" {
				accepted["interval_seconds"] = 120
			} else {
				accepted["config"].(map[string]any)["since"] = "2026-02-01"
			}
			out := h.enable(t, accepted, 200)
			wakes := h.wakes.Load()
			close(release)
			select {
			case status := <-response:
				require.Equal(t, 409, status)
			case <-time.After(5 * time.Second):
				t.Fatal("stale enable did not finish")
			}
			after, err := h.store.IssueSyncBindingByProject(context.Background(), h.project.ID)
			require.NoError(t, err)
			require.Equal(t, out.Binding.IntervalSeconds, after.IntervalSeconds)
			config, err := notionsync.DecodeConfig(after.Config)
			require.NoError(t, err)
			require.Equal(t, out.Binding.Config["since"], config.Since)
			require.Equal(t, wakes, h.wakes.Load())
		})
	}
}

func TestNotionOnceUsesOnlyNotionRunnerAndDefaultProgress(t *testing.T) {
	var githubRuns atomic.Int32
	h := newNotionLifecycleHarness(t, func(cfg *daemon.ServerConfig) {
		cfg.GitHubSyncRunnerFactory = func(c daemon.GitHubSyncRunnerConfig) daemon.GitHubSyncRunner {
			githubRuns.Add(1)
			return daemon.NewDefaultGitHubSyncRunner(c)
		}
	})
	h.enable(t, notionEnableBody(), 200)
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	h.fetcher.beforeSource = func() { close(entered); <-release }
	finished := make(chan int, 1)
	go func() {
		resp, _ := postJSON(t, h.server, issueSyncEndpoint(h.project.ID, "notion", "once"), map[string]any{})
		finished <- resp.StatusCode
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not start")
	}
	resp, raw := getStatusBody(t, h.server, issueSyncEndpoint(h.project.ID, "notion", "status"))
	require.Equal(t, 200, resp.StatusCode, string(raw))
	var out issueSyncResponseBody
	decodeJSON(t, raw, &out)
	require.Equal(t, "running", out.Status.State)
	require.NotNil(t, out.Status.Progress)
	require.Equal(t, "source", out.Status.Progress.Phase)
	close(release)
	require.Equal(t, 200, <-finished)
	require.Zero(t, githubRuns.Load())
}
func TestNotionStatusSelectsProviderProgress(t *testing.T) {
	notionProgress := issuesync.NewProgressTracker()
	githubProgress := githubsync.NewProgressTracker()
	h := newNotionLifecycleHarness(t, func(cfg *daemon.ServerConfig) {
		cfg.NotionSyncProgress = notionProgress
		cfg.GitHubSyncProgress = githubProgress
	})
	out := h.enable(t, notionEnableBody(), 200)
	started := time.Now().UTC().Truncate(time.Second)
	_, claimed, err := h.store.ClaimIssueSyncBinding(context.Background(), out.Binding.ID, "notion", started, started.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, claimed)
	notionProgress.BeginWithPhase(out.Binding.ID, started, "content")
	githubProgress.Begin(out.Binding.ID, started)
	resp, raw := getStatusBody(t, h.server, issueSyncEndpoint(h.project.ID, "notion", "status"))
	require.Equal(t, 200, resp.StatusCode, string(raw))
	decodeJSON(t, raw, &out)
	require.NotNil(t, out.Status.Progress)
	require.Equal(t, "content", out.Status.Progress.Phase)
	resp, raw = getStatusBody(t, h.server, issueSyncEndpoint(h.project.ID, "github", "status"))
	require.Equal(t, 200, resp.StatusCode, string(raw))
	out = issueSyncResponseBody{}
	decodeJSON(t, raw, &out)
	require.Nil(t, out.Binding)
	require.Equal(t, "not_enabled", out.Status.State)
}
func TestNotionRouteAuthority(t *testing.T) {
	for _, mode := range []string{"readonly", "bootstrap"} {
		t.Run(mode, func(t *testing.T) {
			options := []testenv.Option{testenv.WithInsecureReadonly()}
			headers := map[string]string{}
			if mode == "bootstrap" {
				options = []testenv.Option{testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity()}
				headers = bearer("bootstrap-token")
			}
			env := testenv.New(t, options...)
			project, err := env.DB.CreateProject(context.Background(), "example-project")
			require.NoError(t, err)
			for _, action := range []string{"enable", "disable", "once"} {
				body := map[string]any{}
				if action == "enable" {
					body = notionEnableBody()
				}
				resp, raw := envDoRaw(t, env, http.MethodPost, issueSyncEndpoint(project.ID, "notion", action), body, headers)
				want := 403
				if mode == "readonly" {
					want = 401
				}
				require.Equal(t, want, resp.StatusCode, string(raw))
			}
			_, err = env.DB.IssueSyncBindingByProject(context.Background(), project.ID)
			require.ErrorIs(t, err, db.ErrNotFound)
		})
	}
	t.Run("project scope", func(t *testing.T) {
		d := openTestDB(t)
		project, err := d.db.CreateProject(context.Background(), "example-project")
		require.NoError(t, err)
		server := daemon.NewServer(daemon.ServerConfig{DB: d.db, HostAccess: commentProjectHostAccess{deniedProjectID: project.ID}, NotionSyncFetcher: newLifecycleNotionFetcher()})
		t.Cleanup(func() { _ = server.Close() })
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			server.Handler().ServeHTTP(w, r.WithContext(daemon.WithPrincipal(r.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Actor: "agent", Subject: "example-user"})))
		}))
		t.Cleanup(ts.Close)
		for _, action := range []string{"enable", "disable", "once"} {
			resp, raw := postJSON(t, ts, issueSyncEndpoint(project.ID, "notion", action), notionEnableBody())
			require.Equal(t, 404, resp.StatusCode, string(raw))
		}
		resp, raw := getStatusBody(t, ts, issueSyncEndpoint(project.ID, "notion", "status"))
		require.Equal(t, 404, resp.StatusCode, string(raw))
	})
	t.Run("browser local", func(t *testing.T) {
		d := openTestDB(t)
		project, err := d.db.CreateProject(context.Background(), "example-project")
		require.NoError(t, err)
		const origin = "http://127.0.0.1:27123"
		manager, err := daemon.NewWebSessionManager(daemon.WebSessionManagerConfig{Origin: origin, InstanceID: "instance_a", Writable: true, DB: d.db})
		require.NoError(t, err)
		issued, err := manager.IssueSession(daemon.Principal{Kind: daemon.PrincipalWebLocal}, "/")
		require.NoError(t, err)
		server := daemon.NewServer(daemon.ServerConfig{DB: d.db, WebSessions: manager, NotionSyncFetcher: newLifecycleNotionFetcher()})
		t.Cleanup(func() { _ = server.Close() })
		handler, err := server.HandlerFor(daemon.ListenerPolicy{Kind: daemon.ListenerBrowser, Origin: origin, RequireBrowserSession: true, AllowLocalSession: true})
		require.NoError(t, err)
		for _, action := range []string{"enable", "disable", "once"} {
			request := httptest.NewRequest(http.MethodPost, origin+issueSyncEndpoint(project.ID, "notion", action), nil)
			request.AddCookie(manager.Cookie(issued.Cookie))
			request.Header.Set("X-Kata-Web-Session", issued.Session)
			request.Header.Set("X-Kata-CSRF", issued.CSRF)
			request.Header.Set("Origin", origin)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			require.Equal(t, 403, response.Code, response.Body.String())
		}
	})
}

func TestNotionEnableDefaultClientUsesDaemonTokenEnv(t *testing.T) {
	t.Setenv("KATA_NOTION_TOKEN", "unrelated-default-secret")
	t.Setenv("EXAMPLE_NOTION_TOKEN", "")
	h := newNotionLifecycleHarness(t, func(cfg *daemon.ServerConfig) {
		cfg.NotionSyncFetcher = nil
		cfg.NotionSyncConfig.TokenEnv = "EXAMPLE_NOTION_TOKEN"
	})
	resp, raw := postJSON(t, h.server, issueSyncEndpoint(h.project.ID, "notion", "enable"), notionEnableBody())
	require.Equal(t, 400, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), "credentials are unavailable")
	require.NotContains(t, string(raw), "unrelated-default-secret")
	require.Zero(t, h.wakes.Load())
}

func TestNotionEnableTitlePrefixPresence(t *testing.T) {
	h := newNotionLifecycleHarness(t, nil)
	out := h.enable(t, notionEnableBody(), http.StatusOK)
	require.Equal(t, true, out.Binding.Config["title_prefix"])
	want := true
	for _, config := range []map[string]any{{"title_prefix": false}, {}, {"title_prefix": true}, {}} {
		out = h.enable(t, map[string]any{"config": config}, http.StatusOK)
		choice, present := config["title_prefix"]
		if present {
			want = choice.(bool)
		}
		require.Equal(t, want, out.Binding.Config["title_prefix"])
		stored, err := h.store.IssueSyncBindingByProject(context.Background(), h.project.ID)
		require.NoError(t, err)
		decoded, err := notionsync.DecodeConfig(stored.Config)
		require.NoError(t, err)
		require.Equal(t, want, decoded.UseTitlePrefix())
		require.Equal(t, notionSourceID, decoded.DataSourceID)
		require.Equal(t, "status", decoded.StatusPropertyID)
		require.Equal(t, []string{"done"}, decoded.DoneStatusIDs)
	}
	for _, value := range []any{nil, "false", 0, []any{}, map[string]any{}} {
		before := h.fetcher.forRunCalls.Load()
		h.enable(t, map[string]any{"config": map[string]any{"title_prefix": value}}, http.StatusBadRequest)
		require.Equal(t, before, h.fetcher.forRunCalls.Load())
	}
}
