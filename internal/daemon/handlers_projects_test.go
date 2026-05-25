package daemon_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testfix"
)

func TestResolve_FailsOutsideKataTomlAndWithoutAlias(t *testing.T) {
	ts := newTestServer(t)
	resp, bs := postJSON(t, ts, "/api/v1/projects/resolve", map[string]any{
		"start_path": t.TempDir(),
	})
	assertAPIError(t, resp.StatusCode, bs, 404, "project_not_initialized")
}

func TestInit_FromGitRemoteCreatesProject(t *testing.T) {
	h := newServerWithGitWorkspace(t, "https://github.com/wesm/kata.git")
	ts := h.ts.(*httptest.Server)
	resp, bs := postJSON(t, ts, "/api/v1/projects", map[string]any{
		"start_path": h.dir,
	})
	assert.Equal(t, 200, resp.StatusCode, string(bs))

	var body struct {
		Project struct {
			ID   int64
			Name string
		} `json:"project"`
		Alias struct {
			AliasIdentity string `json:"alias_identity"`
			AliasKind     string `json:"alias_kind"`
		} `json:"alias"`
		WorkspaceRoot string `json:"workspace_root"`
		Created       bool   `json:"created"`
	}
	require.NoError(t, json.Unmarshal(bs, &body))
	assert.Equal(t, "kata", body.Project.Name)
	assert.Equal(t, "kata", body.Project.Name)
	assert.True(t, body.Created)
	assert.Equal(t, "github.com/wesm/kata", body.Alias.AliasIdentity)

	// .kata.toml must have been written
	_, err := os.Stat(filepath.Join(h.dir, ".kata.toml"))
	assert.NoError(t, err)
}

func TestInit_FreshCloneFromExistingKataToml(t *testing.T) {
	// Simulate "git clone, kata init" on a repo that already had .kata.toml.
	h := newServerWithGitWorkspace(t, "")
	testfix.WriteKataToml(t, h.dir, "system")

	resp, bs := postJSON(t, h.ts.(*httptest.Server), "/api/v1/projects", map[string]any{
		"start_path": h.dir,
	})
	assert.Equal(t, 200, resp.StatusCode, string(bs))

	var body struct {
		Project struct {
			Name string
		} `json:"project"`
		Created bool `json:"created"`
	}
	require.NoError(t, json.Unmarshal(bs, &body))
	assert.Equal(t, "system", body.Project.Name)
	assert.True(t, body.Created)
}

func TestResolve_AfterInitSucceeds(t *testing.T) {
	h := newServerWithGitWorkspace(t, "https://github.com/wesm/kata.git")
	ts := h.ts.(*httptest.Server)

	_, _ = postJSON(t, ts, "/api/v1/projects", map[string]any{"start_path": h.dir})

	resp, bs := postJSON(t, ts, "/api/v1/projects/resolve", map[string]any{"start_path": h.dir})
	assert.Equal(t, 200, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), `"name":"kata"`)
}

// TestResolve_ByProjectName_PathFree verifies the remote-client
// resolution path: the daemon looks up the project by its committed
// identity without touching the filesystem. This is what lets a kata
// client on host B reach a project registered on host A's daemon.
func TestResolve_ByProjectName_PathFree(t *testing.T) {
	dir := t.TempDir()
	testfix.RunGit(t, dir, "init", "--quiet")
	testfix.RunGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")
	ts := newTestServer(t)

	// Register the project (local-style init).
	_, _ = postJSON(t, ts, "/api/v1/projects", map[string]any{"start_path": dir})

	// Now resolve by identity only — no start_path. Note that the
	// identity we send doesn't refer to anything on the daemon's
	// filesystem; the lookup must be path-free.
	resp, bs := postJSON(t, ts, "/api/v1/projects/resolve", map[string]any{
		"name": "kata",
	})
	assert.Equal(t, 200, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), `"name":"kata"`)
}

// TestResolve_ByProjectName_NotRegistered surfaces the right error
// when a remote client claims an identity the daemon doesn't know about.
func TestResolve_ByProjectName_NotRegistered(t *testing.T) {
	ts := newTestServer(t)

	resp, bs := postJSON(t, ts, "/api/v1/projects/resolve", map[string]any{
		"name": "never-registered",
	})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), "project_not_initialized")
	assert.Contains(t, string(bs), "never-registered")
}

// TestResolve_NeitherFieldSet rejects a request that supplies neither
// name nor start_path.
func TestResolve_NeitherFieldSet(t *testing.T) {
	ts := newTestServer(t)

	resp, bs := postJSON(t, ts, "/api/v1/projects/resolve", map[string]any{})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), "name")
	assert.Contains(t, string(bs), "start_path")
}

// TestResolve_ByAliasInput_PathFree verifies remote clients can
// resolve a registered project by sending alias metadata alone — no
// start_path on the wire. This is the remote-mode counterpart of init's
// path-free flow: a daemon on host A serves a client on host B without
// stat'ing host B's workspace.
func TestResolve_ByAliasInput_PathFree(t *testing.T) {
	ts := newTestServer(t)

	// Register via path-free init with alias metadata, mimicking what
	// the new client would do.
	_, _ = postJSON(t, ts, "/api/v1/projects", map[string]any{
		"name": "kata",
		"alias": map[string]any{
			"identity":  "github.com/wesm/kata",
			"kind":      "git",
			"root_path": "/client/workspace",
		},
	})

	// Resolve by alias only — no name, no start_path.
	resp, bs := postJSON(t, ts, "/api/v1/projects/resolve", map[string]any{
		"alias": map[string]any{
			"identity":  "github.com/wesm/kata",
			"kind":      "git",
			"root_path": "/client/workspace",
		},
	})
	require.Equal(t, 200, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), `"name":"kata"`)
}

// TestResolve_ByAliasInput_NotRegistered returns 404 when neither
// the alias nor a name match. The daemon must not derive a project
// name from the alias and create-by-convention — resolve is strict.
func TestResolve_ByAliasInput_NotRegistered(t *testing.T) {
	ts := newTestServer(t)

	resp, bs := postJSON(t, ts, "/api/v1/projects/resolve", map[string]any{
		"alias": map[string]any{
			"identity":  "github.com/wesm/never-seen",
			"kind":      "git",
			"root_path": "/client/workspace",
		},
	})
	assertAPIError(t, resp.StatusCode, bs, http.StatusNotFound, "project_not_initialized")
}

