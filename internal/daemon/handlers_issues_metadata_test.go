package daemon_test

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// TestCreateIssue_AcceptsMetadata pins that the create body's metadata (opaque
// + reserved-valid keys) is validated and persisted.
func TestCreateIssue_AcceptsMetadata(t *testing.T) {
	env := testenv.New(t)
	projectID := mkProject(t, env, "github.com/test/a", "a")

	var out struct {
		Issue db.Issue `json:"issue"`
	}
	resp := envDoJSON(t, env, http.MethodPost, projectPath(projectID)+"/issues", map[string]any{
		"actor": "tester",
		"title": "with metadata",
		"metadata": map[string]jsontext.Value{
			"work.branch":  jsontext.Value(`"feature/x"`),
			"scheduled_on": jsontext.Value(`"2026-01-02"`),
		},
	}, &out)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got, err := env.DB.IssueByID(context.Background(), out.Issue.ID)
	require.NoError(t, err)
	var m map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(got.Metadata), &m))
	assert.JSONEq(t, `"feature/x"`, string(m["work.branch"]))
	assert.JSONEq(t, `"2026-01-02"`, string(m["scheduled_on"]))
}

// TestCreateIssue_RejectsInvalidReservedMetadata pins the 400
// invalid_metadata_value error shape (same as the patch endpoint).
func TestCreateIssue_RejectsInvalidReservedMetadata(t *testing.T) {
	env := testenv.New(t)
	projectID := mkProject(t, env, "github.com/test/a", "a")

	resp, bs := envDoRaw(t, env, http.MethodPost, projectPath(projectID)+"/issues", map[string]any{
		"actor": "tester",
		"title": "bad reserved",
		"metadata": map[string]jsontext.Value{
			"scheduled_on": jsontext.Value(`"not-a-date"`),
		},
	}, nil)
	assertAPIError(t, resp.StatusCode, bs, http.StatusBadRequest, "invalid_metadata_value")
}

// TestCreateIssue_RejectsNullMetadata pins that a JSON null value at creation
// is a 400 invalid_metadata_value (nothing to clear).
func TestCreateIssue_RejectsNullMetadata(t *testing.T) {
	env := testenv.New(t)
	projectID := mkProject(t, env, "github.com/test/a", "a")

	resp, bs := envDoRaw(t, env, http.MethodPost, projectPath(projectID)+"/issues", map[string]any{
		"actor": "tester",
		"title": "null value",
		"metadata": map[string]jsontext.Value{
			"work.branch": jsontext.Value(`null`),
		},
	}, nil)
	assertAPIError(t, resp.StatusCode, bs, http.StatusBadRequest, "invalid_metadata_value")
}

