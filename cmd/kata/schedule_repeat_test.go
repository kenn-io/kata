package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

type recurrenceRow struct {
	recurrenceView
	TemplateTitle    string   `json:"template_title"`
	TemplateBody     string   `json:"template_body"`
	TemplateOwner    *string  `json:"template_owner"`
	TemplatePriority *int64   `json:"template_priority"`
	TemplateLabels   []string `json:"template_labels"`
}

func listRecurrencesViaHTTP(t *testing.T, env *testenv.Env, pid int64) []recurrenceRow {
	t.Helper()
	body := getJSON[struct {
		Recurrences []recurrenceRow `json:"recurrences"`
	}](t, env.URL+"/api/v1/projects/"+itoa(pid)+"/recurrences")
	return body.Recurrences
}

// isolateCallerZone points both caller-zone sources at nothing so a test sees
// only the zone it sets itself, whatever the machine running it is set to.
func isolateCallerZone(t *testing.T) {
	t.Helper()
	t.Setenv("TZ", "")
	require.NoError(t, os.Unsetenv("TZ"))
	prev := localtimePath
	localtimePath = filepath.Join(t.TempDir(), "no-localtime")
	t.Cleanup(func() { localtimePath = prev })
}

func TestScheduleRepeatStartsRecurrenceFromTheIssue(t *testing.T) {
	isolateCallerZone(t)
	env, dir, pid := setupCLIWorkspace(t)
	created := postJSON[struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
	}](t, env.URL+"/api/v1/projects/"+itoa(pid)+"/issues", map[string]any{
		"actor": "tester", "title": "Water the plants", "body": "front room first",
		"owner": "alice", "priority": 2, "labels": []string{"home"},
	})
	ref := created.Issue.ShortID

	out := runCLI(t, env, dir, "schedule", ref, "2026-11-02", "--repeat", "FREQ=WEEKLY", "--timezone", "Europe/Paris")
	assert.Contains(t, out, "scheduled_on: 2026-11-02")
	assert.Contains(t, out, "timezone: Europe/Paris")

	recs := listRecurrencesViaHTTP(t, env, pid)
	require.Len(t, recs, 1)
	assert.Equal(t, "FREQ=WEEKLY", recs[0].RRule)
	assert.Equal(t, "2026-11-02", recs[0].DTStart)
	assert.Equal(t, "Europe/Paris", recs[0].Timezone)
	assert.Equal(t, "Water the plants", recs[0].TemplateTitle)
	assert.Equal(t, "front room first", recs[0].TemplateBody)
	require.NotNil(t, recs[0].TemplatePriority)
	assert.EqualValues(t, 2, *recs[0].TemplatePriority)
	assert.Equal(t, []string{"home"}, recs[0].TemplateLabels)
	assert.Nil(t, recs[0].TemplateOwner, "the owner must not carry to later occurrences")

	issue := fetchMetaIssueViaHTTP(t, env, pid, ref)
	require.JSONEq(t, `{"scheduled_on":"2026-11-02","timezone":"Europe/Paris"}`, string(issue.Issue.Metadata))
}

// A daemon older than API 0.24.0 returns labels only in the top-level labels
// array of a show response, not under issue.labels.
func TestScheduleRepeatKeepsLabelsFromAnOlderShowResponse(t *testing.T) {
	isolateCallerZone(t)
	env, dir, pid := setupCLIWorkspace(t)
	created := postJSON[struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
	}](t, env.URL+"/api/v1/projects/"+itoa(pid)+"/issues", map[string]any{
		"actor": "tester", "title": "Water the plants", "labels": []string{"home"},
	})
	ref := created.Issue.ShortID
	showPath := "/api/v1/projects/" + itoa(pid) + "/issues/" + ref
	older := interceptingProxy(t, env, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet || r.URL.Path != showPath {
			return false
		}
		var shown map[string]jsontext.Value
		require.NoError(t, json.Unmarshal(getJSON[jsontext.Value](t, env.URL+r.URL.RequestURI()), &shown))
		var issue map[string]jsontext.Value
		require.NoError(t, json.Unmarshal(shown["issue"], &issue))
		delete(issue, "labels")
		encoded, err := json.Marshal(issue)
		require.NoError(t, err)
		shown["issue"] = encoded
		body, err := json.Marshal(shown)
		require.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
		return true
	})

	_, stderr, err := runCLIAgainst(t, older, dir, "schedule", ref, "2026-11-02", "--repeat", "FREQ=DAILY", "--timezone", "UTC")
	require.NoError(t, err, stderr)

	recs := listRecurrencesViaHTTP(t, env, pid)
	require.Len(t, recs, 1)
	assert.Equal(t, []string{"home"}, recs[0].TemplateLabels)
}

