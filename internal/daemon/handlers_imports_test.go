package daemon_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestImportEndpoint_CreatesAndReimports(t *testing.T) {
	env := testenv.New(t)
	pid := createImportTestProject(t, env, "github.com/wesm/kata", "kata").ID
	body := map[string]any{
		"actor":  "importer",
		"source": "beads",
		"items": []map[string]any{{
			"external_id": "beads-1",
			"title":       "Imported",
			"body":        "body",
			"author":      "alice",
			"status":      "open",
			"created_at":  "2026-05-01T10:00:00Z",
			"updated_at":  "2026-05-01T10:00:00Z",
			"labels":      []string{"source:beads", "beads-id:beads-1"},
			"comments": []map[string]any{{
				"external_id": "c1",
				"author":      "alice",
				"body":        "comment",
				"created_at":  "2026-05-01T10:01:00Z",
			}},
		}},
	}
	var out struct {
		Source   string   `json:"source"`
		Created  int      `json:"created"`
		Comments int      `json:"comments"`
		Errors   []string `json:"errors"`
		Items    []struct {
			IssueShortID string `json:"issue_short_id"`
		} `json:"items"`
	}
	envPostJSON(t, env, importEndpointPath(pid), body, &out)
	assert.Equal(t, "beads", out.Source)
	assert.Equal(t, 1, out.Created)
	assert.Equal(t, 1, out.Comments)
	assert.NotNil(t, out.Errors, "success response should emit errors: []")
	assert.Empty(t, out.Errors)
	require.Len(t, out.Items, 1)

	var second struct {
		Created   int `json:"created"`
		Unchanged int `json:"unchanged"`
		Comments  int `json:"comments"`
	}
	envPostJSON(t, env, importEndpointPath(pid), body, &second)
	assert.Equal(t, 0, second.Created)
	assert.Equal(t, 1, second.Unchanged)
	assert.Equal(t, 0, second.Comments)

	issue, err := env.DB.IssueByShortID(context.Background(), pid, out.Items[0].IssueShortID, db.IncludeDeletedNo)
	require.NoError(t, err)
	var commentCount int
	err = env.DB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM comments WHERE issue_id = ?`, issue.ID).Scan(&commentCount)
	require.NoError(t, err)
	assert.Equal(t, 1, commentCount, "reimport should not duplicate mapped comments")
}

func TestImportEndpoint_BroadcastsAndEnqueuesHookEvents(t *testing.T) {
	sink := &recordingSink{}
	bcast := daemon.NewEventBroadcaster()
	h, pid := bootstrapProject(t, withHooksSink(sink), withBroadcaster(bcast))
	ts := h.ts.(*httptest.Server)
	sub := bcast.Subscribe(daemon.SubFilter{ProjectID: pid})
	defer sub.Unsub()

	body := map[string]any{
		"actor":  "importer",
		"source": "beads",
		"items": []map[string]any{{
			"external_id": "beads-1",
			"title":       "Imported",
			"body":        "body",
			"author":      "alice",
			"status":      "open",
			"created_at":  "2026-05-01T10:00:00Z",
			"updated_at":  "2026-05-01T10:00:00Z",
			"comments": []map[string]any{{
				"external_id": "c1",
				"author":      "alice",
				"body":        "comment",
				"created_at":  "2026-05-01T10:01:00Z",
			}},
		}},
	}

	resp, bs := postJSON(t, ts, importEndpointPath(pid), body)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "import status: %s", string(bs))

	broadcastTypes := []string{
		receiveMsg(t, sub.Ch, time.Second, "issue created broadcast").Event.Type,
		receiveMsg(t, sub.Ch, time.Second, "comment broadcast").Event.Type,
	}
	assert.Equal(t, []string{"issue.created", "issue.commented"}, broadcastTypes)

	captured := sink.snapshot()
	require.Len(t, captured, 2)
	hookTypes := []string{captured[0].Type, captured[1].Type}
	assert.Equal(t, broadcastTypes, hookTypes)
}

func TestImportEndpoint_SourceNewerUpdatesIssue(t *testing.T) {
	env := testenv.New(t)
	pid := createImportTestProject(t, env, "github.com/wesm/kata", "kata").ID
	body := map[string]any{
		"actor":  "importer",
		"source": "beads",
		"items": []map[string]any{{
			"external_id": "beads-1",
			"title":       "Old title",
			"body":        "old body",
			"author":      "alice",
			"status":      "open",
			"created_at":  "2026-05-01T10:00:00Z",
			"updated_at":  "2026-05-01T10:00:00Z",
		}},
	}
	var first struct {
		Created int `json:"created"`
		Items   []struct {
			IssueShortID string `json:"issue_short_id"`
		} `json:"items"`
	}
	envPostJSON(t, env, importEndpointPath(pid), body, &first)
	require.Equal(t, 1, first.Created)
	require.Len(t, first.Items, 1)

	body["items"] = []map[string]any{{
		"external_id":   "beads-1",
		"title":         "New title",
		"body":          "new body",
		"author":        "alice",
		"status":        "closed",
		"closed_reason": "done",
		"created_at":    "2026-05-01T10:00:00Z",
		"updated_at":    "2026-05-01T11:00:00Z",
		"closed_at":     "2026-05-01T11:00:00Z",
	}}
	var second struct {
		Updated int `json:"updated"`
	}
	envPostJSON(t, env, importEndpointPath(pid), body, &second)
	assert.Equal(t, 1, second.Updated)

	issue, err := env.DB.IssueByShortID(context.Background(), pid, first.Items[0].IssueShortID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, "New title", issue.Title)
	assert.Equal(t, "new body", issue.Body)
	assert.Equal(t, "closed", issue.Status)
	require.NotNil(t, issue.ClosedReason)
	assert.Equal(t, "done", *issue.ClosedReason)
}

func TestImportEndpoint_FederatedExistingIssueAllowsUnclaimedUpdate(t *testing.T) {
	ctx := context.Background()
	env := testenv.New(t)
	project := createImportTestProject(t, env, "github.com/wesm/kata", "kata")
	initial := map[string]any{
		"actor":  "importer",
		"source": "beads",
		"items": []map[string]any{{
			"external_id": "beads-1",
			"title":       "Old title",
			"body":        "old body",
			"author":      "alice",
			"status":      "open",
			"created_at":  "2026-05-01T10:00:00Z",
			"updated_at":  "2026-05-01T10:00:00Z",
		}},
	}
	var first struct {
		Items []struct {
			IssueShortID string `json:"issue_short_id"`
		} `json:"items"`
	}
	envPostJSON(t, env, importEndpointPath(project.ID), initial, &first)
	require.Len(t, first.Items, 1)
	issue, err := env.DB.IssueByShortID(ctx, project.ID, first.Items[0].IssueShortID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = env.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleHub,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 1,
		Enabled:              true,
	})
	require.NoError(t, err)
	update := map[string]any{
		"actor":  "importer",
		"source": "beads",
		"items": []map[string]any{{
			"external_id": "beads-1",
			"title":       "New title",
			"body":        "new body",
			"author":      "alice",
			"status":      "open",
			"created_at":  "2026-05-01T10:00:00Z",
			"updated_at":  "2026-05-01T11:00:00Z",
		}},
	}

	resp, raw := envDoRaw(t, env, http.MethodPost, importEndpointPath(project.ID), update, nil)

	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var out struct {
		Updated int `json:"updated"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Equal(t, 1, out.Updated)
	changed, err := env.DB.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	assert.Equal(t, "New title", changed.Title)
}