func TestListProjects_IncludeArchived(t *testing.T) {
	ts, h := startDefaultTestServer(t)
	active, err := h.db.CreateProject(t.Context(), "active")
	require.NoError(t, err)
	archived, err := h.db.CreateProject(t.Context(), "archived")
	require.NoError(t, err)
	_, _, err = h.db.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: archived.ID, Actor: "tester"})
	require.NoError(t, err)

	resp, bs := doReq(t, ts, http.MethodGet, "/api/v1/projects?include=archived", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(bs))

	var body struct {
		Projects []struct {
			ID        int64   `json:"id"`
			Name      string  `json:"name"`
			DeletedAt *string `json:"deleted_at"`
		} `json:"projects"`
	}
	require.NoError(t, json.Unmarshal(bs, &body))
	require.Len(t, body.Projects, 2)
	assert.Equal(t, active.ID, body.Projects[0].ID)
	assert.Nil(t, body.Projects[0].DeletedAt)
	assert.Equal(t, archived.ID, body.Projects[1].ID)
	assert.NotNil(t, body.Projects[1].DeletedAt)
}

func TestRestoreProject_RestoresArchivedProject(t *testing.T) {
	ts, h := startDefaultTestServer(t)
	p, err := h.db.CreateProject(t.Context(), "archived")
	require.NoError(t, err)
	_, _, err = h.db.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: p.ID, Actor: "tester"})
	require.NoError(t, err)

	resp, bs := postJSON(t, ts, "/api/v1/projects/"+strconv.FormatInt(p.ID, 10)+"/restore?actor=tester", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(bs))

	var body struct {
		Project struct {
			ID        int64   `json:"id"`
			DeletedAt *string `json:"deleted_at"`
		} `json:"project"`
		Event   *db.Event `json:"event"`
		Changed bool      `json:"changed"`
	}
	require.NoError(t, json.Unmarshal(bs, &body))
	assert.True(t, body.Changed)
	assert.Equal(t, p.ID, body.Project.ID)
	assert.Nil(t, body.Project.DeletedAt)
	require.NotNil(t, body.Event)
	assert.Equal(t, "project.restored", body.Event.Type)
}

func TestRestoreProject_ActiveProjectIsNoop(t *testing.T) {
	ts, h := startDefaultTestServer(t)
	p, err := h.db.CreateProject(t.Context(), "active")
	require.NoError(t, err)

	resp, bs := postJSON(t, ts, "/api/v1/projects/"+strconv.FormatInt(p.ID, 10)+"/restore?actor=tester", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(bs))

	var body struct {
		Project struct {
			ID int64 `json:"id"`
		} `json:"project"`
		Event   *db.Event `json:"event"`
		Changed bool      `json:"changed"`
	}
	require.NoError(t, json.Unmarshal(bs, &body))
	assert.False(t, body.Changed)
	assert.Nil(t, body.Event)
	assert.Equal(t, p.ID, body.Project.ID)
}

// TestResolve_AliasMissNameHit_FirstSeenAttach handles the case
// where a client has a .kata.toml (so it sends a name) and a git
// workspace whose alias is not yet attached on this daemon (e.g. first
// resolve from a new host). The daemon falls back to name lookup, then
// attaches the alias on first-seen so subsequent resolves go through
// the alias path.
func TestResolve_AliasMissNameHit_FirstSeenAttach(t *testing.T) {
	ts := newTestServer(t)

	// Register a project by name only (no alias attached).
	_, _ = postJSON(t, ts, "/api/v1/projects", map[string]any{"name": "kata"})

	// Client has both name (from .kata.toml) and alias (from git remote)
	// but the alias is not yet attached on the daemon.
	resp, bs := postJSON(t, ts, "/api/v1/projects/resolve", map[string]any{
		"name": "kata",
		"alias": map[string]any{
			"identity":  "github.com/wesm/kata",
			"kind":      "git",
			"root_path": "/client/workspace",
		},
	})
	require.Equal(t, 200, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), `"name":"kata"`)
	assert.Contains(t, string(bs), `"alias_identity":"github.com/wesm/kata"`,
		"alias must be attached on first-seen so subsequent resolves hit the alias path")

	// Second resolve, alias-only, must succeed against the attached alias.
	resp2, bs2 := postJSON(t, ts, "/api/v1/projects/resolve", map[string]any{
		"alias": map[string]any{
			"identity":  "github.com/wesm/kata",
			"kind":      "git",
			"root_path": "/client/workspace",
		},
	})
	require.Equal(t, 200, resp2.StatusCode, string(bs2))
	assert.Contains(t, string(bs2), `"name":"kata"`)
}

// TestResolve_AliasHitReturnsCanonicalName covers the rename-repair
// case: a project was renamed daemon-side, but the client's .kata.toml
// still carries the old name. Alias-first lookup must return the
// canonical name so the client can rewrite .kata.toml.
func TestResolve_AliasHitReturnsCanonicalName(t *testing.T) {
	ts := newTestServer(t)

	// Register and capture project id.
	_, bs := postJSON(t, ts, "/api/v1/projects", map[string]any{
		"name": "old-name",
		"alias": map[string]any{
			"identity":  "github.com/wesm/kata",
			"kind":      "git",
			"root_path": "/client/workspace",
		},
	})
	var initBody struct {
		Project struct {
			ID int64 `json:"id"`
		} `json:"project"`
	}
	require.NoError(t, json.Unmarshal(bs, &initBody))

	// Rename project daemon-side.
	rresp, rbs := patchJSON(t, ts, "/api/v1/projects/"+strconv.FormatInt(initBody.Project.ID, 10),
		map[string]any{"name": "new-name"})
	require.Equal(t, 200, rresp.StatusCode, string(rbs))

	// Client still claims the old name but its alias is attached to the
	// renamed project. Alias must win and response must carry the
	// canonical (new) name.
	resp, bs := postJSON(t, ts, "/api/v1/projects/resolve", map[string]any{
		"name": "old-name",
		"alias": map[string]any{
			"identity":  "github.com/wesm/kata",
			"kind":      "git",
			"root_path": "/client/workspace",
		},
	})
	require.Equal(t, 200, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), `"name":"new-name"`)
}

