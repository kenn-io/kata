package daemon_test

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// Create and PATCH both prepare schedules and reject unportable zones atomically.
func TestIssuePortableTimezoneAdmission(t *testing.T) {
	env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.DefaultTimezone = "Local" })
	p := seedProject(t, env, "example-project")
	for _, tc := range []struct {
		name     string
		metadata map[string]any
		want     int
	}{
		{"reject", map[string]any{"timezone": "Local", "scheduled_on": "2026-11-12"}, http.StatusBadRequest},
		{"accept", map[string]any{"timezone": "UTC", "scheduled_on": "2026-11-12"}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := cronHTTP(t, env, http.MethodPost, projectPath(p.ID)+"/issues", map[string]any{"actor": "tester", "title": "Example", "force_new": true, "metadata": tc.metadata})
			require.Equal(t, tc.want, status, string(raw))
			if tc.want == http.StatusBadRequest {
				assertAPIError(t, status, raw, http.StatusBadRequest, "invalid_metadata_value")
			}
			issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Existing", Author: "tester"})
			require.NoError(t, err)
			before := recurrenceWriteCursor(t, env)
			patch := map[string]any{"topic": "changed"}
			for key, value := range tc.metadata {
				patch[key] = value
			}
			status, raw = cronHTTP(t, env, http.MethodPost, fmt.Sprintf("%s/issues/%s/metadata", projectPath(p.ID), issue.ShortID), map[string]any{"actor": "tester", "patch": patch})
			require.Equal(t, tc.want, status, string(raw))
			if tc.want == http.StatusBadRequest {
				assertAPIError(t, status, raw, http.StatusBadRequest, "invalid_metadata_value")
				after, err := env.DB.IssueByID(t.Context(), issue.ID)
				require.NoError(t, err)
				require.Equal(t, issue, after)
				require.Equal(t, before, recurrenceWriteCursor(t, env))
			} else {
				require.Contains(t, string(raw), `"tzdata_version"`)
			}
		})
	}
}

func TestIssuePortableTimezoneTransactionCurrent(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		storage := &projectAccessBeforeIssueMutationStore{Storage: store}
		d := daemon.NewServer(daemon.ServerConfig{DB: storage, StartedAt: time.Now().UTC(), DefaultTimezone: "Local", Broadcaster: daemon.NewEventBroadcaster()})
		t.Cleanup(func() { require.NoError(t, d.Close()) })
		server := httptest.NewServer(d.Handler())
		t.Cleanup(server.Close)
		env := &testenv.Env{URL: server.URL, HTTP: server.Client()}
		exec := store.(interface {
			ExecContext(context.Context, string, ...any) (sql.Result, error)
		}).ExecContext
		p, err := store.CreateProject(t.Context(), "example-project")
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Example", Author: "tester", Metadata: map[string]jsontext.Value{"timezone": jsontext.Value(`"UTC"`)}})
		require.NoError(t, err)
		storage.before = func(ctx context.Context, _ int64) error {
			_, err := exec(ctx, `UPDATE issues SET metadata = '{}' WHERE id = $1`, issue.ID)
			return err
		}
		path := fmt.Sprintf("%s/issues/%s/metadata", projectPath(p.ID), issue.ShortID)
		status, raw := cronHTTP(t, env, http.MethodPost, path, map[string]any{"actor": "tester", "patch": map[string]string{"deadline_on": "2026-11-12"}})
		assertAPIError(t, status, raw, http.StatusBadRequest, "invalid_metadata_value")
		after, err := store.IssueByID(t.Context(), issue.ID)
		require.NoError(t, err)
		require.JSONEq(t, `{}`, string(after.Metadata))
		require.Equal(t, issue.Revision, after.Revision)

	})
}