func TestImportEndpoint_FederatedExistingIssueDeniesOtherLeaseHolder(t *testing.T) {
	ctx := context.Background()
	env := testenv.New(t)
	project := createImportTestProject(t, env, "github.com/wesm/kata", "kata")
	initial := map[string]any{
		"actor":  "importer",
		"source": "beads",
		"items": []map[string]any{{
			"external_id": "beads-1",
			"title":       "Old title",
			"body":        "old body",
			"author":      "alice",
			"status":      "open",
			"created_at":  "2026-05-01T10:00:00Z",
			"updated_at":  "2026-05-01T10:00:00Z",
		}},
	}
	var first struct {
		Items []struct {
			IssueShortID string `json:"issue_short_id"`
		} `json:"items"`
	}
	envPostJSON(t, env, importEndpointPath(project.ID), initial, &first)
	require.Len(t, first.Items, 1)
	issue, err := env.DB.IssueByShortID(ctx, project.ID, first.Items[0].IssueShortID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = env.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleHub,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 1,
		Enabled:              true,
	})
	require.NoError(t, err)

	_, err = env.DB.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: env.DB.InstanceUID(),
			Holder:            "other",
			ClientKind:        "cli",
		},
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)
	update := map[string]any{
		"actor":  "importer",
		"source": "beads",
		"items": []map[string]any{{
			"external_id": "beads-1",
			"title":       "New title",
			"body":        "new body",
			"author":      "alice",
			"status":      "open",
			"created_at":  "2026-05-01T10:00:00Z",
			"updated_at":  "2026-05-01T11:00:00Z",
		}},
	}

	resp, raw := envDoRaw(t, env, http.MethodPost, importEndpointPath(project.ID), update, nil)

	assertAPIError(t, resp.StatusCode, raw, http.StatusConflict, "claim_denied")
	unchanged, err := env.DB.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	assert.Equal(t, "Old title", unchanged.Title)
}