func TestScheduleRepeatZoneSources(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, env *testenv.Env, dir, ref string)
		args  []string
		want  string
	}{
		{
			name: "flag wins over everything",
			setup: func(t *testing.T, env *testenv.Env, dir, ref string) {
				setIssueTimezoneMeta(t, env, dir, ref, "America/Chicago")
				t.Setenv("TZ", "Asia/Tokyo")
			},
			args: []string{"--timezone", "Europe/Paris"},
			want: "Europe/Paris",
		},
		{
			name: "issue metadata wins over the caller zone",
			setup: func(t *testing.T, env *testenv.Env, dir, ref string) {
				setIssueTimezoneMeta(t, env, dir, ref, "America/Chicago")
				t.Setenv("TZ", "Asia/Tokyo")
			},
			want: "America/Chicago",
		},
		{
			name:  "TZ environment",
			setup: func(t *testing.T, _ *testenv.Env, _, _ string) { t.Setenv("TZ", ":Asia/Tokyo") },
			want:  "Asia/Tokyo",
		},
		{
			name: "localtime link",
			setup: func(t *testing.T, _ *testenv.Env, _, _ string) {
				link := filepath.Join(t.TempDir(), "localtime")
				require.NoError(t, os.Symlink("/usr/share/zoneinfo/Australia/Sydney", link))
				localtimePath = link
			},
			want: "Australia/Sydney",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateCallerZone(t)
			env, dir, pid := setupCLIWorkspace(t)
			ref := createIssue(t, env, pid, "zone source")
			tc.setup(t, env, dir, ref)

			args := append([]string{"schedule", ref, "2026-11-02", "--repeat", "FREQ=DAILY"}, tc.args...)
			runCLI(t, env, dir, args...)

			recs := listRecurrencesViaHTTP(t, env, pid)
			require.Len(t, recs, 1)
			assert.Equal(t, tc.want, recs[0].Timezone)
		})
	}
}

func setIssueTimezoneMeta(t *testing.T, env *testenv.Env, dir, ref, zone string) {
	t.Helper()
	runCLI(t, env, dir, "meta", "set", ref, "timezone", zone)
}

func TestScheduleRepeatJSONOutput(t *testing.T) {
	isolateCallerZone(t)
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "json output")

	started := runCLI(t, env, dir, "--json", "schedule", ref, "2026-11-02", "--repeat", "FREQ=WEEKLY", "--timezone", "Europe/Paris")
	var start map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(started), &start))
	assert.JSONEq(t, `"started"`, string(start["action"]))
	assert.JSONEq(t, `"2026-11-02"`, string(start["scheduled_on"]))
	assert.JSONEq(t, `"Europe/Paris"`, string(start["timezone"]))
	var startedRec recurrenceView
	require.NoError(t, json.Unmarshal(start["recurrence"], &startedRec))
	assert.Equal(t, "FREQ=WEEKLY", startedRec.RRule)

	changed := runCLI(t, env, dir, "--json", "schedule", ref, "2026-11-09", "--repeat", "FREQ=DAILY", "--timezone", "Asia/Tokyo")
	var change map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(changed), &change))
	assert.JSONEq(t, `"changed"`, string(change["action"]))
	assert.JSONEq(t, `"2026-11-02"`, string(change["scheduled_on"]))
	assert.JSONEq(t, `"Europe/Paris"`, string(change["timezone"]), "the issue's own zone is reported on a change too")
	var changedRec recurrenceView
	require.NoError(t, json.Unmarshal(change["recurrence"], &changedRec))
	assert.Equal(t, "FREQ=DAILY", changedRec.RRule)
	assert.Equal(t, "2026-11-09", changedRec.DTStart)
	assert.Equal(t, "Asia/Tokyo", changedRec.Timezone)
}