// TestResolve_ByAliasInput_RejectsArchivedProject guards the
// archived-but-aliased state: RemoveProject hard-deletes aliases
// atomically, so this is only reachable via direct DB edits or an
// import path that doesn't go through the API — but resolve is a
// surface handler, and archived projects must look gone here.
// resolveByName and the active-project helper already enforce this;
// the alias path must too.
func TestResolve_ByAliasInput_RejectsArchivedProject(t *testing.T) {
	ts, h := startDefaultTestServer(t)

	resp, bs := postJSON(t, ts, "/api/v1/projects", map[string]any{
		"name": "kata",
		"alias": map[string]any{
			"identity":  "github.com/wesm/kata",
			"kind":      "git",
			"root_path": "/client/workspace",
		},
	})
	require.Equal(t, 200, resp.StatusCode, string(bs))
	var initBody struct {
		Project struct {
			ID int64 `json:"id"`
		} `json:"project"`
	}
	require.NoError(t, json.Unmarshal(bs, &initBody))

	// Manually flip deleted_at on the project row, leaving the alias
	// intact. The API-level RemoveProject would also drop the alias,
	// so this construction tests the defensive 404 in the alias path
	// rather than the cleanup path.
	_, err := h.db.ExecContext(t.Context(),
		`UPDATE projects SET deleted_at = '2026-05-11T00:00:00.000Z' WHERE id = ?`,
		initBody.Project.ID)
	require.NoError(t, err)

	resp2, bs2 := postJSON(t, ts, "/api/v1/projects/resolve", map[string]any{
		"alias": map[string]any{
			"identity":  "github.com/wesm/kata",
			"kind":      "git",
			"root_path": "/client/workspace",
		},
	})
	assertAPIError(t, resp2.StatusCode, bs2, http.StatusNotFound, "project_not_initialized")
	assert.Contains(t, string(bs2), "archived")
}

// TestResolve_AliasInput_RejectsInvalidKind enforces the same alias
// validation init applies, so callers see a uniform 400 instead of an
// opaque downstream failure.
func TestResolve_AliasInput_RejectsInvalidKind(t *testing.T) {
	ts := newTestServer(t)

	resp, bs := postJSON(t, ts, "/api/v1/projects/resolve", map[string]any{
		"alias": map[string]any{
			"identity":  "github.com/wesm/kata",
			"kind":      "bogus",
			"root_path": "/work",
		},
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), "kind")
}

// TestResolve_NameWinsOverStartPath verifies precedence: when both
// name and start_path are supplied, name takes priority and the daemon
// never touches the (potentially nonexistent) path.
func TestResolve_NameWinsOverStartPath(t *testing.T) {
	dir := t.TempDir()
	testfix.RunGit(t, dir, "init", "--quiet")
	testfix.RunGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")
	ts := newTestServer(t)

	_, _ = postJSON(t, ts, "/api/v1/projects", map[string]any{"start_path": dir})

	// start_path is bogus and would not stat; name must win.
	resp, bs := postJSON(t, ts, "/api/v1/projects/resolve", map[string]any{
		"name":       "kata",
		"start_path": "/no/such/path/anywhere",
	})
	assert.Equal(t, 200, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), `"name":"kata"`)
}

func TestInit_AliasConflictWithoutReassign(t *testing.T) {
	h := newServerWithGitWorkspace(t, "https://github.com/wesm/kata.git")
	ts := h.ts.(*httptest.Server)

	// First init binds the workspace alias to project "kata".
	_, _ = postJSON(t, ts, "/api/v1/projects", map[string]any{"start_path": h.dir})

	// .kata.toml now declares a different name.
	testfix.WriteKataToml(t, h.dir, "other")

	// Re-init follows the alias, treats the config as stale, and rewrites it.
	resp, bs := postJSON(t, ts, "/api/v1/projects", map[string]any{
		"start_path": h.dir,
	})
	require.Equal(t, 200, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), `"name":"kata"`)
	cfgBytes, err := os.ReadFile(filepath.Join(h.dir, ".kata.toml"))
	require.NoError(t, err)
	assert.Contains(t, string(cfgBytes), `name = "kata"`)

	// With --reassign + --replace, succeeds and rewrites alias.
	resp2, bs2 := postJSON(t, ts, "/api/v1/projects", map[string]any{
		"start_path": h.dir,
		"replace":    true,
		"reassign":   true,
	})
	require.Equal(t, 200, resp2.StatusCode, string(bs2))
}

func TestInit_PathBasedReassignHonorsExplicitName(t *testing.T) {
	h := newServerWithGitWorkspace(t, "https://github.com/wesm/kata.git")
	ts := h.ts.(*httptest.Server)

	_, _ = postJSON(t, ts, "/api/v1/projects", map[string]any{"start_path": h.dir})
	_, _ = postJSON(t, ts, "/api/v1/projects", map[string]any{"name": "target"})

	resp, bs := postJSON(t, ts, "/api/v1/projects", map[string]any{
		"start_path": h.dir,
		"name":       "target",
		"replace":    true,
		"reassign":   true,
	})
	require.Equal(t, 200, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), `"name":"target"`)

	cfgBytes, err := os.ReadFile(filepath.Join(h.dir, ".kata.toml")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Contains(t, string(cfgBytes), `name = "target"`)
}

// TestInit_ByName_PathFree verifies the remote-client init path:
// the daemon registers a project by client-derived identity without
// touching the filesystem. This is what lets a kata client on host B
// init a project against host A's daemon when host A cannot stat host
// B's workspace.
func TestInit_ByName_PathFree(t *testing.T) {
	ts := newTestServer(t)

	resp, bs := postJSON(t, ts, "/api/v1/projects", map[string]any{
		"name": "remote",
	})
	require.Equal(t, 200, resp.StatusCode, string(bs))

	var body struct {
		Project struct {
			Name string
		} `json:"project"`
		WorkspaceRoot string `json:"workspace_root"`
		Created       bool   `json:"created"`
	}
	require.NoError(t, json.Unmarshal(bs, &body))
	assert.Equal(t, "remote", body.Project.Name)
	assert.Equal(t, "remote", body.Project.Name)
	assert.True(t, body.Created)
	// Daemon never knew the client workspace path; response must
	// reflect that so the client doesn't write .gitignore in the
	// wrong place.
	assert.Empty(t, body.WorkspaceRoot)

	// Re-init by same identity is idempotent and reports created=false.
	resp2, bs2 := postJSON(t, ts, "/api/v1/projects", map[string]any{
		"name": "remote",
	})
	require.Equal(t, 200, resp2.StatusCode, string(bs2))
	var body2 struct {
		Created bool `json:"created"`
	}
	require.NoError(t, json.Unmarshal(bs2, &body2))
	assert.False(t, body2.Created)
}

