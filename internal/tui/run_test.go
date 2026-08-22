package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBoot_ResolvesProject covers §4.2 case 1: cwd is bound to a registered
// project. bootResolveScope should return single-project scope, and the
// initial list fetch should hit the project-scoped endpoint.
func TestBoot_ResolvesProject(t *testing.T) {
	var sawList bool
	srv := mockDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/projects/resolve": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"project": map[string]any{
					"id":       7,
					"identity": "github.com/wesm/kata",
					"name":     "kata",
				},
				"workspace_root": "/tmp/x",
			})
		},
		"/api/v1/projects/7/issues": func(w http.ResponseWriter, _ *http.Request) {
			sawList = true
			_ = json.NewEncoder(w).Encode(map[string]any{"issues": []map[string]any{}})
		},
	})
	c := NewClient(srv.URL, srv.Client())
	bi, err := bootResolveScope(t.Context(), c, "/tmp/x")
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, viewList, bi.view)
	sc := bi.scope
	if sc.allProjects {
		t.Fatal("expected single-project scope, got allProjects")
	}
	if sc.projectID != 7 {
		t.Fatalf("got projectID %d, want 7", sc.projectID)
	}
	if sc.projectName != "kata" {
		t.Fatalf("projectName = %q, want kata", sc.projectName)
	}
	if sc.workspace != "/tmp/x" {
		t.Fatalf("workspace = %q, want /tmp/x", sc.workspace)
	}
	if sc.homeProjectID != 7 || sc.homeProjectName != "kata" {
		t.Fatalf("home* not seeded: id=%d name=%q", sc.homeProjectID, sc.homeProjectName)
	}
	if _, err := c.ListIssues(t.Context(), sc.projectID, ListFilter{}); err != nil {
		t.Fatal(err)
	}
	if !sawList {
		t.Fatal("expected list endpoint to have been hit")
	}
}

// TestBoot_EmptyState_NoProjectsRegistered covers §4.2 case 3: cwd is
// unbound and no projects are registered. bootResolveScope should land
// on viewEmpty so Run renders an onboarding hint instead of a blank
// list. (The companion case-2 test is
// TestBoot_UnresolvedWithProjects_LandsViewProjects below, which pins
// the ≥1 project branch.)
func TestBoot_EmptyState_NoProjectsRegistered(t *testing.T) {
	srv := mockDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/projects/resolve": projectNotInitializedHandler,
		"/api/v1/projects": func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "stats", r.URL.Query().Get("include"))
			_, _ = w.Write([]byte(`{"projects":[]}`))
		},
	})
	c := NewClient(srv.URL, srv.Client())
	bi, err := bootResolveScope(t.Context(), c, "/tmp/empty")
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, viewEmpty, bi.view)
	if !bi.scope.empty {
		t.Fatal("expected scope.empty=true")
	}
	if bi.scope.allProjects {
		t.Fatal("did not expect allProjects")
	}
}

// TestBoot_NonResolveErrorPropagates: a 500 from /resolve should fail Run
// instead of silently downgrading. Black-screen prevention.
func TestBoot_NonResolveErrorPropagates(t *testing.T) {
	srv := mockDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/projects/resolve": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"status":500,"error":{"code":"internal","message":"db down"}}`))
		},
	})
	c := NewClient(srv.URL, srv.Client())
	if _, err := bootResolveScope(t.Context(), c, "/tmp/x"); err == nil {
		t.Fatal("expected error to propagate, got nil")
	}
}

// TestInitialFilter_ZeroValueByDefault asserts the boot-time filter is
// the zero value: today there's no Options field that drives initial
// filter state. The shape is preserved so a future task can wire one up
// without changing fetchInitial.
func TestInitialFilter_ZeroValueByDefault(t *testing.T) {
	got := initialFilter(Options{})
	if got.Status != "" || got.Owner != "" || got.Author != "" ||
		got.Search != "" || len(got.Labels) != 0 {
		t.Fatalf("initialFilter = %+v, want zero value", got)
	}
}