func TestScheduleRepeatRequiresAZoneWhenNoneIsKnown(t *testing.T) {
	isolateCallerZone(t)
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "no zone anywhere")

	_, stderr, err := runCLIWithErr(t, env, dir, "schedule", ref, "2026-11-02", "--repeat", "FREQ=DAILY")
	ce := requireCLIError(t, err, ExitValidation)
	assert.Equal(t, kindValidation, ce.Kind)
	assert.Contains(t, stderr, "--timezone")
	assert.Empty(t, listRecurrencesViaHTTP(t, env, pid))
}

func TestScheduleRepeatNeverSendsLocal(t *testing.T) {
	isolateCallerZone(t)
	t.Setenv("TZ", "Local")
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "local zone")

	_, stderr, err := runCLIWithErr(t, env, dir, "schedule", ref, "2026-11-02", "--repeat", "FREQ=DAILY")
	_ = requireCLIError(t, err, ExitValidation)
	assert.Contains(t, stderr, "--timezone")

	_, _, err = runCLIWithErr(t, env, dir, "schedule", ref, "2026-11-02", "--repeat", "FREQ=DAILY", "--timezone", "Local")
	_ = requireCLIError(t, err, ExitValidation)
	assert.Empty(t, listRecurrencesViaHTTP(t, env, pid))
}

func TestScheduleRepeatChangesAnExistingRecurrence(t *testing.T) {
	isolateCallerZone(t)
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "changing")
	runCLI(t, env, dir, "schedule", ref, "2026-11-02", "--repeat", "FREQ=WEEKLY", "--timezone", "Europe/Paris")
	before := listRecurrencesViaHTTP(t, env, pid)
	require.Len(t, before, 1)

	out := runCLI(t, env, dir, "schedule", ref, "2026-11-09", "--repeat", "FREQ=DAILY", "--timezone", "Asia/Tokyo")
	assert.Contains(t, out, "repeat: FREQ=DAILY, start 2026-11-09, timezone Asia/Tokyo")
	assert.Contains(t, out, "scheduled_on: 2026-11-02 (this issue's date did not move)")
	assert.Contains(t, out, "timezone: Europe/Paris (this issue's zone did not change)")

	after := listRecurrencesViaHTTP(t, env, pid)
	require.Len(t, after, 1, "a change must not start a second recurrence")
	assert.Equal(t, before[0].UID, after[0].UID)
	assert.Equal(t, "FREQ=DAILY", after[0].RRule)
	assert.Equal(t, "2026-11-09", after[0].DTStart)
	assert.Equal(t, "Asia/Tokyo", after[0].Timezone)
	assert.Greater(t, after[0].Revision, before[0].Revision)

	// The issue's own timezone metadata is still Paris; a later change that
	// names no zone keeps the recurrence's zone, not the stale one.
	runCLI(t, env, dir, "schedule", ref, "2026-11-16", "--repeat", "FREQ=WEEKLY")
	again := listRecurrencesViaHTTP(t, env, pid)
	require.Len(t, again, 1)
	assert.Equal(t, "FREQ=WEEKLY", again[0].RRule)
	assert.Equal(t, "Asia/Tokyo", again[0].Timezone)
}

// movingProxy forwards to the daemon, but the first PATCH to a recurrence is
// preceded by a competing change, so the revision the CLI read is stale.
func movingProxy(t *testing.T, env *testenv.Env, competing func()) string {
	t.Helper()
	return proxyBefore(t, env, func(r *http.Request) bool {
		return (r.Method == http.MethodPatch || r.Method == http.MethodDelete) && strings.Contains(r.URL.Path, "/recurrences/")
	}, competing)
}

