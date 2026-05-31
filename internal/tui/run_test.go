package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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
				ProjectSummary: ProjectSummary{ID: 7, Name: "kata"},
				Stats:          &ProjectStatsSummary{Open: 3, Closed: 1, LastEventAt: &t1},
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