type fakeInitialIssueAPI struct {
	detail       *IssueDetail
	err          error
	projects     []ProjectSummary
	listErr      error
	listProjects int
	projectID    int64
	ref          string
}

func (f *fakeInitialIssueAPI) GetIssueDetail(
	_ context.Context, projectID int64, ref string,
) (*IssueDetail, error) {
	f.projectID = projectID
	f.ref = ref
	return f.detail, f.err
}

func (f *fakeInitialIssueAPI) ListProjects(context.Context) ([]ProjectSummary, error) {
	f.listProjects++
	return f.projects, f.listErr
}

func TestResolveInitialIssue_FetchesBareRefInWorkspaceProject(t *testing.T) {
	api := &fakeInitialIssueAPI{
		detail: &IssueDetail{
			Issue: &Issue{
				ProjectID: 7,
				UID:       "01TEST00000000000000000001",
				ShortID:   "abc4",
				Title:     "Target",
			},
		},
	}
	bi := bootInit{scope: scope{projectID: 7}, view: viewList}

	got, err := resolveInitialIssue(t.Context(), api, bi, "abc4")
	require.NoError(t, err)
	assert.Equal(t, int64(7), api.projectID)
	assert.Equal(t, "abc4", api.ref)
	assert.Zero(t, api.listProjects)
	require.NotNil(t, got.initialIssue)
	assert.Equal(t, "abc4", got.initialIssue.ShortID)
}

func TestResolveInitialIssue_QualifiedRefSelectsNamedProject(t *testing.T) {
	api := &fakeInitialIssueAPI{
		projects: []ProjectSummary{
			{ID: 7, Name: "workspace"},
			{ID: 9, Name: "other-project", UID: "01TARGETPROJECT000000000000"},
		},
		detail: &IssueDetail{
			Issue: &Issue{
				ProjectID: 9,
				UID:       "01TEST00000000000000000001",
				ShortID:   "abc4",
				Title:     "Target",
			},
		},
	}
	bi := bootInit{
		scope: scope{projectID: 7, projectName: "workspace", workspace: "/tmp/workspace"},
		view:  viewList,
	}

	got, err := resolveInitialIssue(t.Context(), api, bi, "other-project#abc4")
	require.NoError(t, err)
	assert.Equal(t, 1, api.listProjects)
	assert.Equal(t, int64(9), api.projectID)
	assert.Equal(t, "abc4", api.ref)
	assert.Equal(t, int64(9), got.scope.projectID)
	assert.Equal(t, "other-project", got.scope.projectName)
	assert.Equal(t, int64(9), got.scope.homeProjectID)
	assert.Equal(t, "other-project", got.scope.homeProjectName)
	assert.Equal(t, viewList, got.view)
	require.Len(t, got.projects, 1)
	assert.Equal(t, int64(9), got.projects[0].ID)
}

func TestResolveInitialIssue_QualifiedRefDoesNotRequireWorkspaceScope(t *testing.T) {
	api := &fakeInitialIssueAPI{
		projects: []ProjectSummary{{ID: 9, Name: "other-project"}},
		detail: &IssueDetail{
			Issue: &Issue{
				ProjectID: 9,
				UID:       "01TEST00000000000000000001",
				ShortID:   "abc4",
				Title:     "Target",
			},
		},
	}

	got, err := resolveInitialIssue(
		t.Context(), api, bootInit{view: viewProjects}, "other-project#abc4",
	)
	require.NoError(t, err)
	assert.Equal(t, int64(9), got.scope.projectID)
	assert.False(t, got.scope.empty)
	assert.Equal(t, viewList, got.view)
	require.NotNil(t, got.initialIssue)
	assert.Equal(t, "abc4", got.initialIssue.ShortID)
}