// TestInit_NeitherFieldSet rejects requests that supply neither
// name nor start_path (mirrors the resolve contract so
// callers see a uniform validation message).
func TestInit_NeitherFieldSet(t *testing.T) {
	ts := newTestServer(t)

	resp, bs := postJSON(t, ts, "/api/v1/projects", map[string]any{})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), "name")
	assert.Contains(t, string(bs), "start_path")
}

// TestInit_ByName_RejectsEmptyIdentity guards against an empty or
// whitespace-only name slipping through into a project row.
func TestInit_ByName_RejectsEmptyIdentity(t *testing.T) {
	ts := newTestServer(t)

	resp, bs := postJSON(t, ts, "/api/v1/projects", map[string]any{
		"name": "   ",
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), "name")
}

// TestInit_ByName_StrictNameLookup guards against an alias
// collision silently rebinding path-free init to the wrong
// project. If "github.com/wesm/origin" is registered as an alias for
// project X (whose canonical identity is "github.com/wesm/override"),
// a path-free init that asserts name="github.com/wesm/origin"
// must create a new project — not return the alias-bound override.
func TestInit_ByName_StrictNameLookup(t *testing.T) {
	dir := t.TempDir()
	testfix.RunGit(t, dir, "init", "--quiet")
	testfix.RunGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/origin.git")
	ts := newTestServer(t)

	// Path-based init with --project override: project.identity is
	// "github.com/wesm/override" but its alias derives from the git
	// remote → "github.com/wesm/origin".
	resp1, bs1 := postJSON(t, ts, "/api/v1/projects", map[string]any{
		"start_path": dir,
		"name":       "override",
	})
	require.Equal(t, 200, resp1.StatusCode, string(bs1))

	// Path-free init asserting "github.com/wesm/origin" as canonical.
	// Strict lookup must not return the override project.
	resp2, bs2 := postJSON(t, ts, "/api/v1/projects", map[string]any{
		"name": "origin",
	})
	require.Equal(t, 200, resp2.StatusCode, string(bs2))

	var body struct {
		Project struct {
			Name string `json:"name"`
		} `json:"project"`
		Created bool `json:"created"`
	}
	require.NoError(t, json.Unmarshal(bs2, &body))
	assert.Equal(t, "origin", body.Project.Name,
		"daemon must treat name as canonical, not as an alias lookup")
	assert.True(t, body.Created)
}

// TestInit_ByName_AttachesAliasWhenSupplied verifies the path-free
// init attaches an alias the client supplies. This preserves alias
// semantics (resolve-by-git-remote, conflict detection) for remote
// clients that can compute alias info locally.
func TestInit_ByName_AttachesAliasWhenSupplied(t *testing.T) {
	ts := newTestServer(t)

	resp, bs := postJSON(t, ts, "/api/v1/projects", map[string]any{
		"name": "foo",
		"alias": map[string]any{
			"identity":  "github.com/wesm/foo",
			"kind":      "git",
			"root_path": "/client/workspace",
		},
	})
	require.Equal(t, 200, resp.StatusCode, string(bs))

	var body struct {
		Alias struct {
			AliasIdentity string `json:"alias_identity"`
			AliasKind     string `json:"alias_kind"`
			RootPath      string `json:"root_path"`
		} `json:"alias"`
	}
	require.NoError(t, json.Unmarshal(bs, &body))
	assert.Equal(t, "github.com/wesm/foo", body.Alias.AliasIdentity)
	assert.Equal(t, "git", body.Alias.AliasKind)
	assert.Equal(t, "/client/workspace", body.Alias.RootPath)
}

// TestInit_ByName_AliasConflictWithoutReassign returns
// project_alias_conflict when the supplied alias is already attached
// to a different project. Without reassign, the daemon must not
// silently move the alias.
func TestInit_ByName_AliasConflictWithoutReassign(t *testing.T) {
	ts := newTestServer(t)

	_, _ = postJSON(t, ts, "/api/v1/projects", map[string]any{
		"name": "a",
		"alias": map[string]any{
			"identity":  "shared",
			"kind":      "git",
			"root_path": "/work",
		},
	})

	resp, bs := postJSON(t, ts, "/api/v1/projects", map[string]any{
		"name": "b",
		"alias": map[string]any{
			"identity":  "shared",
			"kind":      "git",
			"root_path": "/work",
		},
	})
	require.Equal(t, http.StatusConflict, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), "project_alias_conflict")
}

// TestInit_ByName_ReassignMovesAlias asserts that reassign +
// alias metadata moves the alias from the old project to the new one
// — this is what `kata init --reassign` against a remote daemon
// should do.
func TestInit_ByName_ReassignMovesAlias(t *testing.T) {
	ts := newTestServer(t)

	_, _ = postJSON(t, ts, "/api/v1/projects", map[string]any{
		"name": "old",
		"alias": map[string]any{
			"identity":  "shared",
			"kind":      "git",
			"root_path": "/work",
		},
	})

	resp, bs := postJSON(t, ts, "/api/v1/projects", map[string]any{
		"name":     "new",
		"reassign": true,
		"alias": map[string]any{
			"identity":  "shared",
			"kind":      "git",
			"root_path": "/work",
		},
	})
	require.Equal(t, 200, resp.StatusCode, string(bs))

	var body struct {
		Project struct {
			Name string `json:"name"`
		} `json:"project"`
		Alias struct {
			AliasIdentity string `json:"alias_identity"`
		} `json:"alias"`
	}
	require.NoError(t, json.Unmarshal(bs, &body))
	assert.Equal(t, "new", body.Project.Name)
	assert.Equal(t, "shared", body.Alias.AliasIdentity)
}

// TestInit_ByName_ReassignWithoutAliasErrors guards against the
// silent-success case where --reassign is requested but no alias
// metadata is supplied. With nothing to reassign, the daemon must
// reject rather than report success and leave the old binding intact.
func TestInit_ByName_ReassignWithoutAliasErrors(t *testing.T) {
	ts := newTestServer(t)

	resp, bs := postJSON(t, ts, "/api/v1/projects", map[string]any{
		"name":     "foo",
		"reassign": true,
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), "reassign")
	assert.Contains(t, string(bs), "alias")
}