// proxyBefore forwards to the daemon, running competing once before the first
// request that match accepts.
func proxyBefore(t *testing.T, env *testenv.Env, match func(*http.Request) bool, competing func()) string {
	t.Helper()
	var once sync.Once
	return interceptingProxy(t, env, func(_ http.ResponseWriter, r *http.Request) bool {
		if match(r) {
			once.Do(competing)
		}
		return false
	})
}

// interceptingProxy forwards to the daemon unless intercept answers the
// request itself and returns true.
func interceptingProxy(t *testing.T, env *testenv.Env, intercept func(http.ResponseWriter, *http.Request) bool) string {
	t.Helper()
	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	// Rewrite, unlike the default director, adds no X-Forwarded-For header,
	// which the daemon would read as a remote caller.
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(target) }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !intercept(w, r) {
			proxy.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func runCLIAgainst(t *testing.T, baseURL, dir string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	return executeRootCapture(t, contextWithBaseURL(context.Background(), baseURL), append([]string{"--workspace", dir}, args...)...)
}

func patchRecurrenceViaHTTP(t *testing.T, env *testenv.Env, pid int64, rec recurrenceRow, rule string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"actor": "tester", "rrule": rule})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPatch,
		env.URL+"/api/v1/projects/"+itoa(pid)+"/recurrences/"+rec.UID, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", `"rev-`+itoa(rec.Revision)+`"`)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestScheduleRepeatChangeLosingTheRaceWritesNothingAndReportsTheCurrentRule(t *testing.T) {
	isolateCallerZone(t)
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "contended")
	runCLI(t, env, dir, "schedule", ref, "2026-11-02", "--repeat", "FREQ=WEEKLY", "--timezone", "UTC")
	rec := listRecurrencesViaHTTP(t, env, pid)[0]

	base := movingProxy(t, env, func() { patchRecurrenceViaHTTP(t, env, pid, rec, "FREQ=MONTHLY") })
	_, stderr, err := runCLIAgainst(t, base, dir, "schedule", ref, "2026-11-09", "--repeat", "FREQ=DAILY")
	ce := requireCLIError(t, err, ExitConfirm)
	assert.Equal(t, kindConfirm, ce.Kind)
	assert.Contains(t, stderr, "revision conflict")
	assert.Contains(t, stderr, "FREQ=MONTHLY")

	after := listRecurrencesViaHTTP(t, env, pid)
	require.Len(t, after, 1)
	assert.Equal(t, "FREQ=MONTHLY", after[0].RRule, "the losing change must not overwrite the winner")
	assert.Equal(t, "2026-11-02", after[0].DTStart)
}

func TestScheduleRepeatStopEndsTheRecurrenceAndKeepsTheIssue(t *testing.T) {
	isolateCallerZone(t)
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "stopping")
	runCLI(t, env, dir, "schedule", ref, "2026-11-02", "--repeat", "FREQ=WEEKLY", "--timezone", "UTC")

	out := runCLI(t, env, dir, "schedule", ref, "--repeat", "-")
	assert.Contains(t, out, "stopped")
	assert.Empty(t, listRecurrencesViaHTTP(t, env, pid))
	assert.Contains(t, runCLI(t, env, dir, "show", ref), "[open]")
}

func TestScheduleRepeatStopWhenAlreadyStoppedWritesNothing(t *testing.T) {
	isolateCallerZone(t)
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "stopped twice")
	runCLI(t, env, dir, "schedule", ref, "2026-11-02", "--repeat", "FREQ=WEEKLY", "--timezone", "UTC")
	runCLI(t, env, dir, "schedule", ref, "--repeat", "-")

	_, stderr, err := runCLIWithErr(t, env, dir, "schedule", ref, "--repeat", "-")
	ce := requireCLIError(t, err, ExitNotFound)
	assert.Equal(t, kindNotFound, ce.Kind)
	assert.Contains(t, stderr, "no active recurrence")

	plain := createIssue(t, env, pid, "never repeated")
	_, stderr, err = runCLIWithErr(t, env, dir, "schedule", plain, "--repeat", "-")
	_ = requireCLIError(t, err, ExitNotFound)
	assert.Contains(t, stderr, "no active recurrence")
}