func TestResolveInitialIssue_EmptyRefDoesNotFetch(t *testing.T) {
	api := &fakeInitialIssueAPI{}
	bi := bootInit{scope: scope{projectID: 7}, view: viewList}

	got, err := resolveInitialIssue(t.Context(), api, bi, "")
	require.NoError(t, err)
	assert.Empty(t, api.ref)
	assert.Nil(t, got.initialIssue)
}

func TestResolveInitialIssue_PropagatesLookupError(t *testing.T) {
	want := errors.New("issue not found")
	api := &fakeInitialIssueAPI{err: want}
	bi := bootInit{scope: scope{projectID: 7}, view: viewList}

	_, err := resolveInitialIssue(t.Context(), api, bi, "abcd")
	assert.ErrorIs(t, err, want)
}

func TestResolveInitialIssue_RequiresProjectScope(t *testing.T) {
	api := &fakeInitialIssueAPI{}

	_, err := resolveInitialIssue(t.Context(), api, bootInit{view: viewProjects}, "abc4")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no project bound to this workspace")
	assert.Empty(t, api.ref)
}

func TestBootClient_ResolvesRequestedIssueBeforeReturning(t *testing.T) {
	srv := mockDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/projects/7/issues/abc4": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issue": map[string]any{
					"id": 1, "project_id": 7,
					"uid": "01TEST00000000000000000001", "short_id": "abc4",
					"title": "Target", "status": "open",
				},
				"children": []map[string]any{},
			})
		},
	})
	client := NewClient(srv.URL, srv.Client())
	old := bootDaemonConnectionForTUI
	bootDaemonConnectionForTUI = func(context.Context, Options) (daemonConnection, error) {
		return daemonConnection{
			api:      client,
			sseHC:    srv.Client(),
			endpoint: srv.URL,
			init: bootInit{
				scope: scope{projectID: 7, projectName: "example-project"},
				view:  viewList,
			},
		}, nil
	}
	t.Cleanup(func() { bootDaemonConnectionForTUI = old })

	_, _, bi, _, _, err := bootClient(t.Context(), Options{InitialIssueRef: "abc4"})

	require.NoError(t, err)
	require.NotNil(t, bi.initialIssue)
	assert.Equal(t, "abc4", bi.initialIssue.ShortID)
}

func TestBootClient_QualifiedRefUsesNamedProjectOverWorkspace(t *testing.T) {
	srv := mockDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/projects": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"projects":[
				{"id":7,"name":"workspace"},
				{"id":9,"uid":"01TARGETPROJECT000000000000","name":"other-project"}
			]}`))
		},
		"/api/v1/projects/9/issues/abc4": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issue": map[string]any{
					"id": 1, "project_id": 9,
					"uid": "01TEST00000000000000000001", "short_id": "abc4",
					"title": "Target", "status": "open",
				},
				"children": []map[string]any{},
			})
		},
	})
	client := NewClient(srv.URL, srv.Client())
	old := bootDaemonConnectionForTUI
	bootDaemonConnectionForTUI = func(context.Context, Options) (daemonConnection, error) {
		return daemonConnection{
			api:      client,
			sseHC:    srv.Client(),
			endpoint: srv.URL,
			init: bootInit{
				scope: scope{projectID: 7, projectName: "workspace"},
				view:  viewList,
			},
		}, nil
	}
	t.Cleanup(func() { bootDaemonConnectionForTUI = old })

	_, _, bi, _, _, err := bootClient(
		t.Context(), Options{
			ProjectName:     "missing-project",
			InitialIssueRef: "other-project#abc4",
		},
	)

	require.NoError(t, err)
	assert.Equal(t, int64(9), bi.scope.projectID)
	assert.Equal(t, "other-project", bi.scope.projectName)
	require.NotNil(t, bi.initialIssue)
	assert.Equal(t, int64(9), bi.initialIssue.ProjectID)
}

func TestBootClient_ProjectSelectorScopesBareRef(t *testing.T) {
	srv := mockDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/projects": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"projects":[
				{"id":7,"name":"workspace"},
				{"id":9,"name":"project-b"}
			]}`))
		},
		"/api/v1/projects/9/issues/abc4": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issue": map[string]any{
					"id": 1, "project_id": 9,
					"uid": "01TEST00000000000000000001", "short_id": "abc4",
					"title": "Target", "status": "open",
				},
				"children": []map[string]any{},
			})
		},
	})
	client := NewClient(srv.URL, srv.Client())
	old := bootDaemonConnectionForTUI
	bootDaemonConnectionForTUI = func(context.Context, Options) (daemonConnection, error) {
		return daemonConnection{
			api:  client,
			init: bootInit{scope: scope{projectID: 7, projectName: "workspace"}, view: viewList},
		}, nil
	}
	t.Cleanup(func() { bootDaemonConnectionForTUI = old })

	_, _, bi, _, _, err := bootClient(t.Context(), Options{
		ProjectName:     "project-b",
		InitialIssueRef: "abc4",
	})

	require.NoError(t, err)
	assert.Equal(t, int64(9), bi.scope.projectID)
	require.NotNil(t, bi.initialIssue)
	assert.Equal(t, int64(9), bi.initialIssue.ProjectID)
}

