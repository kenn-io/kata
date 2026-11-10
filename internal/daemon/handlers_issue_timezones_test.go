package daemon_test

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/internal/testenv"
)

// PATCH owns the matrix; two create cases pin its admission wiring.
func TestIssuePortableTimezoneAdmission(t *testing.T) {
	env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.DefaultTimezone = "Local" })
	p := seedProject(t, env, "example-project")
	check := func(t *testing.T, md map[string]any, want int, create bool) {
		t.Helper()
		if create {
			status, raw := cronHTTP(t, env, http.MethodPost, projectPath(p.ID)+"/issues", map[string]any{"actor": "tester", "title": "Example", "force_new": true, "metadata": md})
			assertAPIError(t, status, raw, http.StatusBadRequest, "invalid_metadata_value")
		}
		issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Existing", Author: "tester"})
		require.NoError(t, err)
		before := recurrenceWriteCursor(t, env)
		md["topic"] = "changed" // A rejection must not apply the accompanying edit.
		status, raw := cronHTTP(t, env, http.MethodPost, fmt.Sprintf("%s/issues/%s/metadata", projectPath(p.ID), issue.ShortID), map[string]any{"actor": "tester", "patch": md})
		require.Equal(t, want, status, string(raw))
		if want == http.StatusBadRequest {
			assertAPIError(t, status, raw, http.StatusBadRequest, "invalid_metadata_value")
			after, err := env.DB.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Equal(t, issue, after)
			require.Equal(t, before, recurrenceWriteCursor(t, env))
		}
	}
	for _, zone := range []string{"UTC", "Etc/UTC", "America/New_York", "Asia/Calcutta", "America/Argentina/Buenos_Aires", "Local", "EST", "US/Eastern", "Etc/GMT+5", "america/New_York", ""} {
		t.Run(zone, func(t *testing.T) {
			want := http.StatusBadRequest
			switch zone {
			case "UTC", "Etc/UTC", "America/New_York", "Asia/Calcutta", "America/Argentina/Buenos_Aires":
				want = http.StatusOK
			}
			check(t, map[string]any{"timezone": zone}, want, zone == "Local")
		})
	}
	for _, tc := range []struct {
		name     string
		metadata map[string]any
		want     int
	}{
		{"civil", map[string]any{"scheduled_on": "2026-11-12T10:30"}, 400},
		{"portable override", map[string]any{"scheduled_on": "2026-11-12", "timezone": "UTC"}, 200},
		{"UTC instant", map[string]any{"scheduled_on": "2026-11-12T10:30:00Z"}, 200},
	} {
		t.Run(tc.name, func(t *testing.T) { check(t, tc.metadata, tc.want, tc.want == http.StatusBadRequest) })
	}
}

func TestIssuePortableTimezoneLegacyCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, stored, patch string
		want                int
		portableDefault     bool
	}{
		{"omitted default", `{}`, `{"$field":"2026-11-12"}`, 200, true},
		{"legacy echo", `{"timezone":"Local","scheduled_on":"2026-11-12"}`, `{"timezone":"Local"}`, 200, false},
		{"legacy schedule edit", `{"timezone":"Local"}`, `{"$field":"2026-11-12"}`, 400, true},
		{"unrelated invalid stored zone", `{"timezone":17}`, `{"topic":"x"}`, 200, false},
		{"restored clear", `{"timezone":17,"$field":"2026-11-12"}`, `{"$field":null}`, 200, false},
		{"restored no-op", `{"timezone":17,"$field":"2026-11-12"}`, `{"$field":"2026-11-12"}`, 200, false},
		{"restored UTC", `{"timezone":17}`, `{"$field":"2026-11-12T10:30:00Z"}`, 200, false},
		{"restored civil", `{"timezone":17}`, `{"$field":"2026-11-12"}`, 400, true},
		{"remove zone", `{"timezone":"UTC","$field":"2026-11-12"}`, `{"timezone":null}`, 400, false},
		{"clear both", `{"timezone":"Local","scheduled_on":"2026-11-12"}`, `{"timezone":null,"scheduled_on":null}`, 200, false},
		{"remove instant zone", `{"timezone":"Local","deadline_on":"2026-11-12T10:30:00Z"}`, `{"timezone":null}`, 200, false},
		{"remove empty zone", `{"timezone":"","scheduled_on":"2026-11-12"}`, `{"timezone":null}`, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t, func(cfg *daemon.ServerConfig) {
				if !tc.portableDefault {
					cfg.DefaultTimezone = "Local"
				}
			})
			p := seedProject(t, env, "example-project")
			fields := []string{"scheduled_on"}
			if strings.Contains(tc.stored+tc.patch, "$field") {
				fields = append(fields, "deadline_on")
			}
			for _, field := range fields {
				issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Legacy", Author: "tester"})
				require.NoError(t, err)
				// Public JSONL restore preserves opaque metadata, including unreadable zones.
				var pe db.ProjectExport
				raw, err := json.Marshal(p)
				require.NoError(t, err)
				require.NoError(t, json.Unmarshal(raw, &pe))
				var ie db.IssueExport
				raw, err = json.Marshal(issue)
				require.NoError(t, err)
				require.NoError(t, json.Unmarshal(raw, &ie))
				ie.Metadata = jsontext.Value(strings.ReplaceAll(tc.stored, "$field", field))
				var backup strings.Builder
				encoder := jsonl.NewEncoder(&backup)
				for _, record := range []db.ImportRecord{&db.MetaKV{Key: "export_version", Value: fmt.Sprint(db.CurrentSchemaVersion())}, &pe, &ie} {
					raw, err := json.Marshal(record)
					require.NoError(t, err)
					require.NoError(t, encoder.Write(jsonl.Envelope{Kind: jsonl.Kind(record.ImportKind()), Data: raw}))
				}
				require.NoError(t, jsonl.Import(t.Context(), strings.NewReader(backup.String()), env.DB))
				resp := doPost(t, env, fmt.Sprintf("%s%s/issues/%s/metadata", env.URL, projectPath(p.ID), issue.ShortID), `{"actor":"tester","patch":`+strings.ReplaceAll(tc.patch, "$field", field)+`}`)
				body := readClose(t, resp)
				require.Equal(t, tc.want, resp.StatusCode, string(body))
				if tc.want == http.StatusBadRequest {
					assertAPIError(t, resp.StatusCode, body, http.StatusBadRequest, "invalid_metadata_value")
				}
				if tc.portableDefault && tc.want == 400 {
					require.Contains(t, string(body), "effective issue timezone")
				}
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
		rec, _, err := store.CreateRecurrence(t.Context(), db.CreateRecurrenceIn{ProjectID: p.ID, Actor: "tester", Rule: "FREQ=DAILY", DTStart: "2026-11-12", Timezone: "US/Eastern", Template: db.RecurrenceTemplate{Title: "Daily example"}})
		require.NoError(t, err)
		out, err := store.MaterializeNext(t.Context(), rec.ID, "2026-11-11", "tester")
		require.NoError(t, err)
		occurrence, err := store.IssueByUID(t.Context(), out.NewIssueUID, db.IncludeDeletedNo)
		require.NoError(t, err)
		path = fmt.Sprintf("%s/issues/%s/metadata", projectPath(p.ID), occurrence.ShortID)
		status, raw = cronHTTP(t, env, http.MethodPost, path, map[string]any{"actor": "tester", "patch": map[string]string{"scheduled_on": "2026-11-13"}})
		require.Equal(t, http.StatusOK, status, string(raw))
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
	env := testenv.New(t)
	p := seedProject(t, env, "example-project")
	rec := seedRecurrence(t, env, p.ID, "FREQ=DAILY", "2026-11-12", "US/Eastern", "Daily example")
	out, err := env.DB.MaterializeNext(t.Context(), rec.ID, "2026-11-11", "tester")
	require.NoError(t, err)
	issue, err := env.DB.IssueByUID(t.Context(), out.NewIssueUID, db.IncludeDeletedNo)
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
	// A restored issue override differing from the recurrence owns admission.
	_, err = env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "tester", Patch: map[string]jsontext.Value{"timezone": jsontext.Value(`"EST"`)}})
	require.NoError(t, err)
	status, raw := cronHTTP(t, env, http.MethodPost, path, map[string]any{"actor": "tester", "patch": map[string]any{"scheduled_on": "2026-11-16"}})
	assertAPIError(t, status, raw, http.StatusBadRequest, "invalid_metadata_value")
}