func TestScheduleRepeatStopLosingTheRaceWritesNothing(t *testing.T) {
	isolateCallerZone(t)
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "contended stop")
	runCLI(t, env, dir, "schedule", ref, "2026-11-02", "--repeat", "FREQ=WEEKLY", "--timezone", "UTC")
	rec := listRecurrencesViaHTTP(t, env, pid)[0]

	base := movingProxy(t, env, func() { patchRecurrenceViaHTTP(t, env, pid, rec, "FREQ=MONTHLY") })
	_, stderr, err := runCLIAgainst(t, base, dir, "schedule", ref, "--repeat", "-")
	_ = requireCLIError(t, err, ExitConfirm)
	assert.Contains(t, stderr, "FREQ=MONTHLY")
	require.Len(t, listRecurrencesViaHTTP(t, env, pid), 1)
}

func TestScheduleRepeatPrintsTheDaemonsRejectionOfARule(t *testing.T) {
	isolateCallerZone(t)
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "bad rule")

	_, stderr, err := runCLIWithErr(t, env, dir, "schedule", ref, "2026-11-02", "--repeat", "every other tuesday", "--timezone", "UTC")
	ce := requireCLIError(t, err, ExitValidation)
	assert.Equal(t, kindValidation, ce.Kind)
	assert.Contains(t, stderr, "every other tuesday", "the daemon's message, which quotes the rule, is printed as is")
	assert.Empty(t, listRecurrencesViaHTTP(t, env, pid))
	issue := fetchMetaIssueViaHTTP(t, env, pid, ref)
	require.JSONEq(t, `{}`, string(issue.Issue.Metadata))
}

func TestScheduleRepeatRefusesTheParentOfACronJob(t *testing.T) {
	isolateCallerZone(t)
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "cron parent")
	_, err := env.DB.ExecContext(t.Context(),
		`UPDATE issues SET metadata='{"cron.progress.01ARZ3NDEKTSV4RRFFQ69G5FAV":{}}' WHERE short_id=?`, ref)
	require.NoError(t, err)

	for _, args := range [][]string{
		{"schedule", ref, "2026-11-02", "--repeat", "FREQ=DAILY", "--timezone", "UTC"},
		{"schedule", ref, "--repeat", "-"},
	} {
		_, stderr, err := runCLIWithErr(t, env, dir, args...)
		ce := requireCLIError(t, err, ExitValidation)
		assert.Equal(t, kindValidation, ce.Kind)
		assert.Contains(t, stderr, "kata cron job create --parent")
	}
	assert.Empty(t, listRecurrencesViaHTTP(t, env, pid))
}

func TestScheduleRepeatHonorsIfMatch(t *testing.T) {
	isolateCallerZone(t)
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "conditional repeat")

	_, stderr, err := runCLIWithErr(t, env, dir, "schedule", "--if-match", "rev-99", ref, "2026-11-02", "--repeat", "FREQ=DAILY", "--timezone", "UTC")
	ce := requireCLIError(t, err, ExitConfirm)
	assert.Equal(t, kindConfirm, ce.Kind)
	assert.Contains(t, stderr, "revision conflict")
	assert.Empty(t, listRecurrencesViaHTTP(t, env, pid))

	runCLI(t, env, dir, "schedule", "--if-match", "1", ref, "2026-11-02", "--repeat", "FREQ=DAILY", "--timezone", "UTC")
	require.Len(t, listRecurrencesViaHTTP(t, env, pid), 1)

	_, _, err = runCLIWithErr(t, env, dir, "schedule", "--if-match", "1", ref, "--repeat", "-")
	_ = requireCLIError(t, err, ExitConfirm)
	require.Len(t, listRecurrencesViaHTTP(t, env, pid), 1, "a stale --if-match must not stop the recurrence")
}