func TestBootClient_WorkspaceSelectorScopesBareRef(t *testing.T) {
	workspace := t.TempDir()
	srv := mockDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/projects/9/issues/abc4": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issue": map[string]any{
					"id": 1, "project_id": 9,
					"uid": "01TEST00000000000000000001", "short_id": "abc4",
					"title": "Target", "status": "open",
				},
				"children": []map[string]any{},
			})
		},
	})
	client := NewClient(srv.URL, srv.Client())
	old := bootDaemonConnectionForTUI
	bootDaemonConnectionForTUI = func(context.Context, Options) (daemonConnection, error) {
		return daemonConnection{
			api:  client,
			init: bootInit{scope: scope{projectID: 9, projectName: "project-b"}, view: viewList},
		}, nil
	}
	t.Cleanup(func() { bootDaemonConnectionForTUI = old })

	_, _, bi, _, _, err := bootClient(t.Context(), Options{
		Workspace:       workspace,
		InitialIssueRef: "abc4",
	})

	require.NoError(t, err)
	assert.Equal(t, int64(9), bi.scope.projectID)
	require.NotNil(t, bi.initialIssue)
	assert.Equal(t, int64(9), bi.initialIssue.ProjectID)
}

func TestBootClient_QualifiedRefWorksFromUnboundWorkspace(t *testing.T) {
	srv := mockDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/projects": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(
				`{"projects":[{"id":9,"name":"other-project"}]}`,
			))
		},
		"/api/v1/projects/9/issues/abc4": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issue": map[string]any{
					"id": 1, "project_id": 9,
					"uid": "01TEST00000000000000000001", "short_id": "abc4",
					"title": "Target", "status": "open",
				},
				"children": []map[string]any{},
			})
		},
	})
	client := NewClient(srv.URL, srv.Client())
	old := bootDaemonConnectionForTUI
	bootDaemonConnectionForTUI = func(context.Context, Options) (daemonConnection, error) {
		return daemonConnection{
			api:      client,
			sseHC:    srv.Client(),
			endpoint: srv.URL,
			init:     bootInit{view: viewProjects},
		}, nil
	}
	t.Cleanup(func() { bootDaemonConnectionForTUI = old })

	_, _, bi, _, _, err := bootClient(
		t.Context(), Options{InitialIssueRef: "other-project#abc4"},
	)

	require.NoError(t, err)
	assert.Equal(t, int64(9), bi.scope.projectID)
	assert.Equal(t, viewList, bi.view)
	require.NotNil(t, bi.initialIssue)
	assert.Equal(t, "abc4", bi.initialIssue.ShortID)
}