// TestCreateIssue_IdempotencyFoldsMetadata pins that create-time metadata is
// part of the idempotency fingerprint now that create persists it:
//   - same key + same metadata → reuse (no new issue).
//   - same key + different metadata → 409 idempotency_mismatch.
//   - same key + no metadata on either request → reuse (unchanged from before
//     metadata joined the fingerprint).
func TestCreateIssue_IdempotencyFoldsMetadata(t *testing.T) {
	post := func(t *testing.T, env *testenv.Env, projectID int64, key string, body map[string]any) (*http.Response, []byte) {
		return envDoRaw(t, env, http.MethodPost, projectPath(projectID)+"/issues", body,
			map[string]string{"Idempotency-Key": key})
	}

	t.Run("same_metadata_reuses", func(t *testing.T) {
		env := testenv.New(t)
		projectID := mkProject(t, env, "github.com/test/a", "a")
		body := map[string]any{
			"actor": "tester",
			"title": "idem",
			"metadata": map[string]jsontext.Value{
				"work.branch": jsontext.Value(`"feature/x"`),
			},
		}
		var out1 struct {
			Issue db.Issue `json:"issue"`
		}
		resp1, bs1 := post(t, env, projectID, "k1", body)
		require.Equalf(t, http.StatusOK, resp1.StatusCode, "body: %s", string(bs1))
		require.NoError(t, json.Unmarshal(bs1, &out1))

		var out2 struct {
			Issue  db.Issue `json:"issue"`
			Reused bool     `json:"reused"`
		}
		resp2, bs2 := post(t, env, projectID, "k1", body)
		require.Equalf(t, http.StatusOK, resp2.StatusCode, "body: %s", string(bs2))
		require.NoError(t, json.Unmarshal(bs2, &out2))
		assert.True(t, out2.Reused, "identical metadata must reuse the original issue")
		assert.Equal(t, out1.Issue.ID, out2.Issue.ID)
	})

	t.Run("different_metadata_conflicts", func(t *testing.T) {
		env := testenv.New(t)
		projectID := mkProject(t, env, "github.com/test/a", "a")
		first := map[string]any{
			"actor": "tester",
			"title": "idem",
			"metadata": map[string]jsontext.Value{
				"work.branch": jsontext.Value(`"original"`),
			},
		}
		resp1, bs1 := post(t, env, projectID, "k1", first)
		require.Equalf(t, http.StatusOK, resp1.StatusCode, "body: %s", string(bs1))

		replay := map[string]any{
			"actor": "tester",
			"title": "idem",
			"metadata": map[string]jsontext.Value{
				"work.branch": jsontext.Value(`"changed"`),
			},
		}
		resp2, bs2 := post(t, env, projectID, "k1", replay)
		assertAPIError(t, resp2.StatusCode, bs2, http.StatusConflict, "idempotency_mismatch")
	})

	t.Run("no_metadata_reuses", func(t *testing.T) {
		env := testenv.New(t)
		projectID := mkProject(t, env, "github.com/test/a", "a")
		body := map[string]any{
			"actor": "tester",
			"title": "idem",
		}
		var out1 struct {
			Issue db.Issue `json:"issue"`
		}
		resp1, bs1 := post(t, env, projectID, "k1", body)
		require.Equalf(t, http.StatusOK, resp1.StatusCode, "body: %s", string(bs1))
		require.NoError(t, json.Unmarshal(bs1, &out1))

		var out2 struct {
			Issue  db.Issue `json:"issue"`
			Reused bool     `json:"reused"`
		}
		resp2, bs2 := post(t, env, projectID, "k1", body)
		require.Equalf(t, http.StatusOK, resp2.StatusCode, "body: %s", string(bs2))
		require.NoError(t, json.Unmarshal(bs2, &out2))
		assert.True(t, out2.Reused, "metadata-free replay must still reuse")
		assert.Equal(t, out1.Issue.ID, out2.Issue.ID)
	})
}

// TestListIssues_MetaQueryFilters pins the meta query param on the per-project
// list endpoint (presence and equality forms).
func TestListIssues_MetaQueryFilters(t *testing.T) {
	env := testenv.New(t)
	projectID := mkProject(t, env, "github.com/test/a", "a")

	mk := func(title, attention string) string {
		var out struct {
			Issue db.Issue `json:"issue"`
		}
		resp := envDoJSON(t, env, http.MethodPost, projectPath(projectID)+"/issues", map[string]any{
			"actor": "tester",
			"title": title,
			"metadata": map[string]jsontext.Value{
				"work.attention": jsontext.Value(`"` + attention + `"`),
			},
		}, &out)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		return out.Issue.ShortID
	}
	stuck := mk("stuck one", "stuck")
	_ = mk("ok one", "ok")

	// Equality form.
	var eqOut struct {
		Issues []struct {
			ShortID string `json:"short_id"`
		} `json:"issues"`
	}
	resp := envDoJSON(t, env, http.MethodGet,
		projectPath(projectID)+"/issues?meta=work.attention=stuck", nil, &eqOut)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Len(t, eqOut.Issues, 1)
	assert.Equal(t, stuck, eqOut.Issues[0].ShortID)

	// Presence form matches both.
	var presOut struct {
		Issues []struct {
			ShortID string `json:"short_id"`
		} `json:"issues"`
	}
	resp = envDoJSON(t, env, http.MethodGet,
		projectPath(projectID)+"/issues?meta=work.attention", nil, &presOut)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Len(t, presOut.Issues, 2)
}

// TestListIssues_MetaEmptyKeyRejected pins that a meta param with an empty key
// is a validation error.
func TestListIssues_MetaEmptyKeyRejected(t *testing.T) {
	env := testenv.New(t)
	projectID := mkProject(t, env, "github.com/test/a", "a")

	resp, bs := envGetRaw(t, env, projectPath(projectID)+"/issues?meta==value")
	assertAPIError(t, resp.StatusCode, bs, http.StatusBadRequest, "validation")
}
