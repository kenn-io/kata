package daemon_test

import (
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

type previewOccurrence struct {
	At    time.Time `json:"at"`
	Local string    `json:"local"`
}
type previewResult struct {
	JobUID             string              `json:"job_uid"`
	ScheduledBy        string              `json:"scheduled_by"`
	Timezone           string              `json:"timezone"`
	ConfiguredTimezone string              `json:"configured_timezone"`
	From               time.Time           `json:"from"`
	Anchor             *time.Time          `json:"anchor"`
	AnchorSource       string              `json:"anchor_source"`
	Count              int                 `json:"count"`
	Occurrences        []previewOccurrence `json:"occurrences"`
	Exhausted          bool                `json:"exhausted"`
	Error              string              `json:"error"`
	Active             *bool               `json:"active"`
	InactiveReason     string              `json:"inactive_reason"`
	Note               string              `json:"note"`
	NextSweep          *struct {
		Now     time.Time           `json:"now"`
		Anchor  *time.Time          `json:"anchor"`
		Through *time.Time          `json:"through"`
		Due     []previewOccurrence `json:"due"`
		More    bool                `json:"more"`
		Error   string              `json:"error"`
	} `json:"next_sweep"`
}

func previewTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, e := time.Parse(time.RFC3339, s)
	require.NoError(t, e)
	return v.UTC()
}
func previewNative(parent string) cron.JobDefinition {
	return cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 3600}, Action: cron.Action{Kind: "notify", Recipient: "current-owner-or-author"}, Issue: &cron.IssuePolicy{Kind: "per-run", ParentUID: parent, Title: "Review"}, Overlap: "allow", Catchup: "all"}
}
func previewDecode(t *testing.T, raw []byte) previewResult {
	t.Helper()
	var out struct {
		Preview previewResult `json:"preview"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	return out.Preview
}
func TestCronPreviewUnsavedShowsZoneOffsets(t *testing.T) {
	env := testenv.New(t)
	p := previewProject(t, env, "preview-project")
	parent := previewParent(t, env, p.ID)
	j := previewNative(parent.UID)
	j.Trigger = cron.Trigger{Kind: "cron", Cron: "0 12 * * *", Timezone: "America/New_York"}
	before, e := env.DB.MaxEventID(t.Context())
	require.NoError(t, e)
	sub := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: p.ID})
	defer sub.Unsub()
	status, raw := previewHTTP(t, env, "POST", fmt.Sprintf("/api/v1/projects/%d/cron/preview", p.ID), map[string]any{"definition": j, "count": 4, "from": "2026-03-06T00:00:00Z"})
	require.Equal(t, 200, status, string(raw))
	out := previewDecode(t, raw)
	require.Equal(t, 4, out.Count)
	require.Equal(t, "kata", out.ScheduledBy)
	require.Equal(t, "America/New_York", out.Timezone)
	require.Equal(t, []previewOccurrence{
		{previewTime(t, "2026-03-06T17:00:00Z"), "2026-03-06T12:00:00-05:00"},
		{previewTime(t, "2026-03-07T17:00:00Z"), "2026-03-07T12:00:00-05:00"},
		{previewTime(t, "2026-03-08T16:00:00Z"), "2026-03-08T12:00:00-04:00"},
		{previewTime(t, "2026-03-09T16:00:00Z"), "2026-03-09T12:00:00-04:00"},
	}, out.Occurrences)
	after, e := env.DB.MaxEventID(t.Context())
	require.NoError(t, e)
	require.Equal(t, before, after)
	children, e := env.DB.ChildrenOfIssue(t.Context(), parent.ID)
	require.NoError(t, e)
	require.Empty(t, children)
	select {
	case <-sub.Ch:
		t.Fatal("preview published an event")
	default:
	}
}
func TestCronPreviewUnsavedRejectsLikeCreate(t *testing.T) {
	env := testenv.New(t)
	p := previewProject(t, env, "preview-project")
	j := previewNative(previewUID(t))
	j.Trigger.IntervalSeconds = 30
	status, raw := previewHTTP(t, env, "POST", fmt.Sprintf("/api/v1/projects/%d/cron/preview", p.ID), map[string]any{"definition": j})
	require.Equal(t, 400, status, string(raw))
	require.Contains(t, string(raw), "native recurring intervals must be at least 60 seconds")
}
func TestCronPreviewAdapterTriggerOnly(t *testing.T) {
	env := testenv.New(t)
	p := previewProject(t, env, "preview-project")
	base := fmt.Sprintf("/api/v1/projects/%d/cron", p.ID)
	for _, trigger := range []cron.Trigger{{Kind: "cron", Cron: "0 9 * * *"}, {Kind: "interval", IntervalSeconds: 3600}, {Kind: "once", At: "2026-01-01T00:00:00Z"}} {
		j := previewAdapter(t)
		j.Trigger = trigger
		job, _, e := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Adapter", Actor: "worker", Definition: j})
		require.NoError(t, e)
		for _, stored := range []bool{false, true} {
			method, path, body := "POST", base+"/preview", any(map[string]any{"definition": j, "from": "2026-03-06T00:00:00Z", "count": 2})
			if stored {
				method, path, body = "GET", base+"/jobs/"+job.UID+"/preview?from=2026-03-06T00:00:00Z&count=2", nil
			}
			status, raw := previewHTTP(t, env, method, path, body)
			require.Equal(t, 200, status, string(raw))
			out := previewDecode(t, raw)
			require.Equal(t, "adapter", out.ScheduledBy)
			require.NotEmpty(t, out.Note)
			require.Nil(t, out.Active)
			require.Nil(t, out.NextSweep)
			switch trigger.Kind {
			case "interval":
				require.Equal(t, "from", out.AnchorSource)
				require.Equal(t, previewTime(t, "2026-03-06T01:00:00Z"), out.Occurrences[0].At)
			case "once":
				require.True(t, out.Exhausted)
				require.Len(t, out.Occurrences, 1)
				require.Equal(t, previewTime(t, trigger.At), out.Occurrences[0].At)
			case "cron":
				require.Equal(t, previewTime(t, "2026-03-06T09:00:00Z"), out.Occurrences[0].At)
			}
		}
	}
}
func TestCronPreviewRefusesUnschedulableTriggers(t *testing.T) {
	env := testenv.New(t)
	p := previewProject(t, env, "preview-project")
	for _, kind := range []string{"manual", "issue-scheduled", "issue-deadline"} {
		j := previewAdapter(t)
		j.Trigger = cron.Trigger{Kind: kind}
		for _, stored := range []bool{false, true} {
			method, path, body := "POST", fmt.Sprintf("/api/v1/projects/%d/cron/preview", p.ID), any(map[string]any{"definition": j})
			if stored {
				// Store the structurally valid document, as import and restore may do.
				original := previewAdapter(t)
				job, _, e := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Adapter", Actor: "worker", Definition: original})
				require.NoError(t, e)
				b, e := json.Marshal(j)
				require.NoError(t, e)
				_, e = env.DB.ExecContext(t.Context(), "UPDATE cron_jobs SET definition_json=$1 WHERE uid=$2", string(b), job.UID)
				require.NoError(t, e)
				method, path, body = "GET", fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/preview", p.ID, job.UID), nil
			}
			status, raw := previewHTTP(t, env, method, path, body)
			require.Equal(t, 400, status, string(raw))
			require.Contains(t, string(raw), "preview supports cron, interval and once triggers; this job uses "+kind)
		}
	}
}
func TestCronPreviewBounds(t *testing.T) {
	env := testenv.New(t)
	p := previewProject(t, env, "preview-project")
	j := previewAdapter(t)
	for _, opts := range []map[string]any{{"count": 0}, {"count": 101}, {"from": "bad"}, {"from": "0001-01-01T00:00:00Z"}, {"from": "10000-01-01T00:00:00Z"}, {"from": "9999-12-31T23:00:00-02:00"}} {
		opts["definition"] = j
		status, raw := previewHTTP(t, env, "POST", fmt.Sprintf("/api/v1/projects/%d/cron/preview", p.ID), opts)
		require.Equal(t, 400, status, string(raw))
		require.Contains(t, string(raw), "validation")
	}
}
func TestCronPreviewUnsavedMissingParent(t *testing.T) {
	env := testenv.New(t)
	p := previewProject(t, env, "preview-project")
	status, raw := previewHTTP(t, env, "POST", fmt.Sprintf("/api/v1/projects/%d/cron/preview", p.ID), map[string]any{"definition": previewNative(previewUID(t))})
	require.Equal(t, 200, status, string(raw))
	out := previewDecode(t, raw)
	require.NotNil(t, out.Active)
	require.False(t, *out.Active)
	require.Equal(t, "parent_missing", out.InactiveReason)
}

// Each stored-state case runs against both backends. One testcontainer is reused
// across a case's subtests, keeping this read-only feature cheap to verify.
type previewFixture struct {
	store   db.Storage
	sql     *sql.DB
	url     string
	project db.Project
	parent  db.Issue
	job     db.CronJob
	now     time.Time
}

func previewBackends(t *testing.T, run func(*testing.T, *previewFixture)) {
	t.Helper()
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			f := &previewFixture{now: previewTime(t, "2026-03-06T12:00:00Z")}
			if backend == "sqlite" {
				env := testenv.New(t)
				f.store, f.sql = env.DB, env.DB.DB
			} else {
				if testing.Short() {
					t.Skip("requires postgres testcontainer")
				}
				dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
				t.Cleanup(cleanup)
				s, e := pgstore.Open(t.Context(), dsn)
				require.NoError(t, e)
				t.Cleanup(func() { _ = s.Close() })
				f.store, f.sql = s, s.DB
			}
			var e error
			f.project, e = f.store.CreateProject(t.Context(), "preview-project")
			require.NoError(t, e)
			f.parent, _, e = f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.project.ID, Title: "Parent", Author: "worker"})
			require.NoError(t, e)
			f.job, _, e = f.store.PutCronJob(t.Context(), db.PutCronJob{ProjectID: f.project.ID, Name: "Recurring", Actor: "worker", Definition: previewNative(f.parent.UID)})
			require.NoError(t, e)
			srv := daemon.NewServer(daemon.ServerConfig{DB: f.store, CronClock: func() time.Time { return f.now }})
			t.Cleanup(func() { _ = srv.Close() })
			httpSrv := httptest.NewServer(srv.Handler())
			t.Cleanup(httpSrv.Close)
			f.url = httpSrv.URL
			run(t, f)
		})
	}
}
func (f *previewFixture) progress(t *testing.T, p cron.RecurringProgress) {
	t.Helper()
	raw, e := json.Marshal(map[string]any{"cron.progress." + f.job.UID: p})
	require.NoError(t, e)
	_, e = f.sql.ExecContext(t.Context(), "UPDATE issues SET metadata=$1 WHERE uid=$2", string(raw), f.parent.UID)
	require.NoError(t, e)
}
func (f *previewFixture) definition(t *testing.T, j cron.JobDefinition) {
	t.Helper()
	raw, e := json.Marshal(j)
	require.NoError(t, e)
	_, e = f.sql.ExecContext(t.Context(), "UPDATE cron_jobs SET definition_json=$1 WHERE uid=$2", string(raw), f.job.UID)
	require.NoError(t, e)
	f.job.Definition = j
}
func (f *previewFixture) get(t *testing.T, query string) previewResult {
	t.Helper()
	r, e := http.NewRequestWithContext(t.Context(), "GET", fmt.Sprintf("%s/api/v1/projects/%d/cron/jobs/%s/preview%s", f.url, f.project.ID, f.job.UID, query), nil)
	require.NoError(t, e)
	resp, e := http.DefaultClient.Do(r)
	require.NoError(t, e)
	defer func() { _ = resp.Body.Close() }()
	raw, e := io.ReadAll(resp.Body)
	require.NoError(t, e)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	return previewDecode(t, raw)
}
func (f *previewFixture) unchanged(t *testing.T, action func()) {
	t.Helper()
	parent, e := f.store.IssueByUID(t.Context(), f.parent.UID, db.IncludeDeletedYes)
	require.NoError(t, e)
	job, e := f.store.CronJob(t.Context(), f.project.ID, f.job.UID)
	require.NoError(t, e)
	events, e := f.store.MaxEventID(t.Context())
	require.NoError(t, e)
	children, e := f.store.ChildrenOfIssue(t.Context(), f.parent.ID)
	require.NoError(t, e)
	action()
	after, e := f.store.IssueByUID(t.Context(), f.parent.UID, db.IncludeDeletedYes)
	require.NoError(t, e)
	require.Equal(t, parent, after)
	afterJob, e := f.store.CronJob(t.Context(), f.project.ID, f.job.UID)
	require.NoError(t, e)
	require.Equal(t, job, afterJob)
	afterEvents, e := f.store.MaxEventID(t.Context())
	require.NoError(t, e)
	require.Equal(t, events, afterEvents)
	afterChildren, e := f.store.ChildrenOfIssue(t.Context(), f.parent.ID)
	require.NoError(t, e)
	require.Equal(t, children, afterChildren)
}
func TestCronPreviewStoredIntervalUsesBindingAnchor(t *testing.T) {
	previewBackends(t, func(t *testing.T, f *previewFixture) {
		parent, e := f.store.IssueByUID(t.Context(), f.parent.UID, db.IncludeDeletedYes)
		require.NoError(t, e)
		var meta map[string]jsontext.Value
		require.NoError(t, json.Unmarshal([]byte(parent.Metadata), &meta))
		var p cron.RecurringProgress
		require.NoError(t, json.Unmarshal(meta["cron.progress."+f.job.UID], &p))
		from := p.Anchor.Add(90 * time.Minute)
		f.unchanged(t, func() {
			out := f.get(t, "?count=2&from="+from.Format(time.RFC3339Nano))
			require.Equal(t, "binding", out.AnchorSource)
			require.Equal(t, []previewOccurrence{{p.Anchor.Add(2 * time.Hour), p.Anchor.Add(2 * time.Hour).Format(time.RFC3339)}, {p.Anchor.Add(3 * time.Hour), p.Anchor.Add(3 * time.Hour).Format(time.RFC3339)}}, out.Occurrences)
		})
	})
}
func TestCronPreviewStoredDisabledShowsBacklog(t *testing.T) {
	previewBackends(t, func(t *testing.T, f *previewFixture) {
		j := f.job.Definition
		j.Enabled = false
		f.definition(t, j)
		anchor := f.now.Add(-3*time.Hour - time.Minute)
		f.progress(t, cron.RecurringProgress{Version: 1, Anchor: anchor, Through: anchor})
		f.unchanged(t, func() {
			out := f.get(t, "")
			require.False(t, *out.Active)
			require.Equal(t, "disabled", out.InactiveReason)
			require.Len(t, out.NextSweep.Due, 3)
			require.Equal(t, anchor, *out.NextSweep.Anchor)
			require.Equal(t, anchor, *out.NextSweep.Through)
		})
	})
}
func TestCronPreviewStoredReportsPlannerError(t *testing.T) {
	previewBackends(t, func(t *testing.T, f *previewFixture) {
		j := f.job.Definition
		j.Trigger = cron.Trigger{Kind: "cron", Cron: "0 0 30 2 *"}
		f.definition(t, j)
		a := f.now.Add(-24 * time.Hour)
		f.progress(t, cron.RecurringProgress{Version: 1, Anchor: a, Through: a})
		out := f.get(t, "")
		require.NotEmpty(t, out.Error)
		require.Equal(t, "UTC", out.Timezone)
		require.Equal(t, out.Error, out.NextSweep.Error)
		require.Empty(t, out.Occurrences)
		sweep := daemon.NewDueNotificationSweeper(f.store, daemon.NewEventPublisher(daemon.NewEventBroadcaster(), nil), "UTC")
		require.ErrorContains(t, sweep.RunOnce(t.Context(), f.now), out.Error)
	})
}
func TestCronPreviewStoredParentClosed(t *testing.T) {
	previewBackends(t, func(t *testing.T, f *previewFixture) {
		_, e := f.sql.ExecContext(t.Context(), "UPDATE issues SET status='closed' WHERE uid=$1", f.parent.UID)
		require.NoError(t, e)
		out := f.get(t, "")
		require.False(t, *out.Active)
		require.Equal(t, "parent_not_open", out.InactiveReason)
	})
}
func TestCronPreviewPlannerBranches(t *testing.T) {
	previewBackends(t, func(t *testing.T, f *previewFixture) {
		for _, name := range []string{"retained latest", "disabled latest", "latest interval", "grace edge", "outside grace", "all batch"} {
			t.Run(name, func(t *testing.T) {
				j := previewNative(f.parent.UID)
				a := f.now.Add(-100 * time.Hour)
				p := cron.RecurringProgress{Version: 1, Anchor: a, Through: a}
				switch name {
				case "retained latest", "disabled latest":
					j.Trigger = cron.Trigger{Kind: "cron", Cron: "* * * * *"}
					j.Catchup = "latest"
					p.Through = f.now.Add(-30 * time.Minute)
					p.Scan = &cron.RecurringScan{DefinitionEventUID: f.job.DefinitionEventUID, Until: f.now.Add(time.Hour), Cursor: f.now.Add(-15 * time.Minute)}
					j.Enabled = name != "disabled latest"
				case "latest interval":
					j.Catchup = "latest"
				case "grace edge", "outside grace":
					j.Catchup = "skip"
					j.GraceSeconds = 60
					p.Anchor = f.now.Add(-time.Hour - time.Minute)
					p.Through = p.Anchor
					if name == "outside grace" {
						p.Anchor = p.Anchor.Add(-time.Second)
						p.Through = p.Anchor
					}
				}
				f.definition(t, j)
				f.progress(t, p)
				hypothetical := j
				hypothetical.Enabled = true
				revision := f.job.DefinitionEventUID
				if !j.Enabled {
					revision = previewUID(t)
				}
				expected, e := cron.PlanRecurring(hypothetical, revision, p, f.now)
				require.NoError(t, e)
				f.unchanged(t, func() {
					out := f.get(t, "?count=3")
					require.Empty(t, out.Error)
					require.Equal(t, expected.More, out.NextSweep.More)
					require.Len(t, out.NextSweep.Due, len(expected.Due))
					for i, v := range expected.Due {
						require.Equal(t, v, out.NextSweep.Due[i].At)
					}
					if name == "retained latest" {
						require.True(t, out.NextSweep.More)
						require.Empty(t, out.NextSweep.Due)
					}
					if name == "disabled latest" {
						require.False(t, out.NextSweep.More)
						require.Len(t, out.NextSweep.Due, 1)
					}
					if name == "all batch" {
						require.Len(t, out.NextSweep.Due, 50)
						require.True(t, out.NextSweep.More)
					}
					if name == "grace edge" {
						require.Len(t, out.NextSweep.Due, 1)
					}
					if name == "outside grace" {
						require.Empty(t, out.NextSweep.Due)
					}
				})
			})
		}
	})
}
func TestCronPreviewStoredLegacyJobs(t *testing.T) {
	previewBackends(t, func(t *testing.T, f *previewFixture) {
		for _, tr := range []cron.Trigger{{Kind: "cron", Cron: "0 9 1-7 * 2"}, {Kind: "cron", Cron: "* 9 * * *"}, {Kind: "cron", Cron: "0 9 * * *", Timezone: "Local"}} {
			for _, enabled := range []bool{true, false} {
				j := previewNative(f.parent.UID)
				j.Trigger = tr
				j.Enabled = enabled
				f.definition(t, j)
				a := f.now.Add(-3 * 24 * time.Hour)
				p := cron.RecurringProgress{Version: 1, Anchor: a, Through: a}
				f.progress(t, p)
				hypothetical := j
				hypothetical.Enabled = true
				batch, e := cron.PlanRecurring(hypothetical, f.job.DefinitionEventUID, p, f.now)
				require.NoError(t, e)
				out := f.get(t, "?count=2&from="+a.Format(time.RFC3339))
				require.Empty(t, out.Error)
				require.GreaterOrEqual(t, len(batch.Due), 2)
				require.Equal(t, batch.Due[0], out.Occurrences[0].At)
				require.Equal(t, batch.Due[1], out.Occurrences[1].At)
				if tr.Timezone == "Local" {
					require.Equal(t, "UTC", out.Timezone)
					require.Equal(t, "Local", out.ConfiguredTimezone)
				}
			}
		}
	})
}
func TestCronPreviewStoredUnsafeDefinitions(t *testing.T) {
	previewBackends(t, func(t *testing.T, f *previewFixture) {
		for _, tr := range []cron.Trigger{{Kind: "interval"}, {Kind: "interval", IntervalSeconds: -1}, {Kind: "interval", IntervalSeconds: 9223372037}, {Kind: "cron", Cron: "0 9 * * *", Timezone: "Not/AZone"}, {Kind: "once", At: "bad"}} {
			j := previewNative(f.parent.UID)
			j.Trigger = tr
			if tr.Kind == "once" {
				j = previewAdapter(t)
				j.Trigger = tr
			}
			f.definition(t, j)
			out := f.get(t, "")
			require.NotEmpty(t, out.Error)
			require.Empty(t, out.Occurrences)
			if tr.Kind == "once" {
				require.Contains(t, out.Error, "once requires an RFC3339 instant")
			}
		}
	})
}
func TestCronPreviewStoredBadProgress(t *testing.T) {
	previewBackends(t, func(t *testing.T, f *previewFixture) {
		for _, meta := range []string{`{}`, `{"cron.progress.` + f.job.UID + `":{"version":4}}`, `{"cron.progress.` + f.job.UID + `":"broken"}`} {
			_, e := f.sql.ExecContext(t.Context(), "UPDATE issues SET metadata=$1 WHERE uid=$2", meta, f.parent.UID)
			require.NoError(t, e)
			out := f.get(t, "")
			require.NotEmpty(t, out.Error)
			require.Equal(t, out.Error, out.NextSweep.Error)
			require.Nil(t, out.NextSweep.Anchor)
			require.Nil(t, out.NextSweep.Through)
			require.Nil(t, out.NextSweep.Due)
		}
	})
}
func TestCronPreviewAuthorization(t *testing.T) {
	for _, mode := range []string{"ordinary", "readonly", "scoped"} {
		t.Run(mode, func(t *testing.T) {
			opts := []testenv.Option{testenv.WithAuthToken("tok")}
			if mode == "scoped" {
				opts = append(opts, testenv.WithRequireTokenIdentity())
			}
			if mode == "readonly" {
				opts = []testenv.Option{testenv.WithInsecureReadonly()}
			}
			env := testenv.New(t, opts...)
			p := previewProject(t, env, "preview-project")
			parent := previewParent(t, env, p.ID)
			j := previewNative(parent.UID)
			job, _, e := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Review", Actor: "worker", Definition: j})
			require.NoError(t, e)
			token := "tok"
			if mode == "readonly" {
				token = ""
			}
			if mode == "scoped" {
				expires := time.Now().UTC().Add(time.Hour)
				_, _, e = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "scoped-token", Actor: "worker", AdminActor: db.BootstrapActor, ExpiresAt: &expires, Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: p.UID, RootIssueUID: parent.UID}})
				require.NoError(t, e)
				token = "scoped-token"
			}
			for _, method := range []string{"GET", "POST"} {
				path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/preview", p.ID, job.UID)
				var body any
				if method == "POST" {
					path = fmt.Sprintf("/api/v1/projects/%d/cron/preview", p.ID)
					body = map[string]any{"definition": j}
				}
				headers := map[string]string{}
				if token != "" {
					headers["Authorization"] = "Bearer " + token
				}
				resp, raw := previewRequest(t, env, method, path, body, headers)
				want := 200
				if mode == "scoped" {
					want = 403
				}
				if mode == "readonly" && method == "POST" {
					want = 401
				}
				require.Equal(t, want, resp.StatusCode, string(raw))
				if mode == "scoped" {
					require.Contains(t, string(raw), "scoped_operation_forbidden")
				}
			}
			other := previewProject(t, env, "other-project")
			resp, raw := previewRequest(t, env, "GET", fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/preview", other.ID, job.UID), nil, map[string]string{"Authorization": "Bearer tok"})
			require.Equal(t, 404, resp.StatusCode, string(raw))
		})
	}
}

func previewParent(t *testing.T, env *testenv.Env, id int64) db.Issue {
	t.Helper()
	p, _, e := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: id, Title: "Parent", Author: "worker"})
	require.NoError(t, e)
	return p
}

func TestCronPreviewStoredEligibilityOrder(t *testing.T) {
	previewBackends(t, func(t *testing.T, f *previewFixture) {
		cases := []struct {
			name, reason string
			mutate       func()
		}{
			{"disabled", "disabled", func() { j := f.job.Definition; j.Enabled = false; f.definition(t, j) }},
			{"deleted job", "job_deleted", func() {
				_, e := f.sql.ExecContext(t.Context(), "UPDATE cron_jobs SET deleted_at=$1 WHERE uid=$2", f.now.Format(time.RFC3339), f.job.UID)
				require.NoError(t, e)
			}},
			{"foreign parent", "parent_missing", func() {
				other, e := f.store.CreateProject(t.Context(), "foreign-project")
				require.NoError(t, e)
				p, _, e := f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: other.ID, Title: "Other parent", Author: "worker"})
				require.NoError(t, e)
				j := f.job.Definition
				j.Issue.ParentUID = p.UID
				f.definition(t, j)
			}},
			{"deleted parent", "parent_deleted", func() {
				_, e := f.sql.ExecContext(t.Context(), "UPDATE issues SET deleted_at=$1 WHERE uid=$2", f.now.Format(time.RFC3339), f.parent.UID)
				require.NoError(t, e)
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, e := f.sql.ExecContext(t.Context(), "UPDATE cron_jobs SET deleted_at=NULL WHERE uid=$1", f.job.UID)
				require.NoError(t, e)
				_, e = f.sql.ExecContext(t.Context(), "UPDATE issues SET deleted_at=NULL WHERE uid=$1", f.parent.UID)
				require.NoError(t, e)
				f.definition(t, previewNative(f.parent.UID))
				tc.mutate()
				out := f.get(t, "")
				require.False(t, *out.Active)
				require.Equal(t, tc.reason, out.InactiveReason)
			})
		}
	})
}

func previewProject(t *testing.T, env *testenv.Env, name string) db.Project {
	t.Helper()
	p, e := env.DB.CreateProject(t.Context(), name)
	require.NoError(t, e)
	return p
}
func previewAdapter(_ *testing.T) cron.JobDefinition {
	return cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 60}, Action: cron.Action{Kind: "execute", Prompt: "Inspect"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Inspection result"}, Overlap: "forbid", Catchup: "all"}
}
func previewUID(t *testing.T) string { t.Helper(); v, e := uid.New(); require.NoError(t, e); return v }
func previewRequest(t *testing.T, env *testenv.Env, method, path string, body any, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	var raw []byte
	var e error
	if body != nil {
		raw, e = json.Marshal(body)
		require.NoError(t, e)
	}
	req, e := http.NewRequestWithContext(t.Context(), method, env.URL+path, strings.NewReader(string(raw)))
	require.NoError(t, e)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, e := env.HTTP.Do(req)
	require.NoError(t, e)
	defer func() { _ = resp.Body.Close() }()
	b, e := io.ReadAll(resp.Body)
	require.NoError(t, e)
	return resp, b
}
func previewHTTP(t *testing.T, env *testenv.Env, method, path string, body any) (int, []byte) {
	t.Helper()
	r, b := previewRequest(t, env, method, path, body, nil)
	return r.StatusCode, b
}

func TestCronPreviewFutureHorizonDoesNotChangeNextSweep(t *testing.T) {
	previewBackends(t, func(t *testing.T, f *previewFixture) {
		a := f.now.Add(-3 * time.Hour)
		f.progress(t, cron.RecurringProgress{Version: 1, Anchor: a, Through: a})
		out := f.get(t, "?from=9999-12-31T12:00:00Z")
		require.NotEmpty(t, out.Error)
		require.Empty(t, out.NextSweep.Error, "the requested future horizon does not change planning at now")
		require.Len(t, out.NextSweep.Due, 3)
	})
}

func TestCronPreviewUnsavedYearZero(t *testing.T) {
	env := testenv.New(t)
	p := previewProject(t, env, "preview-project")
	j := previewAdapter(t)
	j.Trigger = cron.Trigger{Kind: "cron", Cron: "0 0 * * *"}
	status, raw := previewHTTP(t, env, "POST", fmt.Sprintf("/api/v1/projects/%d/cron/preview", p.ID), map[string]any{"definition": j, "from": "0000-12-30T00:00:00Z", "count": 1})
	require.Equal(t, 200, status, string(raw))
	out := previewDecode(t, raw)
	require.Equal(t, previewTime(t, "0000-12-31T00:00:00Z"), out.Occurrences[0].At)
}

func TestCronPreviewStoredOnceBounds(t *testing.T) {
	previewBackends(t, func(t *testing.T, f *previewFixture) {
		for _, instant := range []string{"9999-12-31T23:00:00-02:00", "0000-01-01T00:00:00+01:00"} {
			t.Run(instant, func(t *testing.T) {
				j := previewAdapter(t)
				j.Trigger = cron.Trigger{Kind: "once", At: instant}
				f.definition(t, j)
				out := f.get(t, "")
				require.NotEmpty(t, out.Error)
				require.Empty(t, out.Occurrences)
			})
		}
	})
}
