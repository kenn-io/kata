package daemon_test

import (
	"context"
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
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/testenv"
)

// New timezone choices must be portable; existing stored choices stay usable.
func TestIssuePortableTimezoneAdmission(t *testing.T) {
	env := testenv.New(t)
	p := seedProject(t, env, "example-project")
	for _, zone := range []string{"UTC", "Etc/UTC", "America/New_York", "Asia/Calcutta", "America/Argentina/Buenos_Aires", "Local", "EST", "US/Eastern", "Etc/GMT+5", "+05:00", "america/New_York", "America/Imaginary", ""} {
		t.Run(zone, func(t *testing.T) {
			want := http.StatusBadRequest
			switch zone {
			case "UTC", "Etc/UTC", "America/New_York", "Asia/Calcutta", "America/Argentina/Buenos_Aires":
				want = http.StatusOK
			}
			status, raw := cronHTTP(t, env, http.MethodPost, projectPath(p.ID)+"/issues", map[string]any{"actor": "tester", "title": "Example", "force_new": true, "metadata": map[string]any{"timezone": zone}})
			require.Equal(t, want, status, string(raw))
			issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Existing", Author: "tester"})
			require.NoError(t, err)
			before := recurrenceWriteCursor(t, env)
			status, raw = cronHTTP(t, env, http.MethodPost, fmt.Sprintf("%s/issues/%s/metadata", projectPath(p.ID), issue.ShortID), map[string]any{"actor": "tester", "patch": map[string]any{"timezone": zone, "topic": "changed"}})
			require.Equal(t, want, status, string(raw))
			if want == http.StatusBadRequest {
				require.Contains(t, string(raw), "invalid_metadata_value")
				after, err := env.DB.IssueByID(t.Context(), issue.ID)
				require.NoError(t, err)
				require.Equal(t, issue, after)
				require.Equal(t, before, recurrenceWriteCursor(t, env))
			}
		})
	}
}

func TestIssuePortableDefaultAdmission(t *testing.T) {
	env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.DefaultTimezone = "Local" })
	p := seedProject(t, env, "example-project")
	for _, field := range []string{"scheduled_on", "deadline_on"} {
		for _, value := range []string{"2026-11-12", "2026-11-12T10:30", "2026-11-12T10:30:00Z"} {
			for _, zone := range []string{"", "UTC"} {
				t.Run(field+value+zone, func(t *testing.T) {
					md := map[string]any{field: value}
					if zone != "" {
						md["timezone"] = zone
					}
					want := http.StatusOK
					if zone == "" && value != "2026-11-12T10:30:00Z" {
						want = http.StatusBadRequest
					}
					status, raw := cronHTTP(t, env, http.MethodPost, projectPath(p.ID)+"/issues", map[string]any{"actor": "tester", "title": "Example", "force_new": true, "metadata": md})
					require.Equal(t, want, status, string(raw))
					issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Existing", Author: "tester"})
					require.NoError(t, err)
					path := fmt.Sprintf("%s/issues/%s/metadata", projectPath(p.ID), issue.ShortID)
					status, raw = cronHTTP(t, env, http.MethodPost, path, map[string]any{"actor": "tester", "patch": md})
					require.Equal(t, want, status, string(raw))
					if want == http.StatusBadRequest {
						require.Contains(t, string(raw), "config.toml")
					}
				})
			}
		}
	}
}