func TestScheduleRepeatUsageErrors(t *testing.T) {
	isolateCallerZone(t)
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "usage")

	_, _, err := runCLIWithErr(t, env, dir, "schedule", ref, "--repeat", "FREQ=DAILY", "--timezone", "UTC")
	_ = requireCLIError(t, err, ExitUsage)
	_, _, err = runCLIWithErr(t, env, dir, "schedule", ref, "2026-11-02", "--repeat", "-")
	_ = requireCLIError(t, err, ExitUsage)
	_, _, err = runCLIWithErr(t, env, dir, "schedule", ref, "--repeat", "-", "--timezone", "UTC")
	_ = requireCLIError(t, err, ExitUsage)
	_, _, err = runCLIWithErr(t, env, dir, "schedule", ref, "2026-11-02", "--timezone", "UTC")
	_ = requireCLIError(t, err, ExitUsage)
	_, _, err = runCLIWithErr(t, env, dir, "schedule", ref)
	_ = requireCLIError(t, err, ExitUsage)
	assert.Empty(t, listRecurrencesViaHTTP(t, env, pid))
}

func TestShowPrintsTheRecurrenceAndJSONCarriesIt(t *testing.T) {
	isolateCallerZone(t)
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "weekly review")
	runCLI(t, env, dir, "schedule", ref, "2026-11-02", "--repeat", "FREQ=WEEKLY", "--timezone", "Europe/Paris")

	text := runCLI(t, env, dir, "show", ref)
	assert.Contains(t, text, "repeat: FREQ=WEEKLY, start 2026-11-02, timezone Europe/Paris, next 2026-11-09")

	raw := runCLI(t, env, dir, "--json", "show", ref)
	var body struct {
		Recurrence recurrenceView `json:"recurrence"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &body))
	assert.NotEmpty(t, body.Recurrence.UID)
	assert.Equal(t, "FREQ=WEEKLY", body.Recurrence.RRule)
	assert.Equal(t, "2026-11-02", body.Recurrence.DTStart)
	assert.Equal(t, "Europe/Paris", body.Recurrence.Timezone)
	require.NotNil(t, body.Recurrence.NextOccurrenceKey)
	assert.Equal(t, "2026-11-09", *body.Recurrence.NextOccurrenceKey)
	assert.Positive(t, body.Recurrence.Revision)

	plain := createIssue(t, env, pid, "one-off")
	assert.NotContains(t, runCLI(t, env, dir, "show", plain), "repeat:")
	assert.NotContains(t, runCLI(t, env, dir, "--json", "show", plain), `"recurrence"`)
}

func TestScheduleRepeatAgentOutputStartsWithOK(t *testing.T) {
	isolateCallerZone(t)
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "agent output")

	start := runCLI(t, env, dir, "--agent", "schedule", ref, "2026-11-02", "--repeat", "FREQ=WEEKLY", "--timezone", "UTC")
	change := runCLI(t, env, dir, "--agent", "schedule", ref, "2026-11-09", "--repeat", "FREQ=DAILY")
	stop := runCLI(t, env, dir, "--agent", "schedule", ref, "--repeat", "-")

	for action, out := range map[string]string{"started": start, "changed": change, "stopped": stop} {
		lines := strings.Split(strings.TrimSpace(out), "\n")
		require.Len(t, lines, 1, action)
		assert.Contains(t, lines[0], "OK schedule ref="+ref, action)
		assert.Contains(t, lines[0], "repeat="+action, action)
	}
	assert.Contains(t, start, "rrule=FREQ=WEEKLY")
}

func TestScheduleRepeatThroughAMovedIssuesOldRefUsesItsCurrentProject(t *testing.T) {
	isolateCallerZone(t)
	env, dir, source, target, issue := setupMoveCLIProjects(t)
	runCLI(t, env, dir, "--project", source.Name, "move", issue.ShortID, target.Name)
	oldRef := source.Name + "#" + issue.ShortID

	runCLI(t, env, dir, "schedule", oldRef, "2026-11-02", "--repeat", "FREQ=WEEKLY", "--timezone", "UTC")
	require.Empty(t, listRecurrencesViaHTTP(t, env, source.ID), "the old project must not gain a recurrence")
	recs := listRecurrencesViaHTTP(t, env, target.ID)
	require.Len(t, recs, 1)
	assert.Equal(t, "FREQ=WEEKLY", recs[0].RRule)

	shown := runCLI(t, env, dir, "show", oldRef)
	assert.Contains(t, shown, "FREQ=WEEKLY", "show through the old ref must print the recurrence")

	runCLI(t, env, dir, "schedule", oldRef, "2026-11-09", "--repeat", "FREQ=DAILY")
	recs = listRecurrencesViaHTTP(t, env, target.ID)
	require.Len(t, recs, 1, "a change must not start a second recurrence")
	assert.Equal(t, "FREQ=DAILY", recs[0].RRule)

	runCLI(t, env, dir, "schedule", oldRef, "--repeat", "-")
	assert.Empty(t, listRecurrencesViaHTTP(t, env, target.ID))
}

func TestScheduleRepeatIfMatchLosesToACompetingWriteBeforeTheRecurrenceIsCreated(t *testing.T) {
	isolateCallerZone(t)
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "contended start")
	proxyURL := proxyBefore(t, env, func(r *http.Request) bool {
		return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/recurrences")
	}, func() { runCLI(t, env, dir, "meta", "set", ref, "note", "competing") })

	_, stderr, err := runCLIAgainst(t, proxyURL, dir, "schedule", "--if-match", "1", ref, "2026-11-02", "--repeat", "FREQ=DAILY", "--timezone", "UTC")

	ce := requireCLIError(t, err, ExitConfirm)
	assert.Equal(t, kindConfirm, ce.Kind)
	assert.Contains(t, stderr, "revision conflict")
	assert.Empty(t, listRecurrencesViaHTTP(t, env, pid), "the competing write must not be overwritten")
}

func TestScheduleRepeatReportsAMovedIssueWithItsCurrentProjectRef(t *testing.T) {
	isolateCallerZone(t)
	env, dir, source, target, issue := setupMoveCLIProjects(t)
	runCLI(t, env, dir, "--project", source.Name, "move", issue.ShortID, target.Name)
	moved, err := env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)

	out := runCLI(t, env, dir, "schedule", source.Name+"#"+issue.ShortID, "2026-11-02", "--repeat", "FREQ=DAILY", "--timezone", "UTC")

	assert.True(t, strings.HasPrefix(out, target.Name+"#"+moved.ShortID+" repeat started"), out)
}

// olderDaemonProxy forwards to the daemon but reports an API version that
// predates the If-Match guard on starting a recurrence.
func olderDaemonProxy(t *testing.T, env *testenv.Env) string {
	t.Helper()
	return interceptingProxy(t, env, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/v1/health" {
			return false
		}
		_, _ = w.Write([]byte(`{"ok":true,"api_schema_version":"0.36.0"}`))
		return true
	})
}

func TestScheduleRepeatIfMatchOnAnOlderDaemonOnlyBlocksStarting(t *testing.T) {
	isolateCallerZone(t)
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "older daemon")
	older := olderDaemonProxy(t, env)

	_, stderr, err := runCLIAgainst(t, older, dir, "schedule", "--if-match", "1", ref, "2026-11-02", "--repeat", "FREQ=DAILY", "--timezone", "UTC")
	require.Error(t, err, "starting needs the daemon guard")
	assert.Contains(t, stderr, "--if-match when starting a recurrence requires daemon API 0.37.0")
	assert.Empty(t, listRecurrencesViaHTTP(t, env, pid))

	runCLI(t, env, dir, "schedule", "--if-match", "1", ref, "2026-11-02", "--repeat", "FREQ=DAILY", "--timezone", "UTC")
	rev := listRecurrencesViaHTTP(t, env, pid)
	require.Len(t, rev, 1)

	_, _, err = runCLIAgainst(t, older, dir, "schedule", "--if-match", "2", ref, "2026-11-09", "--repeat", "FREQ=WEEKLY")
	require.NoError(t, err, "a change uses the recurrence PATCH guard that older daemons have")
	assert.Equal(t, "FREQ=WEEKLY", listRecurrencesViaHTTP(t, env, pid)[0].RRule)
}