// TestOutputIsTerminal_RejectsNonFile confirms a non-*os.File writer
// (e.g., bytes.Buffer in tests) is treated as a non-terminal so Run
// surfaces errNotATTY instead of writing alt-screen control sequences
// into a buffer that cannot honor them.
func TestOutputIsTerminal_RejectsNonFile(t *testing.T) {
	var buf bytes.Buffer
	if outputIsTerminal(&buf) {
		t.Fatal("outputIsTerminal(*bytes.Buffer) = true, want false")
	}
}

// TestRun_NonFileStdout_ReturnsNotATTY: piping into a bytes.Buffer (the
// natural test rig) must surface errNotATTY rather than panicking deep
// inside Bubble Tea's renderer.
func TestRun_NonFileStdout_ReturnsNotATTY(t *testing.T) {
	var buf bytes.Buffer
	err := Run(t.Context(), Options{Stdout: &buf})
	if !errors.Is(err, errNotATTY) {
		t.Fatalf("Run returned %v, want errNotATTY", err)
	}
}

func TestSSERestartIgnoresStaleGeneration(t *testing.T) {
	root, cancelRoot := context.WithCancel(context.Background())
	t.Cleanup(cancelRoot)

	var cancelled int
	var started []uint64
	state := newSSERestartState(root, func() {
		cancelled++
	}, func(_ context.Context, _ sseClient, _ string, _ *int64, _ chan tea.Msg, gen uint64) {
		started = append(started, gen)
	})
	conn := daemonConnection{
		sseHC:    &http.Client{},
		endpoint: "https://daemon.example",
		init:     bootInit{scope: homedScope(7, "kata")},
	}

	newer := state.restart(conn, 3, nil)
	older := state.restart(conn, 2, nil)

	newer()
	older()

	assert.Equal(t, 1, cancelled)
	assert.Equal(t, []uint64{3}, started)
}

// TestBoot_UnresolvedWithProjects_LandsViewProjects pins the new boot
// rule: an unresolved cwd plus ≥1 registered project lands on
// viewProjects, not viewEmpty. Spec §4.2.
func TestBoot_UnresolvedWithProjects_LandsViewProjects(t *testing.T) {
	srv := mockDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/projects/resolve": projectNotInitializedHandler,
		"/api/v1/projects": func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "stats", r.URL.Query().Get("include"))
			_, _ = w.Write([]byte(`{"projects":[
				{"id":7,"identity":"github.com/wesm/kata","name":"kata",
				 "stats":{"open":3,"closed":1,"last_event_at":"2026-05-04T12:00:00.000Z"}}
			]}`))
		},
	})
	c := NewClient(srv.URL, srv.Client())

	bi, err := bootResolveScope(t.Context(), c, "/tmp/unbound")
	require.NoError(t, err)
	assert.Equal(t, viewProjects, bi.view)
	assert.False(t, bi.scope.empty)
	assert.Zero(t, bi.scope.projectID)
	assert.False(t, bi.scope.allProjects)
	require.Len(t, bi.projects, 1, "boot fetched rows must be threaded through")
	assert.Equal(t, int64(7), bi.projects[0].ID)
	assert.Equal(t, "kata", bi.projects[0].Name)
}

func TestBootRemoteUnboundPathLandsOnProjects(t *testing.T) {
	srv := mockDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/projects": func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "stats", r.URL.Query().Get("include"))
			_, _ = w.Write([]byte(`{"projects":[{"id":7,"name":"example-workspace"}]}`))
		},
	})
	c := NewClient(srv.URL, srv.Client())

	bi, err := bootResolveScopePathFree(t.Context(), c, t.TempDir())

	require.NoError(t, err)
	assert.Equal(t, viewProjects, bi.view)
	require.Len(t, bi.projects, 1)
	assert.Equal(t, "example-workspace", bi.projects[0].Name)
}

