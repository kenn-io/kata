package tui

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	kataapi "go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/pkg/client/generated"
)

type passoverTestAPI struct {
	*cronEnabledTestAPI
	passover  *db.CronPassoverState
	discarded []enabledCall
}

func (a *passoverTestAPI) readCronJob(context.Context, int64, string) (cronJobRead, error) {
	return cronJobRead{Job: a.shown, Passover: a.passover}, a.showErr
}
func (a *passoverTestAPI) ResumeCronJobDiscard(_ context.Context, _ int64, id, expected, actor string) (db.CronJob, error) {
	a.discarded = append(a.discarded, enabledCall{id, expected, actor, true})
	return a.shown, a.errorWrite
}

func TestCronResumePassoverCount(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "clamped", true: "invalid"}[invalid], func(t *testing.T) {
			m, base := enabledFixture(t, pausedNative())
			withEnabledBacklog(base)
			api := &passoverTestAPI{cronEnabledTestAPI: base, passover: &db.CronPassoverState{Through: time.Now().UTC().Add(time.Hour), Invalid: invalid}}
			m.api = api
			m, lookup := press(m, keyRune('p'))
			m, write := deliverEnabled(t, m, lookup)
			require.Equal(t, modalNone, m.modal)
			if invalid {
				require.Nil(t, m.cron.enabled)
				require.NotNil(t, m.toast)
				require.Equal(t, "Could not change the job: invalid cutoff; repair with --discard-missed", m.toast.text)
				require.Empty(t, api.enabledCalls)
			} else {
				require.NotNil(t, write)
				_, _ = deliverEnabled(t, m, write)
				require.Len(t, api.enabledCalls, 1)
			}
		})
	}
}
func TestCronDiscardResumeChoice(t *testing.T) {
	for _, supported := range []bool{false, true} {
		for _, fence := range []string{"none", "connection", "scope", "selection", "readonly"} {
			t.Run(map[bool]string{false: "old", true: "supported"}[supported]+"/"+fence, func(t *testing.T) {
				m, base := enabledFixture(t, pausedNative())
				withEnabledBacklog(base)
				if supported {
					m.cron.data.project.controls = []string{"resume_discard_missed"}
				}
				api := &passoverTestAPI{cronEnabledTestAPI: base}
				m.api = api
				m, lookup := press(m, keyRune('p'))
				m, _ = deliverEnabled(t, m, lookup)
				require.Equal(t, modalCronResume, m.modal)
				view := stripANSI(m.overlayInputAndModal(m.viewBody()))
				if supported {
					require.Contains(t, view, "d resumes and discards all missed runs up to")
					require.Contains(t, view, "when it is saved, including older backlog.")
					require.Contains(t, view, "d discard missed")
				} else {
					require.NotContains(t, view, "discard")
				}
				switch fence {
				case "connection":
					m.connGen++
				case "scope":
					m.scopeGen++
				case "selection":
					m.cron.data.jobs = nil
				case "readonly":
					m.issueScoped = true
					m.scopedWritable = false
				}
				m, write := m.routeModalKey(keyRune('d'))
				if supported && fence == "none" {
					require.NotNil(t, write)
					_, _ = deliverEnabled(t, m, write)
					require.Equal(t, []enabledCall{{api.shown.UID, api.shown.DefinitionEventUID, "operator", true}}, api.discarded)
				} else {
					require.Nil(t, write)
					require.Empty(t, api.discarded)
				}
				require.Empty(t, api.enabledCalls)
			})
		}
	}
}
func TestCronNextPassover(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(3 * time.Minute)
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "adapter", true: "native"}[native], func(t *testing.T) {
			m := cronFixture(t)
			j := m.cron.data.jobs[0]
			j.Definition.Enabled = true
			if native {
				nativeNotify(generated.TriggerResponse{Kind: "interval", IntervalSeconds: new(int64(60))})(&j.Definition)
			} else {
				j.Definition.Trigger = generated.TriggerResponse{Kind: "cron", Cron: new("* * * * *"), Timezone: new("UTC")}
			}
			p := cronData{progress: map[string]cron.RecurringProgress{j.UID: {Version: 1, Anchor: now.Add(-time.Hour), Through: now.Add(-time.Hour)}}, parentOpen: map[string]bool{nextParentUID: true}, passovers: map[string]*db.CronPassoverState{j.UID: {Through: cutoff}}}
			_, lines := cronNextRuns(p, j, now)
			require.NotEmpty(t, lines)
			require.Contains(t, lines[0], "12:04")
			p.passovers[j.UID].Invalid = true
			_, lines = cronNextRuns(p, j, now)
			require.Equal(t, []string{"unavailable"}, lines)
		})
	}
}