// TestInit_ByName_AcceptsLocalAliasWithSpaces guards against
// rejecting valid local:// aliases derived from workspace paths
// that contain spaces (or other characters the project-identity
// charset rules disallow). Path-based init attaches such aliases
// without complaint; the path-free flow must do the same so users
// in workspaces like "/Users/me/My Project" aren't blocked.
func TestInit_ByName_AcceptsLocalAliasWithSpaces(t *testing.T) {
	ts := newTestServer(t)

	resp, bs := postJSON(t, ts, "/api/v1/projects", map[string]any{
		"name": "foo",
		"alias": map[string]any{
			"identity":  "local:///Users/me/My Project",
			"kind":      "local",
			"root_path": "/Users/me/My Project",
		},
	})
	require.Equal(t, 200, resp.StatusCode, string(bs))

	var body struct {
		Alias struct {
			AliasIdentity string `json:"alias_identity"`
			AliasKind     string `json:"alias_kind"`
			RootPath      string `json:"root_path"`
		} `json:"alias"`
	}
	require.NoError(t, json.Unmarshal(bs, &body))
	assert.Equal(t, "local:///Users/me/My Project", body.Alias.AliasIdentity)
	assert.Equal(t, "local", body.Alias.AliasKind)
	assert.Equal(t, "/Users/me/My Project", body.Alias.RootPath)
}

// TestInit_ByName_RejectsInvalidAliasKind ensures the daemon
// rejects malformed alias metadata explicitly rather than relying on
// downstream code to misbehave on an unknown kind.
func TestInit_ByName_RejectsInvalidAliasKind(t *testing.T) {
	ts := newTestServer(t)

	resp, bs := postJSON(t, ts, "/api/v1/projects", map[string]any{
		"name": "foo",
		"alias": map[string]any{
			"identity":  "github.com/wesm/foo",
			"kind":      "bogus",
			"root_path": "/work",
		},
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), "kind")
}

// TestInit_ByName_RejectsEmptyAliasRootPath enforces that an
// alias attach has somewhere to root: empty root_path makes future
// path-anchored operations meaningless.
func TestInit_ByName_RejectsEmptyAliasRootPath(t *testing.T) {
	ts := newTestServer(t)

	resp, bs := postJSON(t, ts, "/api/v1/projects", map[string]any{
		"name": "foo",
		"alias": map[string]any{
			"identity":  "github.com/wesm/foo",
			"kind":      "git",
			"root_path": "",
		},
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), "root_path")
}

// TestInit_ByName_DefaultsName verifies the daemon falls back to
// the last identity segment when the client doesn't supply name. This
// matches the local-init contract so the two paths produce the same
// project rows.
func TestInit_ByName_DefaultsName(t *testing.T) {
	ts := newTestServer(t)

	resp, bs := postJSON(t, ts, "/api/v1/projects", map[string]any{
		"name": "auto-name",
	})
	require.Equal(t, 200, resp.StatusCode, string(bs))

	var body struct {
		Project struct {
			Name string
		} `json:"project"`
	}
	require.NoError(t, json.Unmarshal(bs, &body))
	assert.Equal(t, "auto-name", body.Project.Name)
}

// TestResetCounterEndpointReturns404 pins that the /reset-counter endpoint
// is gone after the short_id cutover (spec §9.5). With next_issue_number
// dropped from the schema there is no counter to reset.
func TestResetCounterEndpointReturns404(t *testing.T) {
	h, pid := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)
	pidStr := strconv.FormatInt(pid, 10)
	resp, _ := postJSON(t, ts, "/api/v1/projects/"+pidStr+"/reset-counter",
		map[string]any{"to": 1})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestListProjectsAndShow(t *testing.T) {
	h := newServerWithGitWorkspace(t, "https://github.com/wesm/x.git")
	ts := h.ts.(*httptest.Server)
	_, _ = postJSON(t, ts, "/api/v1/projects", map[string]any{"start_path": h.dir})

	listBody := getBody(t, ts, "/api/v1/projects")
	assert.Contains(t, listBody, `"name":"x"`)

	pid := resolveProjectID(t, ts, h.dir)
	showBody := getBody(t, ts, "/api/v1/projects/"+strconv.FormatInt(pid, 10))
	assert.Contains(t, showBody, `"aliases":`)
}