// TestBootScopedResolveSeedsProjectUID: a TUI launched inside a project must
// know that project's UID from the first frame. The adopt-first enroll flow's
// rejoin detection reads projectUIDByID; before the async project-list fetch
// lands, an unseeded cache would let a post-leave adoption rewrite the
// project's history instead of rebinding it.
func TestBootScopedResolveSeedsProjectUID(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/projects/resolve", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(
			`{"project":{"id":7,"uid":"01HZNQ7VFPK1XGD8R5MABCD4EX","name":"spoke-project"},"workspace_root":"/tmp/ws"}`))
	})

	bi, err := bootResolveScope(t.Context(), c, "/tmp/ws")
	require.NoError(t, err)
	assert.Equal(t, viewList, bi.view)
	assert.Equal(t, int64(7), bi.scope.projectID)

	m := buildRunModel(Options{}, c, bi)
	assert.Equal(t, "spoke-project", m.projectsByID[7])
	assert.Equal(t, "01HZNQ7VFPK1XGD8R5MABCD4EX", m.projectUIDByID[7],
		"scoped boot must seed the resolved project's UID for rejoin detection")
}

// TestBuildRunModel_SeedsViewProjectsCacheFromBoot pins that when boot
// lands on viewProjects, the initial model's cache maps are populated
// from the boot fetch — no empty-then-fill flicker on the first frame.
// Spec §4.3.
func TestBuildRunModel_SeedsViewProjectsCacheFromBoot(t *testing.T) {
	t1 := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	bi := bootInit{
		view:  viewProjects,
		scope: scope{},
		projects: []ProjectSummaryWithStats{
			{
				ID: 7, Name: "kata",
				Stats: &ProjectStatsSummary{Open: 3, Closed: 1, LastEventAt: &t1},
			},
		},
	}
	m := buildRunModel(Options{}, &Client{}, bi)
	assert.Equal(t, viewProjects, m.view)
	assert.Equal(t, "kata", m.projectsByID[7])
	assert.Equal(t, "kata", m.projectIdentByID[7])
	require.Contains(t, m.projectStats, int64(7))
	assert.Equal(t, 3, m.projectStats[7].Open)
	assert.Equal(t, 1, m.projectStats[7].Closed)
}

func TestBuildRunModel_SeedsInitialIssue(t *testing.T) {
	bi := bootInit{
		scope: scope{projectID: 7, projectName: "example-project"},
		view:  viewList,
		initialIssue: &Issue{
			ProjectID: 7,
			UID:       "01TEST00000000000000000001",
			ShortID:   "abc4",
			Title:     "Target",
		},
	}

	m := buildRunModel(Options{InitialIssueRef: "abc4"}, &Client{}, bi)

	assert.Equal(t, viewDetail, m.view)
	require.NotNil(t, m.detail.issue)
	assert.Equal(t, "abc4", m.detail.issue.ShortID)
	assert.Equal(t, int64(7), m.detail.scopePID)
}

func TestInitialDetail_InitAddsFourDetailFetches(t *testing.T) {
	baseInit := bootInit{
		scope: scope{projectID: 7, projectName: "example-project"},
		view:  viewList,
	}
	directInit := baseInit
	directInit.initialIssue = &Issue{
		ProjectID: 7,
		UID:       "01TEST00000000000000000001",
		ShortID:   "abc4",
		Title:     "Target",
	}

	baseBatch, ok := buildRunModel(Options{}, &Client{}, baseInit).Init()().(tea.BatchMsg)
	require.True(t, ok)
	directBatch, ok := buildRunModel(
		Options{InitialIssueRef: "abc4"}, &Client{}, directInit,
	).Init()().(tea.BatchMsg)
	require.True(t, ok)
	require.Len(t, directBatch, len(baseBatch)+1)

	detailBatch, ok := directBatch[len(directBatch)-1]().(tea.BatchMsg)
	require.True(t, ok)
	assert.Len(t, detailBatch, 4, "initial detail must fetch issue, comments, events, and links")
}