func TestIssuePortableTimezoneRevisionPrecedesAdmission(t *testing.T) {
	env := testenv.New(t)
	p := seedProject(t, env, "example-project")
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Example", Author: "tester"})
	require.NoError(t, err)
	path := fmt.Sprintf("%s/issues/%s/metadata", projectPath(p.ID), issue.ShortID)
	resp, raw := envDoRaw(t, env, http.MethodPost, path, map[string]any{"actor": "tester", "patch": map[string]string{"timezone": "Local"}}, map[string]string{"If-Match": `"rev-999"`})
	assertAPIError(t, resp.StatusCode, raw, http.StatusPreconditionFailed, "revision_conflict")
}

// Contract: successful idempotent creates remain replayable through upgrades;
// a replay changes no metadata, including a stored legacy timezone.
func TestIssuePortableTimezoneLegacyCreateRetry(t *testing.T) {
	env := testenv.New(t)
	p := seedProject(t, env, "example-project")
	md := map[string]jsontext.Value{"timezone": jsontext.Value(`"Local"`)}
	_, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: p.ID, Title: "Legacy example", Author: "tester", Metadata: md,
		IdempotencyKey: "legacy-create", IdempotencyFingerprint: db.Fingerprint("Legacy example", "", nil, nil, nil, nil, md),
	})
	require.NoError(t, err)
	before := recurrenceWriteCursor(t, env)
	resp, raw := envDoRaw(t, env, http.MethodPost, projectPath(p.ID)+"/issues",
		map[string]any{"actor": "tester", "title": "Legacy example", "metadata": md},
		map[string]string{"Idempotency-Key": "legacy-create"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), `"reused":true`)
	require.Equal(t, before, recurrenceWriteCursor(t, env))
}

func TestIssuePortableTimezoneCopiedRecurrence(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		d := daemon.NewServer(daemon.ServerConfig{DB: store, StartedAt: time.Now().UTC(), DefaultTimezone: "Local", Broadcaster: daemon.NewEventBroadcaster()})
		t.Cleanup(func() { require.NoError(t, d.Close()) })
		server := httptest.NewServer(d.Handler())
		t.Cleanup(server.Close)
		env := &testenv.Env{URL: server.URL, HTTP: server.Client()}
		p, err := store.CreateProject(t.Context(), "example-project")
		require.NoError(t, err)
		rec, _, err := store.CreateRecurrence(t.Context(), db.CreateRecurrenceIn{ProjectID: p.ID, Actor: "tester", Rule: "FREQ=DAILY", DTStart: "2026-11-12", Timezone: "US/Eastern", Template: db.RecurrenceTemplate{Title: "Daily example"}})
		require.NoError(t, err)
		out, err := store.MaterializeNext(t.Context(), rec.ID, "2026-11-11", "tester")
		require.NoError(t, err)
		issue, err := store.IssueByUID(t.Context(), out.NewIssueUID, db.IncludeDeletedNo)
		require.NoError(t, err)
		path := fmt.Sprintf("%s/issues/%s/metadata", projectPath(p.ID), issue.ShortID)
		for _, tc := range []struct {
			patch string
			want  int
		}{
			{`{"scheduled_on":"2026-11-13"}`, 200},
			{`{"deadline_on":"2026-11-14"}`, 400},
			{`{"timezone":"Local"}`, 400},
			{`{"timezone":null,"scheduled_on":"2026-11-15"}`, 200},
		} {
			resp := doPost(t, env, env.URL+path, `{"actor":"tester","patch":`+tc.patch+`}`)
			body := readClose(t, resp)
			require.Equal(t, tc.want, resp.StatusCode, string(body))
			if tc.want == http.StatusBadRequest {
				assertAPIError(t, resp.StatusCode, body, http.StatusBadRequest, "invalid_metadata_value")
			}
		}
		_, err = store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "tester", Patch: map[string]jsontext.Value{"timezone": jsontext.Value(`"EST"`)}})
		require.NoError(t, err)
		status, raw := cronHTTP(t, env, http.MethodPost, path, map[string]any{"actor": "tester", "patch": map[string]any{"scheduled_on": "2026-11-16"}})
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Contains(t, string(raw), `"timezone":"US/Eastern"`)
	})
}