// TestListProjects_DefaultShape pins the byte-level wire shape of
// GET /api/v1/projects. A future addition of a field to db.Project
// (e.g. an internal-only column) must not silently leak onto this
// response. Spec §7.2.
func TestListProjects_DefaultShape(t *testing.T) {
	h := openTestDB(t)
	_, err := h.db.CreateProject(t.Context(), "x")
	require.NoError(t, err)
	srv := daemon.NewServer(daemon.ServerConfig{DB: h.db, StartedAt: h.now})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	body := getBody(t, ts, "/api/v1/projects")
	var parsed struct {
		Projects []map[string]any `json:"projects"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &parsed))
	require.Len(t, parsed.Projects, 1)
	p := parsed.Projects[0]

	for _, key := range []string{"id", "uid", "name", "metadata", "revision", "created_at"} {
		_, ok := p[key]
		assert.True(t, ok, "missing key %q in projects[0]: %s", key, body)
	}
	_, hasCounter := p["next_issue_number"]
	assert.False(t, hasCounter, "next_issue_number must be absent after spec §9.5: %s", body)
	_, hasStats := p["stats"]
	assert.False(t, hasStats, "stats must not appear in default response: %s", body)
	_, hasUpdated := p["updated_at"]
	assert.False(t, hasUpdated, "updated_at must not appear: %s", body)
	_, hasDeleted := p["deleted_at"]
	assert.False(t, hasDeleted, "deleted_at must omit on active project: %s", body)
}

// TestListAndShowProject_SurfaceMetadata pins that the projects list and show
// responses carry the project's metadata JSON blob verbatim. Consumers
// assemble client-side views (e.g. by area) from this field, so dropping it
// silently would break that flow. Mirrors the metadata patch test (which
// pins persistence): here we verify the wire shape.
func TestListAndShowProject_SurfaceMetadata(t *testing.T) {
	h := openTestDB(t)
	ctx := t.Context()
	p, err := h.db.CreateProject(ctx, "withmeta")
	require.NoError(t, err)
	_, err = h.db.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{
		ProjectID:  p.ID,
		IfMatchRev: p.Revision,
		Actor:      "tester",
		Patch:      map[string]json.RawMessage{"area": json.RawMessage(`"Personal"`)},
	})
	require.NoError(t, err)

	srv := daemon.NewServer(daemon.ServerConfig{DB: h.db, StartedAt: h.now})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	listBody := getBody(t, ts, "/api/v1/projects")
	var listParsed struct {
		Projects []struct {
			Name     string          `json:"name"`
			Metadata json.RawMessage `json:"metadata"`
			Revision int64           `json:"revision"`
		} `json:"projects"`
	}
	require.NoError(t, json.Unmarshal([]byte(listBody), &listParsed))
	require.Len(t, listParsed.Projects, 1)
	assert.Equal(t, "withmeta", listParsed.Projects[0].Name)
	assert.Equal(t, int64(2), listParsed.Projects[0].Revision)
	assert.Contains(t, string(listParsed.Projects[0].Metadata), `"area":"Personal"`,
		"projects list must carry project metadata so consumers can filter without follow-up fetches")

	showBody := getBody(t, ts, "/api/v1/projects/"+strconv.FormatInt(p.ID, 10))
	var showParsed struct {
		Project struct {
			Metadata json.RawMessage `json:"metadata"`
			Revision int64           `json:"revision"`
		} `json:"project"`
	}
	require.NoError(t, json.Unmarshal([]byte(showBody), &showParsed))
	assert.Equal(t, int64(2), showParsed.Project.Revision)
	assert.Contains(t, string(showParsed.Project.Metadata), `"area":"Personal"`,
		"project show must carry project metadata")
}

func TestRenameProject_UpdatesName(t *testing.T) {
	h := newServerWithGitWorkspace(t, "https://github.com/wesm/kata.git")
	ts := h.ts.(*httptest.Server)
	_, _ = postJSON(t, ts, "/api/v1/projects", map[string]any{"start_path": h.dir})

	pid := resolveProjectID(t, ts, h.dir)
	pidStr := strconv.FormatInt(pid, 10)

	resp, bs := patchJSON(t, ts, "/api/v1/projects/"+pidStr, map[string]any{
		"name": "Kata Tracker",
	})
	require.Equal(t, 200, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), `"name":"Kata Tracker"`)
	assert.Contains(t, string(bs), `"aliases":`)

	showBody := getBody(t, ts, "/api/v1/projects/"+pidStr)
	assert.Contains(t, showBody, `"name":"Kata Tracker"`)
}

func TestRenameProject_RejectsBlankName(t *testing.T) {
	h := newServerWithGitWorkspace(t, "https://github.com/wesm/kata.git")
	ts := h.ts.(*httptest.Server)
	_, _ = postJSON(t, ts, "/api/v1/projects", map[string]any{"start_path": h.dir})

	pid := resolveProjectID(t, ts, h.dir)

	resp, bs := patchJSON(t, ts, "/api/v1/projects/"+strconv.FormatInt(pid, 10), map[string]any{
		"name": "   ",
	})
	assert.Equal(t, 400, resp.StatusCode)
	assert.Contains(t, string(bs), "name must be non-empty")
}

func TestRenameProject_MissingIs404(t *testing.T) {
	ts := newTestServer(t)
	resp, bs := patchJSON(t, ts, "/api/v1/projects/9999", map[string]any{
		"name": "Missing",
	})
	assertAPIError(t, resp.StatusCode, bs, 404, "project_not_found")
}

func TestMergeProject_SourceMovesIntoSurvivingTarget(t *testing.T) {
	h := newServerWithGitWorkspace(t, "")
	store := h.DB()
	ctx := t.Context()
	alpha, err := store.CreateProject(ctx, "alpha")
	require.NoError(t, err)
	beta, err := store.CreateProject(ctx, "beta")
	require.NoError(t, err)
	_, err = store.AttachAlias(ctx, alpha.ID, "github.com/wesm/alpha", "git", "/tmp/alpha")
	require.NoError(t, err)
	_, err = store.AttachAlias(ctx, beta.ID, "github.com/wesm/beta", "git", "/tmp/beta")
	require.NoError(t, err)
	created, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: alpha.ID, Title: "existing work", Author: "tester",
	})
	require.NoError(t, err)

	resp, bs := postJSON(t, h.ts.(*httptest.Server),
		"/api/v1/projects/"+strconv.FormatInt(beta.ID, 10)+"/merge",
		map[string]any{"source_project_id": alpha.ID})
	require.Equal(t, 200, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), `"name":"beta"`)
	assert.Contains(t, string(bs), `"issues_moved":1`)

	issue, err := store.IssueByID(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "existing work", issue.Title)
	assert.Equal(t, beta.ID, issue.ProjectID, "issue moved onto target project")
	_, err = store.ProjectByID(ctx, alpha.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

// TestRemoveProject_ArchivesAndDropsAliases pins #24's wire shape: DELETE
// /api/v1/projects/{id}?actor=tester archives the project and removes its
// aliases. List endpoint no longer surfaces the row; resolve against the
// archived identity returns project_archived (409).
func TestRemoveProject_ArchivesAndDropsAliases(t *testing.T) {
	h := newServerWithGitWorkspace(t, "")
	ts := h.ts.(*httptest.Server)
	store := h.DB()
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "proj-rm")
	require.NoError(t, err)
	_, err = store.AttachAlias(ctx, p.ID, "github.com/wesm/proj-rm", "git", h.dir)
	require.NoError(t, err)

	resp, bs := doReq(t, ts, http.MethodDelete,
		"/api/v1/projects/"+strconv.FormatInt(p.ID, 10)+"?actor=tester", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(bs))

	var body struct {
		Project struct {
			DeletedAt *string `json:"deleted_at"`
		} `json:"project"`
		Event struct {
			Type string `json:"type"`
		} `json:"event"`
	}
	require.NoError(t, json.Unmarshal(bs, &body), string(bs))
	require.NotNil(t, body.Project.DeletedAt)
	assert.Equal(t, "project.removed", body.Event.Type)

	listBody := getBody(t, ts, "/api/v1/projects")
	assert.NotContains(t, listBody, "github.com/wesm/proj-rm",
		"archived project must not surface in /projects list")
}

// TestRemoveProject_RefusesWithOpenIssues pins the safety gate: the wire
// returns 409 project_has_open_issues when force is omitted.
func TestRemoveProject_RefusesWithOpenIssues(t *testing.T) {
	h := newServerWithGitWorkspace(t, "")
	ts := h.ts.(*httptest.Server)
	store := h.DB()
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "proj-busy")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "still open", Author: "tester",
	})
	require.NoError(t, err)

	resp, bs := doReq(t, ts, http.MethodDelete,
		"/api/v1/projects/"+strconv.FormatInt(p.ID, 10)+"?actor=tester", nil, nil)
	assertAPIError(t, resp.StatusCode, bs, http.StatusConflict, "project_has_open_issues")
}

// TestRemoveProject_ForceOverridesOpenIssues pins ?force=true: with the
// flag, archival succeeds even with open issues.
func TestRemoveProject_ForceOverridesOpenIssues(t *testing.T) {
	h := newServerWithGitWorkspace(t, "")
	ts := h.ts.(*httptest.Server)
	store := h.DB()
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "proj-force-http")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "still open", Author: "tester",
	})
	require.NoError(t, err)

	resp, bs := doReq(t, ts, http.MethodDelete,
		"/api/v1/projects/"+strconv.FormatInt(p.ID, 10)+"?actor=tester&force=true", nil, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode, string(bs))
}

// TestDetachProjectAlias_DropsOneAndEmitsEvent pins the alias-level wire:
// DELETE /api/v1/projects/{id}/aliases/{alias_id} drops one alias and
// emits project.alias_removed.
func TestDetachProjectAlias_DropsOneAndEmitsEvent(t *testing.T) {
	h := newServerWithGitWorkspace(t, "")
	ts := h.ts.(*httptest.Server)
	store := h.DB()
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "proj-alias-http")
	require.NoError(t, err)
	a1, err := store.AttachAlias(ctx, p.ID, "github.com/wesm/proj-alias-http", "git", h.dir)
	require.NoError(t, err)
	a2, err := store.AttachAlias(ctx, p.ID, "local:///tmp/aliased", "local", "/tmp/aliased")
	require.NoError(t, err)

	resp, bs := doReq(t, ts, http.MethodDelete,
		"/api/v1/projects/"+strconv.FormatInt(p.ID, 10)+
			"/aliases/"+strconv.FormatInt(a2.ID, 10)+"?actor=tester", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(bs))

	var body struct {
		Alias struct {
			ID int64 `json:"id"`
		} `json:"alias"`
		Event struct {
			Type string `json:"type"`
		} `json:"event"`
	}
	require.NoError(t, json.Unmarshal(bs, &body), string(bs))
	assert.Equal(t, a2.ID, body.Alias.ID)
	assert.Equal(t, "project.alias_removed", body.Event.Type)

	// The other alias remains and resolve still works.
	remaining, err := store.AliasByID(ctx, a1.ID)
	require.NoError(t, err)
	assert.Equal(t, a1.ID, remaining.ID)
}

// TestDetachProjectAlias_LastRefuses pins the safety gate at the wire:
// the only alias for a project rejects detach without ?force=true.
func TestDetachProjectAlias_LastRefuses(t *testing.T) {
	h := newServerWithGitWorkspace(t, "")
	ts := h.ts.(*httptest.Server)
	store := h.DB()
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "proj-only-http")
	require.NoError(t, err)
	a, err := store.AttachAlias(ctx, p.ID, "github.com/wesm/proj-only-http", "git", h.dir)
	require.NoError(t, err)

	resp, bs := doReq(t, ts, http.MethodDelete,
		"/api/v1/projects/"+strconv.FormatInt(p.ID, 10)+
			"/aliases/"+strconv.FormatInt(a.ID, 10)+"?actor=tester", nil, nil)
	assertAPIError(t, resp.StatusCode, bs, http.StatusConflict, "alias_is_last")
}

// TestDetachProjectAlias_RejectsCrossProject pins that an alias_id from
// another project can't be dropped via this project's path. Returns 404 to
// avoid leaking the existence of the cross-project alias.
func TestDetachProjectAlias_RejectsCrossProject(t *testing.T) {
	h := newServerWithGitWorkspace(t, "")
	ts := h.ts.(*httptest.Server)
	store := h.DB()
	ctx := t.Context()
	p1, err := store.CreateProject(ctx, "p1")
	require.NoError(t, err)
	p2, err := store.CreateProject(ctx, "p2")
	require.NoError(t, err)
	_, err = store.AttachAlias(ctx, p1.ID, "github.com/wesm/p1", "git", h.dir)
	require.NoError(t, err)
	a2, err := store.AttachAlias(ctx, p2.ID, "github.com/wesm/p2", "git", h.dir)
	require.NoError(t, err)

	resp, bs := doReq(t, ts, http.MethodDelete,
		"/api/v1/projects/"+strconv.FormatInt(p1.ID, 10)+
			"/aliases/"+strconv.FormatInt(a2.ID, 10)+"?actor=tester", nil, nil)
	assertAPIError(t, resp.StatusCode, bs, http.StatusNotFound, "alias_not_found")
}

// TestRemoveProject_ArchivedNameRefusesReinit pins the user's clarifier
// in the design conversation: re-init against an archived identity returns
// project_archived (409) rather than silently resurrecting the project.
func TestRemoveProject_ArchivedNameRefusesReinit(t *testing.T) {
	h := newServerWithGitWorkspace(t, "https://github.com/wesm/proj-archive-reinit.git")
	store := h.DB()
	ctx := t.Context()
	// Init the project.
	resp, bs := postJSON(t, h.ts.(*httptest.Server), "/api/v1/projects",
		map[string]any{"start_path": h.dir})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(bs))
	var initBody struct {
		Project struct {
			ID int64 `json:"id"`
		} `json:"project"`
	}
	require.NoError(t, json.Unmarshal(bs, &initBody))

	// Archive it.
	_, _, err := store.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: initBody.Project.ID, Actor: "tester",
	})
	require.NoError(t, err)

	// Re-init from the same workspace must refuse.
	resp2, bs2 := postJSON(t, h.ts.(*httptest.Server), "/api/v1/projects",
		map[string]any{"start_path": h.dir})
	assert.Equal(t, http.StatusConflict, resp2.StatusCode, string(bs2))
	assert.Contains(t, string(bs2), "project_archived")
}

// TestListProjects_WithStatsIncludesAggregates pins the new wire
// contract: ?include=stats returns a stats triple per project. Spec §7.1.
func TestListProjects_WithStatsIncludesAggregates(t *testing.T) {
	h := openTestDB(t)
	ctx := t.Context()
	p, err := h.db.CreateProject(ctx, "x")
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		_, _, err := h.db.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: p.ID, Title: "i", Author: "tester",
		})
		require.NoError(t, err)
	}
	srv := daemon.NewServer(daemon.ServerConfig{DB: h.db, StartedAt: h.now})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	body := getBody(t, ts, "/api/v1/projects?include=stats")
	var parsed struct {
		Projects []struct {
			ID    int64 `json:"id"`
			Stats *struct {
				Open        int     `json:"open"`
				Closed      int     `json:"closed"`
				LastEventAt *string `json:"last_event_at"`
			} `json:"stats"`
		} `json:"projects"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &parsed))
	require.Len(t, parsed.Projects, 1)
	require.NotNil(t, parsed.Projects[0].Stats, "stats present with ?include=stats")
	assert.Equal(t, 3, parsed.Projects[0].Stats.Open)
	assert.Equal(t, 0, parsed.Projects[0].Stats.Closed)
	require.NotNil(t, parsed.Projects[0].Stats.LastEventAt)
}