func TestSnapshot_CronDiscardResume(t *testing.T) {
	defer snapshotInit(t)()
	for _, width := range []int{80, 120} {
		m, _ := enabledFixture(t, pausedNative())
		m.width = width
		m.modal = modalCronResume
		m.cron.enabled = &cronEnabledRequest{name: "Weekly report", count: 5, allowDiscard: true}
		assertGolden(t, fmt.Sprintf("cron-resume-discard-%d", width), m.View().Content)
	}
}

func TestCronDiscardClientWire(t *testing.T) {
	for _, discard := range []bool{false, true} {
		t.Run(fmt.Sprint(discard), func(t *testing.T) {
			record := pausedNative()
			record.Definition.Options = []byte(`{"large":9007199254740993}`)
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodPost, r.Method)
				var body map[string]jsontext.Value
				require.NoError(t, json.UnmarshalRead(r.Body, &body))
				if discard {
					require.Equal(t, "true", string(body["discard_missed"]))
				} else {
					require.NotContains(t, body, "discard_missed")
				}
				require.JSONEq(t, fmt.Sprintf("%q", record.DefinitionEventUID), string(body["expected_event_uid"]))
				respondJSON(t, w, map[string]any{"job": kataapi.CronJobFrom(record), "passover": db.CronPassoverState{Through: time.Now().UTC(), Scope: "job"}})
			})
			var got db.CronJob
			var err error
			if discard {
				got, err = client.ResumeCronJobDiscard(t.Context(), 7, record.UID, record.DefinitionEventUID, "operator")
			} else {
				got, err = client.CronJobEnabled(t.Context(), 7, record.UID, record.DefinitionEventUID, "operator", true)
			}
			require.NoError(t, err)
			require.Equal(t, string(record.Definition.Options), string(got.Definition.Options))
		})
	}
}
func TestCronClientPassoverEnvelope(t *testing.T) {
	record := pausedNative()
	record.Definition.Options = []byte(`{"large":9007199254740993}`)
	cutoff := time.Now().UTC().Truncate(time.Second)
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		state := db.CronPassoverState{Through: cutoff, Scope: "job"}
		if strings.HasSuffix(r.URL.Path, "/jobs") {
			respondJSON(t, w, map[string]any{"jobs": []kataapi.CronJob{kataapi.CronJobFrom(record)}, "passovers": map[string]any{record.UID: state}})
			return
		}
		respondJSON(t, w, map[string]any{"job": kataapi.CronJobFrom(record), "passover": state})
	})
	one, err := client.readCronJob(t.Context(), 7, record.UID)
	require.NoError(t, err)
	require.Equal(t, cutoff, one.Passover.Through)
	require.Equal(t, string(record.Definition.Options), string(one.Job.Definition.Options))
	list, err := client.readCronJobs(t.Context(), 7)
	require.NoError(t, err)
	require.Len(t, list.Jobs, 1)
	require.Equal(t, one.Passover, list.Passovers[record.UID])
}

// Pausing remains available when scheduling is blocked by malformed cutoff
// state; it does not plan, launch work, or repair the retained metadata.
func TestCronPauseWithInvalidPassover(t *testing.T) {
	record := pausedNative()
	record.Definition.Enabled = true
	m, base := enabledFixture(t, record)
	api := &passoverTestAPI{cronEnabledTestAPI: base, passover: &db.CronPassoverState{Invalid: true}}
	m.api = api
	m, lookup := press(m, keyRune('p'))
	m, write := deliverEnabled(t, m, lookup)
	require.NotNil(t, write)
	_, _ = deliverEnabled(t, m, write)
	require.Equal(t, []enabledCall{{record.UID, record.DefinitionEventUID, "operator", false}}, api.enabledCalls)
	require.Empty(t, api.discarded)
}

// Adapter once previews use a strict after boundary, including equality.
func TestCronNextOncePassoverBoundary(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(time.Hour)
	for _, delta := range []time.Duration{-time.Second, 0, time.Second} {
		t.Run(delta.String(), func(t *testing.T) {
			m := cronFixture(t)
			j := m.cron.data.jobs[0]
			j.Definition.Enabled = true
			j.Definition.Trigger = generated.TriggerResponse{Kind: "once", At: new(cutoff.Add(delta).Format(time.RFC3339Nano))}
			p := cronData{passovers: map[string]*db.CronPassoverState{j.UID: {Through: cutoff}}}
			_, lines := cronNextRuns(p, j, now)
			if delta <= 0 {
				require.Empty(t, lines)
			} else {
				require.Equal(t, []string{"2026-01-01 13:00:01 UTC"}, lines)
			}
		})
	}
}
