package main

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func reapFixtureServer(t *testing.T, route http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects":
			_, _ = io.WriteString(w, `{"projects":[{"id":7,"name":"spoke-project"}]}`)
		case "/api/v1/projects/7":
			_, _ = io.WriteString(w, `{"project":{"id":7,"name":"spoke-project"}}`)
		default:
			route(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

const reapRunJSON = `{"uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","revision":4,"status":"running","updated_at":"2026-11-01T00:00:00Z","created_at":"2026-11-01T00:00:00Z","staleness":{"stale":true,"stale_at":"2026-11-02T00:00:00Z","basis":"default"}}`

func TestCronReapDryRunAndApply(t *testing.T) {
	writes := 0
	s := reapFixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"runs":[`+reapRunJSON+`]}`)
			return
		}
		writes++
		require.True(t, strings.HasSuffix(r.URL.Path, "/reap"))
		var body map[string]any
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &body))
		require.Equal(t, float64(4), body["expected_revision"])
		require.Equal(t, "operator", body["actor"])
		_, _ = io.WriteString(w, `{"run":`+strings.ReplaceAll(strings.Replace(reapRunJSON, `"running"`, `"unknown"`, 1), `"revision":4`, `"revision":5`)+`,"events":[{"uid":"01ARZ3NDEKTSV4RRFFQ69G5FB0","type":"cron.run.observed"}]}`)
	})
	t.Setenv("KATA_AUTHOR", "operator")
	out, _, err := executeCronCLI(t, s.URL, nil, "run", "reap")
	require.NoError(t, err)
	var preview map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &preview))
	require.Equal(t, true, preview["dry_run"])
	require.Equal(t, 0, writes)
	out, _, err = executeCronCLI(t, s.URL, nil, "run", "reap", "--apply")
	require.NoError(t, err)
	var applied map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &applied))
	require.Equal(t, "reaped", applied["runs"].([]any)[0].(map[string]any)["action"])
	require.Equal(t, 1, writes)
}

func TestCronReapConflictsAndFailures(t *testing.T) {
	for _, tc := range []struct {
		status     int
		code, want string
		ok         bool
	}{
		{409, "cron_conflict", "changed", true}, {409, "cron_run_not_running", "not_running", true}, {409, "cron_run_not_stale", "not_stale", true},
		{409, "other_conflict", "", false}, {400, "validation", "", false}, {500, "internal", "", false}, {404, "not_found", "", false},
	} {
		t.Run(fmt.Sprintf("%d-%s", tc.status, tc.code), func(t *testing.T) {
			s := reapFixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, `{"runs":[`+reapRunJSON+`]}`)
					return
				}
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":"fixture failure"}}`, tc.code)
			})
			out, _, err := executeCronCLI(t, s.URL, nil, "run", "reap", "--apply")
			if tc.ok {
				require.NoError(t, err)
				require.Contains(t, out, tc.want)
			} else {
				require.Error(t, err)
				require.Contains(t, out, "fixture failure")
			}
		})
	}
}

func TestCronReapOldDaemon(t *testing.T) {
	for _, routeStatus := range []int{0, 404, 405} {
		t.Run(fmt.Sprint(routeStatus), func(t *testing.T) {
			s := reapFixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					record := reapRunJSON
					if routeStatus == 0 {
						record = strings.Replace(record, `,"staleness":{"stale":true,"stale_at":"2026-11-02T00:00:00Z","basis":"default"}`, "", 1)
					}
					_, _ = io.WriteString(w, `{"runs":[`+record+`]}`)
					return
				}
				w.WriteHeader(routeStatus)
				_, _ = io.WriteString(w, "route unavailable")
			})
			_, _, err := executeCronCLI(t, s.URL, nil, "run", "reap", "--apply")
			require.ErrorContains(t, err, "daemon does not support run reap; upgrade the daemon")
		})
	}
}

func TestCronReapBoundedScan(t *testing.T) {
	reads := 0
	s := reapFixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		reads++
		if reads == 1 {
			require.Equal(t, "2", r.URL.Query().Get("limit"))
		}
		record := reapRunJSON
		cursor := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
		if reads == 2 {
			require.Equal(t, cursor, r.URL.Query().Get("before_uid"))
			record = strings.ReplaceAll(record, cursor, "01ARZ3NDEKTSV4RRFFQ69G5FAW")
			cursor = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
		}
		_, _ = io.WriteString(w, `{"runs":[`+record+`],"next_before_uid":"`+cursor+`"}`)
	})
	out, _, err := executeCronCLI(t, s.URL, nil, "run", "reap", "--scan-limit", "2")
	require.NoError(t, err)
	require.LessOrEqual(t, reads, 2)
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	require.Equal(t, float64(2), result["scanned"])
}