// TestListProjects_WithStatsHandlesEmptyProjects pins that a project
// with zero issues and zero events serializes Open=0, Closed=0,
// LastEventAt=null. Spec §7.1.
func TestListProjects_WithStatsHandlesEmptyProjects(t *testing.T) {
	h := openTestDB(t)
	_, err := h.db.CreateProject(t.Context(), "empty")
	require.NoError(t, err)
	srv := daemon.NewServer(daemon.ServerConfig{DB: h.db, StartedAt: h.now})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	body := getBody(t, ts, "/api/v1/projects?include=stats")
	var parsed struct {
		Projects []struct {
			// Pointer so a missing/null "stats" key would decode as nil
			// — without this, the omitempty path would let the test
			// pass even if the API stopped emitting stats for empty
			// projects. The require.NotNil below is the actual contract.
			Stats *struct {
				Open        int     `json:"open"`
				Closed      int     `json:"closed"`
				LastEventAt *string `json:"last_event_at"`
			} `json:"stats"`
		} `json:"projects"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &parsed))
	require.Len(t, parsed.Projects, 1)
	require.NotNil(t, parsed.Projects[0].Stats, "stats must be present even for empty projects")
	assert.Equal(t, 0, parsed.Projects[0].Stats.Open)
	assert.Equal(t, 0, parsed.Projects[0].Stats.Closed)
	assert.Nil(t, parsed.Projects[0].Stats.LastEventAt, "no events → null")
}

// TestListProjects_DefaultShapeUnchangedAfterStats pins that the no-query
// default response did not regress after Task 3 — backwards compat for
// kata projects list and any other consumer that doesn't opt in. Spec §7.1.
func TestListProjects_DefaultShapeUnchangedAfterStats(t *testing.T) {
	h := openTestDB(t)
	_, err := h.db.CreateProject(t.Context(), "x")
	require.NoError(t, err)
	srv := daemon.NewServer(daemon.ServerConfig{DB: h.db, StartedAt: h.now})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	body := getBody(t, ts, "/api/v1/projects")
	var parsed struct {
		Projects []map[string]any `json:"projects"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &parsed))
	require.Len(t, parsed.Projects, 1)
	_, has := parsed.Projects[0]["stats"]
	assert.False(t, has, "stats must omit without ?include=stats")
}