func TestIssuePortableTimezoneLegacyCompatibility(t *testing.T) {
	env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.DefaultTimezone = "Local" })
	p := seedProject(t, env, "example-project")
	for _, tc := range []struct {
		name, stored, patch string
		want                int
	}{
		{"legacy echo", `{"timezone":"Local","scheduled_on":"2026-11-12"}`, `{"timezone":"Local"}`, 200},
		{"legacy schedule edit", `{"timezone":"Local"}`, `{"scheduled_on":"2026-11-12"}`, 200},
		{"unrelated", `{"scheduled_on":"2026-11-12"}`, `{"topic":"x"}`, 200},
		{"civil no-op", `{"scheduled_on":"2026-11-12"}`, `{"scheduled_on":"2026-11-12"}`, 200},
		{"clear date", `{"scheduled_on":"2026-11-12"}`, `{"scheduled_on":null}`, 200},
		{"remove zone", `{"timezone":"UTC","scheduled_on":"2026-11-12"}`, `{"timezone":null}`, 400},
		{"remove deadline zone", `{"timezone":"UTC","deadline_on":"2026-11-12"}`, `{"timezone":null}`, 400},
		{"clear both", `{"timezone":"Local","scheduled_on":"2026-11-12"}`, `{"timezone":null,"scheduled_on":null}`, 200},
		{"remove instant zone", `{"timezone":"Local","deadline_on":"2026-11-12T10:30:00Z"}`, `{"timezone":null}`, 200},
		{"remove empty zone", `{"timezone":"","scheduled_on":"2026-11-12"}`, `{"timezone":null}`, 200},
		{"clear unused zone", `{"timezone":"Local"}`, `{"timezone":null}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Legacy", Author: "tester"})
			require.NoError(t, err)
			_, err = env.DB.ExecContext(t.Context(), `UPDATE issues SET metadata = ? WHERE id = ?`, tc.stored, issue.ID)
			require.NoError(t, err)
			resp := doPost(t, env, fmt.Sprintf("%s%s/issues/%s/metadata", env.URL, projectPath(p.ID), issue.ShortID), `{"actor":"tester","patch":`+tc.patch+`}`)
			require.Equal(t, tc.want, resp.StatusCode, string(readClose(t, resp)))
		})
	}
	// An omitted daemon zone still means UTC.
	utc := testenv.New(t)
	up := seedProject(t, utc, "example-project")
	status, raw := cronHTTP(t, utc, http.MethodPost, projectPath(up.ID)+"/issues", map[string]any{"actor": "tester", "title": "Example", "force_new": true, "metadata": map[string]string{"scheduled_on": "2026-11-12"}})
	require.Equal(t, http.StatusOK, status, string(raw))
}

func TestIssuePortableDefaultRecurrenceFallback(t *testing.T) {
	env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.DefaultTimezone = "Local" })
	p := seedProject(t, env, "example-project")
	rec := seedRecurrence(t, env, p.ID, "FREQ=WEEKLY", "2026-11-12", "Local", "Weekly example")
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Occurrence", Author: "tester", Metadata: map[string]jsontext.Value{"timezone": jsontext.Value(`"UTC"`)}})
	require.NoError(t, err)
	_, err = env.DB.ExecContext(t.Context(), `UPDATE issues SET recurrence_id = ? WHERE id = ?`, rec.ID, issue.ID)
	require.NoError(t, err)
	path := fmt.Sprintf("%s/issues/%s/metadata", projectPath(p.ID), issue.ShortID)
	status, raw := cronHTTP(t, env, http.MethodPost, path, map[string]any{"actor": "tester", "patch": map[string]any{"timezone": nil, "scheduled_on": "2026-11-13"}})
	require.Equal(t, http.StatusOK, status, string(raw))
	status, raw = cronHTTP(t, env, http.MethodPost, path, map[string]any{"actor": "tester", "patch": map[string]any{"deadline_on": "2026-11-14"}})
	require.Equal(t, http.StatusBadRequest, status, string(raw))
}

// Simulate a committed concurrent write after the handler read, before its
// metadata transaction. Admission must inspect the transaction's current row.
type timezoneInterleaveStorage struct {
	db.Storage
	beforePatch func(context.Context) error
}

func (s *timezoneInterleaveStorage) PatchIssueMetadata(ctx context.Context, in db.PatchIssueMetadataIn) (db.PatchIssueMetadataOut, error) {
	if err := s.beforePatch(ctx); err != nil {
		return db.PatchIssueMetadataOut{}, err
	}
	return s.Storage.PatchIssueMetadata(ctx, in)
}

func TestIssuePortableTimezoneTransactionCurrent(t *testing.T) {
	var storage *timezoneInterleaveStorage
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.DefaultTimezone = "Local"
		storage = &timezoneInterleaveStorage{Storage: cfg.DB}
		cfg.DB = storage
	})
	p := seedProject(t, env, "example-project")
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Example", Author: "tester", Metadata: map[string]jsontext.Value{"timezone": jsontext.Value(`"UTC"`)}})
	require.NoError(t, err)
	storage.beforePatch = func(ctx context.Context) error {
		_, err := env.DB.ExecContext(ctx, `UPDATE issues SET metadata = '{}' WHERE id = ?`, issue.ID)
		return err
	}
	status, raw := cronHTTP(t, env, http.MethodPost, fmt.Sprintf("%s/issues/%s/metadata", projectPath(p.ID), issue.ShortID), map[string]any{"actor": "tester", "patch": map[string]string{"scheduled_on": "2026-11-12"}})
	require.Equal(t, http.StatusBadRequest, status, string(raw))
	after, err := env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(after.Metadata))
}

func TestIssuePortableTimezonePatchGuards(t *testing.T) {
	env := testenv.New(t)
	p := seedProject(t, env, "example-project")
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Example", Author: "tester"})
	require.NoError(t, err)
	path := fmt.Sprintf("%s/issues/%s/metadata", projectPath(p.ID), issue.ShortID)
	before := recurrenceWriteCursor(t, env)
	resp, raw := envDoRaw(t, env, http.MethodPost, path, map[string]any{"actor": "tester", "patch": map[string]string{"timezone": "Local"}}, map[string]string{"If-Match": `"rev-999"`})
	assertAPIError(t, resp.StatusCode, raw, http.StatusPreconditionFailed, "revision_conflict")
	status, raw := cronHTTP(t, env, http.MethodPost, path, map[string]any{"actor": "tester", "patch": map[string]string{"timezone": "UTC"}, "guard": map[string]any{"key": "timezone", "if_value": `"Local"`}})
	assertAPIError(t, status, raw, http.StatusConflict, "metadata_guard_failed")
	after, err := env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, issue, after)
	require.Equal(t, before, recurrenceWriteCursor(t, env))
}

func TestIssuePortableTimezonePostgres(t *testing.T) {
	dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	storage := &timezoneInterleaveStorage{Storage: store}
	d := daemon.NewServer(daemon.ServerConfig{DB: storage, StartedAt: time.Now().UTC(), DefaultTimezone: "Local", Broadcaster: daemon.NewEventBroadcaster()})
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	server := httptest.NewServer(d.Handler())
	t.Cleanup(server.Close)
	env := &testenv.Env{URL: server.URL, HTTP: server.Client()}
	p, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	status, raw := cronHTTP(t, env, http.MethodPost, projectPath(p.ID)+"/issues", map[string]any{"actor": "tester", "title": "Example", "metadata": map[string]string{"timezone": "Local"}})
	require.Equal(t, http.StatusBadRequest, status, string(raw))
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Example", Author: "tester", Metadata: map[string]jsontext.Value{"timezone": jsontext.Value(`"UTC"`)}})
	require.NoError(t, err)
	storage.beforePatch = func(ctx context.Context) error {
		_, err := store.ExecContext(ctx, `UPDATE issues SET metadata = '{}' WHERE id = $1`, issue.ID)
		return err
	}
	path := fmt.Sprintf("%s/issues/%s/metadata", projectPath(p.ID), issue.ShortID)
	status, raw = cronHTTP(t, env, http.MethodPost, path, map[string]any{"actor": "tester", "patch": map[string]string{"scheduled_on": "2026-11-12"}})
	require.Equal(t, http.StatusBadRequest, status, string(raw))
	after, err := store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(after.Metadata))
	require.Equal(t, issue.Revision, after.Revision)
}