func TestCronReapNamedIdentityMismatch(t *testing.T) {
	writes := 0
	s := reapFixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"run":`+strings.ReplaceAll(reapRunJSON, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "01ARZ3NDEKTSV4RRFFQ69G5FAW")+`}`)
			return
		}
		writes++
		_, _ = io.WriteString(w, `{"run":{"uid":"01ARZ3NDEKTSV4RRFFQ69G5FAW","status":"unknown","revision":5},"events":[]}`)
	})
	_, _, err := executeCronCLI(t, s.URL, nil, "run", "reap", "01ARZ3NDEKTSV4RRFFQ69G5FAV", "--apply")
	require.Error(t, err)
	require.Zero(t, writes, "a show response cannot redirect an explicit reap")
}

func TestCronReapMalformedSuccess(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"missing revision", `{"run":{"uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"unknown"}}`},
		{"revision did not advance", `{"run":{"uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"unknown","revision":4},"events":[{"uid":"01ARZ3NDEKTSV4RRFFQ69G5FB0","type":"cron.run.observed"}]}`},
		{"committed event missing", `{"run":{"uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","status":"unknown","revision":5},"events":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := reapFixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, `{"runs":[`+reapRunJSON+`]}`)
				} else {
					_, _ = io.WriteString(w, tc.body)
				}
			})
			_, _, err := executeCronCLI(t, s.URL, nil, "run", "reap", "--apply")
			require.ErrorContains(t, err, "daemon returned an invalid reap result; inspect the run before retrying")
		})
	}
}

func TestCronReapCursorCycle(t *testing.T) {
	ids := []string{"01ARZ3NDEKTSV4RRFFQ69G5FAV", "01ARZ3NDEKTSV4RRFFQ69G5FAW", "01ARZ3NDEKTSV4RRFFQ69G5FAX", "01ARZ3NDEKTSV4RRFFQ69G5FAY"}
	reads := 0
	s := reapFixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		i := reads
		reads++
		record := strings.ReplaceAll(reapRunJSON, ids[0], ids[i])
		cursor := ids[i]
		if i == 2 {
			cursor = ids[0]
		}
		if i == 3 {
			cursor = ""
		}
		_, _ = fmt.Fprintf(w, `{"runs":[%s],"next_before_uid":%q}`, record, cursor)
	})
	_, _, err := executeCronCLI(t, s.URL, nil, "run", "reap", "--scan-limit", "4")
	require.ErrorContains(t, err, "daemon could not provide the next results")
	require.Equal(t, 3, reads)
}

func TestCronReapPartialFailureStopsWrites(t *testing.T) {
	ids := []string{"01ARZ3NDEKTSV4RRFFQ69G5FAV", "01ARZ3NDEKTSV4RRFFQ69G5FAW", "01ARZ3NDEKTSV4RRFFQ69G5FAX"}
	writes := 0
	s := reapFixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			records := []string{}
			for _, id := range ids {
				records = append(records, strings.ReplaceAll(reapRunJSON, ids[0], id))
			}
			_, _ = io.WriteString(w, `{"runs":[`+strings.Join(records, ",")+`]}`)
			return
		}
		writes++
		if writes == 2 {
			w.WriteHeader(500)
			_, _ = io.WriteString(w, `{"error":{"code":"internal","message":"fixture failure"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"run":{"uid":"`+ids[0]+`","status":"unknown","revision":5},"events":[{"uid":"01ARZ3NDEKTSV4RRFFQ69G5FB0","type":"cron.run.observed"}]}`)
	})
	out, _, err := executeCronCLI(t, s.URL, nil, "run", "reap", "--apply")
	require.Error(t, err)
	require.Equal(t, 2, writes)
	var report map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	items := report["runs"].([]any)
	require.Len(t, items, 2)
	require.Equal(t, "reaped", items[0].(map[string]any)["action"])
	require.Contains(t, items[1].(map[string]any)["error"], "fixture failure")
}

func TestCronReapRejectsMalformedRead(t *testing.T) {
	s := reapFixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		_, _ = io.WriteString(w, `{"runs":[`+strings.ReplaceAll(reapRunJSON, `"revision":4,`, "")+`]}`)
	})
	_, _, err := executeCronCLI(t, s.URL, nil, "run", "reap")
	require.ErrorContains(t, err, "daemon returned invalid run data")
}

func TestCronReapRejectsMissingOrDuplicateRunIdentifiers(t *testing.T) {
	for _, tc := range []struct{ name, runs string }{
		{"missing identifier", strings.Replace(reapRunJSON, `"uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV",`, "", 1)},
		{"duplicate identifier", reapRunJSON + "," + reapRunJSON},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := reapFixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"runs":[`+tc.runs+`]}`)
			})
			_, _, err := executeCronCLI(t, s.URL, nil, "run", "reap")
			require.ErrorContains(t, err, "daemon returned missing or duplicate run identifiers")
		})
	}
}

func TestCronReapNamedDeduplicationAndForcePreview(t *testing.T) {
	id := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	reads := 0
	s := reapFixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.True(t, strings.HasSuffix(r.URL.Path, id))
		reads++
		_, _ = io.WriteString(w, `{"run":`+strings.Replace(reapRunJSON, `"stale":true`, `"stale":false`, 1)+`}`)
	})
	out, _, err := executeCronCLI(t, s.URL, nil, "run", "reap", id, strings.ToLower(id), "--force")
	require.NoError(t, err)
	require.Equal(t, 1, reads)
	var report map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	require.Equal(t, true, report["dry_run"])
	require.Len(t, report["runs"], 1)
}