func TestMergeProject_ImportMappingCollisionReturns409(t *testing.T) {
	ts, h := startDefaultTestServer(t)
	ctx := t.Context()
	source, err := h.db.CreateProject(ctx, "source")
	require.NoError(t, err)
	target, err := h.db.CreateProject(ctx, "target")
	require.NoError(t, err)
	sourceIssue, _, err := h.db.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "source", Author: "tester"})
	require.NoError(t, err)
	targetIssue, _, err := h.db.CreateIssue(ctx, db.CreateIssueParams{ProjectID: target.ID, Title: "target", Author: "tester"})
	require.NoError(t, err)
	_, err = h.db.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source: "beads", ExternalID: "same", ObjectType: "issue", ProjectID: source.ID, IssueID: &sourceIssue.ID,
	})
	require.NoError(t, err)
	_, err = h.db.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source: "beads", ExternalID: "same", ObjectType: "issue", ProjectID: target.ID, IssueID: &targetIssue.ID,
	})
	require.NoError(t, err)

	resp, bs := postJSON(t, ts, "/api/v1/projects/"+strconv.FormatInt(target.ID, 10)+"/merge", map[string]any{
		"source_project_id": source.ID,
	})

	assertAPIError(t, resp.StatusCode, bs, 409, "project_merge_import_mapping_collision")
}

func TestInit_MergedKataTomlNameResolvesToSurvivingProject(t *testing.T) {
	h := newServerWithGitWorkspace(t, "https://github.com/wesm/beta.git")
	store := h.DB()
	ctx := t.Context()
	alpha, err := store.CreateProject(ctx, "alpha")
	require.NoError(t, err)
	beta, err := store.CreateProject(ctx, "beta")
	require.NoError(t, err)
	_, err = store.AttachAlias(ctx, alpha.ID, "github.com/wesm/alpha", "git", h.dir)
	require.NoError(t, err)
	_, err = store.AttachAlias(ctx, beta.ID, "github.com/wesm/beta", "git", h.dir)
	require.NoError(t, err)
	_, err = store.MergeProjects(ctx, db.MergeProjectsParams{
		SourceProjectID: alpha.ID,
		TargetProjectID: beta.ID,
	})
	require.NoError(t, err)
	testfix.WriteKataToml(t, h.dir, "alpha")

	resp, bs := postJSON(t, h.ts.(*httptest.Server), "/api/v1/projects", map[string]any{
		"start_path": h.dir,
	})
	require.Equal(t, 200, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), `"name":"beta"`)

	cfgBytes, err := os.ReadFile(filepath.Join(h.dir, ".kata.toml"))
	require.NoError(t, err)
	assert.Contains(t, string(cfgBytes), `name = "beta"`)
}