func TestImportEndpoint_FederatedCommentOnlyImportBypassesOtherLeaseHolder(t *testing.T) {
	ctx := context.Background()
	env := testenv.New(t)
	project := createImportTestProject(t, env, "github.com/wesm/kata", "kata")
	initial := map[string]any{
		"actor":  "importer",
		"source": "beads",
		"items": []map[string]any{{
			"external_id": "beads-1",
			"title":       "Imported",
			"body":        "body",
			"author":      "alice",
			"status":      "open",
			"created_at":  "2026-05-01T10:00:00Z",
			"updated_at":  "2026-05-01T10:00:00Z",
		}},
	}
	var first struct {
		Items []struct {
			IssueShortID string `json:"issue_short_id"`
		} `json:"items"`
	}
	envPostJSON(t, env, importEndpointPath(project.ID), initial, &first)
	require.Len(t, first.Items, 1)
	issue, err := env.DB.IssueByShortID(ctx, project.ID, first.Items[0].IssueShortID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = env.DB.EnableProjectFederation(ctx, project.ID, "tester")
	require.NoError(t, err)
	_, err = env.DB.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: env.DB.InstanceUID(),
			Holder:            "other",
			ClientKind:        "cli",
		},
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)
	commentOnly := map[string]any{
		"actor":  "importer",
		"source": "beads",
		"items": []map[string]any{{
			"external_id": "beads-1",
			"title":       "Imported",
			"body":        "body",
			"author":      "alice",
			"status":      "open",
			"created_at":  "2026-05-01T10:00:00Z",
			"updated_at":  "2026-05-01T10:00:00Z",
			"comments": []map[string]any{{
				"external_id": "c1",
				"author":      "alice",
				"body":        "append-only note",
				"created_at":  "2026-05-01T10:01:00Z",
			}},
		}},
	}

	resp, raw := envDoRaw(t, env, http.MethodPost, importEndpointPath(project.ID), commentOnly, nil)

	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var out struct {
		Unchanged int `json:"unchanged"`
		Comments  int `json:"comments"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Equal(t, 1, out.Unchanged)
	assert.Equal(t, 1, out.Comments)
}

func TestImportEndpoint_FederatedIdempotentReimportDoesNotRequireLease(t *testing.T) {
	env := testenv.New(t)
	project := createImportTestProject(t, env, "github.com/wesm/kata", "kata")
	body := map[string]any{
		"actor":  "importer",
		"source": "beads",
		"items": []map[string]any{{
			"external_id": "beads-1",
			"title":       "Imported",
			"body":        "body",
			"author":      "alice",
			"status":      "open",
			"created_at":  "2026-05-01T10:00:00Z",
			"updated_at":  "2026-05-01T10:00:00Z",
			"comments": []map[string]any{{
				"external_id": "c1",
				"author":      "alice",
				"body":        "comment",
				"created_at":  "2026-05-01T10:01:00Z",
			}},
		}},
	}
	envPostJSON(t, env, importEndpointPath(project.ID), body, &struct{}{})
	_, err := env.DB.EnableProjectFederation(context.Background(), project.ID, "tester")
	require.NoError(t, err)
	var out struct {
		Unchanged int `json:"unchanged"`
		Comments  int `json:"comments"`
	}

	envPostJSON(t, env, importEndpointPath(project.ID), body, &out)

	assert.Equal(t, 1, out.Unchanged)
	assert.Equal(t, 0, out.Comments)
}

func TestImportEndpoint_FederatedNewIssueLinkTargetDoesNotRequireTargetLease(t *testing.T) {
	env := testenv.New(t)
	project := createImportTestProject(t, env, "github.com/wesm/kata", "kata")
	parent := map[string]any{
		"actor":  "importer",
		"source": "beads",
		"items": []map[string]any{{
			"external_id": "parent",
			"title":       "Parent",
			"body":        "parent body",
			"author":      "alice",
			"status":      "open",
			"created_at":  "2026-05-01T10:00:00Z",
			"updated_at":  "2026-05-01T10:00:00Z",
		}},
	}
	envPostJSON(t, env, importEndpointPath(project.ID), parent, &struct{}{})
	_, err := env.DB.EnableProjectFederation(context.Background(), project.ID, "tester")
	require.NoError(t, err)
	child := map[string]any{
		"actor":  "importer",
		"source": "beads",
		"items": []map[string]any{{
			"external_id": "child",
			"title":       "Child",
			"body":        "child body",
			"author":      "alice",
			"status":      "open",
			"created_at":  "2026-05-01T10:00:00Z",
			"updated_at":  "2026-05-01T10:00:00Z",
			"links": []map[string]any{{
				"type":               "parent",
				"target_external_id": "parent",
			}},
		}},
	}
	var out struct {
		Created int `json:"created"`
		Links   int `json:"links"`
	}

	envPostJSON(t, env, importEndpointPath(project.ID), child, &out)

	assert.Equal(t, 1, out.Created)
	assert.Equal(t, 1, out.Links)
}

func TestImportEndpoint_FederatedImportLinkAddDeniesOtherPeerLeaseHolder(t *testing.T) {
	ctx := context.Background()
	env := testenv.New(t)
	project := createImportTestProject(t, env, "github.com/wesm/kata", "kata")
	parent := map[string]any{
		"actor":  "importer",
		"source": "beads",
		"items": []map[string]any{{
			"external_id": "parent",
			"title":       "Parent",
			"body":        "parent body",
			"author":      "alice",
			"status":      "open",
			"created_at":  "2026-05-01T10:00:00Z",
			"updated_at":  "2026-05-01T10:00:00Z",
		}},
	}
	envPostJSON(t, env, importEndpointPath(project.ID), parent, &struct{}{})
	parentMapping, err := env.DB.ImportMappingBySource(ctx, project.ID, "beads", "issue", "parent")
	require.NoError(t, err)
	require.NotNil(t, parentMapping.IssueID)
	parentIssue, err := env.DB.IssueByID(ctx, *parentMapping.IssueID)
	require.NoError(t, err)
	_, err = env.DB.EnableProjectFederation(ctx, project.ID, "tester")
	require.NoError(t, err)
	_, err = env.DB.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID,
		IssueRef:  parentIssue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: env.DB.InstanceUID(),
			Holder:            "other",
			ClientKind:        "cli",
		},
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)
	child := map[string]any{
		"actor":  "importer",
		"source": "beads",
		"items": []map[string]any{{
			"external_id": "child",
			"title":       "Child",
			"body":        "child body",
			"author":      "alice",
			"status":      "open",
			"created_at":  "2026-05-01T10:00:00Z",
			"updated_at":  "2026-05-01T10:00:00Z",
			"links": []map[string]any{{
				"type":               "parent",
				"target_external_id": "parent",
			}},
		}},
	}

	resp, raw := envDoRaw(t, env, http.MethodPost, importEndpointPath(project.ID), child, nil)

	assertAPIError(t, resp.StatusCode, raw, http.StatusConflict, "claim_denied")
	_, err = env.DB.ImportMappingBySource(ctx, project.ID, "beads", "issue", "child")
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestImportEndpoint_FederatedImportLinkRemovalDeniesOtherPeerLeaseHolder(t *testing.T) {
	ctx := context.Background()
	env := testenv.New(t)
	project := createImportTestProject(t, env, "github.com/wesm/kata", "kata")
	initial := map[string]any{
		"actor":  "importer",
		"source": "beads",
		"items": []map[string]any{
			{
				"external_id": "source",
				"title":       "Source",
				"body":        "source body",
				"author":      "alice",
				"status":      "open",
				"created_at":  "2026-05-01T10:00:00Z",
				"updated_at":  "2026-05-01T10:00:00Z",
				"links": []map[string]any{{
					"type":               "blocks",
					"target_external_id": "peer",
				}},
			},
			{
				"external_id": "peer",
				"title":       "Peer",
				"body":        "peer body",
				"author":      "alice",
				"status":      "open",
				"created_at":  "2026-05-01T10:00:00Z",
				"updated_at":  "2026-05-01T10:00:00Z",
			},
		},
	}
	envPostJSON(t, env, importEndpointPath(project.ID), initial, &struct{}{})
	sourceMapping, err := env.DB.ImportMappingBySource(ctx, project.ID, "beads", "issue", "source")
	require.NoError(t, err)
	require.NotNil(t, sourceMapping.IssueID)
	peerMapping, err := env.DB.ImportMappingBySource(ctx, project.ID, "beads", "issue", "peer")
	require.NoError(t, err)
	require.NotNil(t, peerMapping.IssueID)
	sourceIssue, err := env.DB.IssueByID(ctx, *sourceMapping.IssueID)
	require.NoError(t, err)
	peerIssue, err := env.DB.IssueByID(ctx, *peerMapping.IssueID)
	require.NoError(t, err)
	link, err := env.DB.LinkByEndpoints(ctx, sourceIssue.ID, peerIssue.ID, "blocks")
	require.NoError(t, err)
	_, err = env.DB.EnableProjectFederation(ctx, project.ID, "tester")
	require.NoError(t, err)
	_, err = env.DB.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID,
		IssueRef:  peerIssue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: env.DB.InstanceUID(),
			Holder:            "other",
			ClientKind:        "cli",
		},
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)
	updateWithoutLink := map[string]any{
		"actor":  "importer",
		"source": "beads",
		"items": []map[string]any{{
			"external_id": "source",
			"title":       "Source without link",
			"body":        "source body",
			"author":      "alice",
			"status":      "open",
			"created_at":  "2026-05-01T10:00:00Z",
			"updated_at":  "2026-05-01T11:00:00Z",
		}},
	}

	resp, raw := envDoRaw(t, env, http.MethodPost, importEndpointPath(project.ID), updateWithoutLink, nil)

	assertAPIError(t, resp.StatusCode, raw, http.StatusConflict, "claim_denied")
	_, err = env.DB.LinkByID(ctx, link.ID)
	assert.NoError(t, err)
}

func TestImportEndpoint_PriorityRoundtrips(t *testing.T) {
	env := testenv.New(t)
	pid := createImportTestProject(t, env, "github.com/wesm/kata", "kata").ID
	body := map[string]any{
		"actor":  "importer",
		"source": "beads",
		"items": []map[string]any{importEndpointItem(map[string]any{
			"external_id": "beads-prio",
			"priority":    1,
		})},
	}
	var out struct {
		Items []struct {
			IssueShortID string `json:"issue_short_id"`
		} `json:"items"`
	}
	envPostJSON(t, env, importEndpointPath(pid), body, &out)
	require.Len(t, out.Items, 1)
	issue, err := env.DB.IssueByShortID(context.Background(), pid, out.Items[0].IssueShortID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.NotNil(t, issue.Priority)
	assert.Equal(t, int64(1), *issue.Priority)
}

func TestImportEndpoint_PriorityOutOfRangeIsValidation(t *testing.T) {
	env := testenv.New(t)
	pid := createImportTestProject(t, env, "github.com/wesm/kata", "kata").ID
	body := map[string]any{
		"actor":  "importer",
		"source": "beads",
		"items": []map[string]any{importEndpointItem(map[string]any{
			"external_id": "beads-bad-prio",
			"priority":    9,
		})},
	}
	resp, raw := envDoRaw(t, env, http.MethodPost, importEndpointPath(pid), body, nil)
	assertAPIError(t, resp.StatusCode, raw, http.StatusBadRequest, "validation")
}

func TestImportEndpoint_RejectsBlankActor(t *testing.T) {
	env := testenv.New(t)
	pid := createImportTestProject(t, env, "github.com/wesm/kata", "kata").ID
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{name: "missing", body: map[string]any{"source": "beads", "items": []map[string]any{}}},
		{name: "blank", body: map[string]any{"actor": "   ", "source": "beads", "items": []map[string]any{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := envDoRaw(t, env, http.MethodPost, importEndpointPath(pid), tc.body, nil)
			assertAPIError(t, resp.StatusCode, body, http.StatusBadRequest, "validation")
			assert.Contains(t, string(body), "actor")
		})
	}
}

func TestImportEndpoint_InvalidImportMapsToValidation(t *testing.T) {
	env := testenv.New(t)
	pid := createImportTestProject(t, env, "github.com/wesm/kata", "kata").ID
	for _, tc := range []struct {
		name string
		item map[string]any
	}{
		{
			name: "status",
			item: importEndpointItem(map[string]any{"status": "bad"}),
		},
		{
			name: "label",
			item: importEndpointItem(map[string]any{"labels": []string{"BadCase"}}),
		},
		{
			name: "empty closed reason",
			item: importEndpointItem(map[string]any{
				"status":        "closed",
				"closed_reason": "",
				"closed_at":     "2026-05-01T10:00:00Z",
			}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := envDoRaw(t, env, http.MethodPost, importEndpointPath(pid), map[string]any{
				"actor":  "importer",
				"source": "beads",
				"items":  []map[string]any{tc.item},
			}, nil)
			assertAPIError(t, resp.StatusCode, body, http.StatusBadRequest, "validation")
		})
	}
}

func importEndpointItem(overrides map[string]any) map[string]any {
	item := map[string]any{
		"external_id": "beads-invalid",
		"title":       "Imported",
		"body":        "body",
		"author":      "alice",
		"status":      "open",
		"created_at":  "2026-05-01T10:00:00Z",
		"updated_at":  "2026-05-01T10:00:00Z",
	}
	for k, v := range overrides {
		item[k] = v
	}
	return item
}

func importEndpointPath(projectID int64) string {
	return "/api/v1/projects/" + strconv.FormatInt(projectID, 10) + "/imports"
}

func createImportTestProject(t *testing.T, env *testenv.Env, _ string, name string) db.Project {
	t.Helper()
	p, err := env.DB.CreateProject(context.Background(), name)
	require.NoError(t, err)
	return p
}

// TestImportEndpoint_AcceptsLargeBody guards against regressing to huma's
// default 1 MiB MaxBodyBytes — large beads imports (a few hundred issues with
// comments) easily exceed 1 MiB, and the importer does all of bd export + per-
// issue bd comments before POSTing, so a 413 wastes minutes of upstream work.
func TestImportEndpoint_AcceptsLargeBody(t *testing.T) {
	env := testenv.New(t)
	pid := createImportTestProject(t, env, "github.com/wesm/kata", "kata").ID
	// 1.5 MiB body field puts the serialized request comfortably above
	// huma's 1 MiB default but well under any reasonable per-endpoint cap.
	largeBody := strings.Repeat("x", 1500000)
	req := map[string]any{
		"actor":  "importer",
		"source": "beads",
		"items": []map[string]any{{
			"external_id": "beads-large",
			"title":       "Imported",
			"body":        largeBody,
			"author":      "alice",
			"status":      "open",
			"created_at":  "2026-05-01T10:00:00Z",
			"updated_at":  "2026-05-01T10:00:00Z",
		}},
	}
	resp, raw := envDoRaw(t, env, http.MethodPost, importEndpointPath(pid), req, nil)
	require.Equalf(t, http.StatusOK, resp.StatusCode,
		"large import body rejected (status=%d body=%s)", resp.StatusCode, string(raw))
}
