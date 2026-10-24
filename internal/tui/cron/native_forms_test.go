package cron

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"go.kenn.io/kata/internal/tui/cron/records"
)

func TestCronNativeFormsDoNotRequireHostConfig(t *testing.T) {
	j := nativeFixture().Jobs[0]
	j.Definition = jsontext.Value(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Write a summary"},"issue":{"kind":"per-run","title":"Summary"},"overlap":"allow","catchup":"skip","checkout_key":"example-checkout","secret_refs":{"API_KEY":"example-secret"},"options":{"agent":"portable-adapter","number":9007199254740993,"description":"opaque"}}`)
	f := newJobForm(context.Background(), nil, &j, nil, editorContext{ProjectUID: "7", Project: "example-project"})
	f.Set("prompt", "Updated prompt")
	body, err := f.definition()
	if err != nil {
		t.Fatal(err)
	}
	if err = f.validate(body); err != nil {
		t.Fatal("native document required host config", err)
	}
	if !strings.Contains(string(body), "9007199254740993") {
		t.Fatal("lost exact option", string(body))
	}
	for _, field := range f.fields() {
		if field.Key == "profile" || field.Key == "favorite" || field.Key == "run_parent" {
			t.Fatal("host field retained", field)
		}
	}
}

func TestCronTriggerKindClearsOtherFields(t *testing.T) {
	j := nativeFixture().Jobs[0]
	j.Definition = jsontext.Value(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"issue-deadline","issue_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","lead_seconds":60},"action":{"kind":"execute","prompt":"Write summary"},"issue":{"kind":"per-run","title":"Summary"},"overlap":"allow","catchup":"skip"}`)
	f := newJobForm(context.Background(), nil, &j, nil, editorContext{})
	f.Set("trigger", "manual")
	body, err := f.definition()
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err = json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	trigger := raw["trigger"].(map[string]any)
	for _, key := range []string{"issue_uid", "lead_seconds", "cron", "at", "interval_seconds"} {
		if _, ok := trigger[key]; ok {
			t.Fatal("other trigger field survived", key, string(body))
		}
	}
}

func TestCronRecurringRecipientEditable(t *testing.T) {
	j := records.Job{UID: "job", Name: "Recurring summary", DefinitionEventUID: "guard", Definition: jsontext.Value(`{"version":1,"kind":"job","enabled":false,"trigger":{"kind":"interval","interval_seconds":3600,"timezone":"UTC"},"action":{"kind":"notify","recipient":"current-owner-or-author"},"issue":{"kind":"per-run","parent_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","title":"Summary"},"overlap":"allow","catchup":"latest"}`)}
	f := newJobForm(context.Background(), nil, &j, nil, editorContext{})
	editable := map[string]bool{}
	for _, field := range f.fields() {
		editable[field.Key] = field.Kind != 4
	}
	if editable["parent"] || !editable["recipient"] || editable["mode"] {
		t.Fatal("native recurring binding policy not reflected", editable)
	}
	f.Set("recipient", "example-recipient")
	body, err := f.definition()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "example-recipient") || !strings.Contains(string(body), "01ARZ3NDEKTSV4RRFFQ69G5FAV") {
		t.Fatal(string(body))
	}
}

type nativeFormWriter struct {
	save func(records.Job) (records.Job, error)
	show func(string) (records.Job, error)
}

func (w nativeFormWriter) SaveJob(_ context.Context, j records.Job, _ string) (records.Job, error) {
	return w.save(j)
}
func (w nativeFormWriter) ShowJob(_ context.Context, id string) (records.Job, error) {
	return w.show(id)
}
func (w nativeFormWriter) ShowIssue(context.Context, string) (records.IssueView, error) {
	return records.IssueView{}, nil
}

func formTestJob() records.Job {
	return records.Job{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Name: "Summary", DefinitionEventUID: "original-guard", Definition: jsontext.Value(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Write a summary"},"issue":{"kind":"per-run","title":"Summary"},"overlap":"allow","catchup":"skip","options":{"number":9007199254740993}}`)}
}

func TestCronUncertainSaveReadsBeforeRetry(t *testing.T) {
	j := formTestJob()
	calls := []string{}
	var bodies []string
	w := nativeFormWriter{save: func(j records.Job) (records.Job, error) {
		calls = append(calls, "save")
		bodies = append(bodies, string(j.Definition))
		return records.Job{}, errors.New("transport unavailable")
	}, show: func(string) (records.Job, error) {
		calls = append(calls, "read")
		return records.Job{}, errors.New("read unavailable")
	}}
	f := newJobForm(context.Background(), w, &j, nil, editorContext{ProjectUID: "7"})
	f.Set("prompt", "Changed prompt")
	cmd := f.save()
	if cmd == nil {
		t.Fatal(f.status)
	}
	result := cmd()
	f.Update(result)
	if f.phase != phaseUncertain {
		t.Fatal(f.phase, f.status)
	}
	calls = nil
	cmd = f.save()
	if cmd == nil {
		t.Fatal(f.status)
	}
	cmd()
	if len(calls) == 0 || calls[0] != "read" {
		t.Fatal("uncertain retry wrote before readback", calls)
	}
}

func TestCronConflictKeepsDraftAndOriginalGuard(t *testing.T) {
	j := formTestJob()
	newer := j
	newer.DefinitionEventUID = "newer-guard"
	newer.Name = "Changed elsewhere"
	w := nativeFormWriter{save: func(pending records.Job) (records.Job, error) {
		return records.Job{}, &ConflictError{Pending: pending, Newer: newer, Cause: errors.New("conflict")}
	}, show: func(string) (records.Job, error) {
		t.Fatal("conflict current already provided")
		return records.Job{}, nil
	}}
	f := newJobForm(context.Background(), w, &j, nil, editorContext{})
	f.Set("prompt", "My draft")
	cmd := f.save()
	if cmd == nil {
		t.Fatal(f.status)
	}
	f.Update(cmd())
	if f.phase != phaseConflict || f.Value("prompt") != "My draft" || f.job.DefinitionEventUID != "original-guard" {
		t.Fatal(f.phase, f.job, f.values)
	}
	if !strings.Contains(ansi.Strip(f.View()), "original definition guard kept") {
		t.Fatal(f.View())
	}
}

func TestCronNotifyIssueDateUsesTriggerTarget(t *testing.T) {
	j := formTestJob()
	f := newJobForm(context.Background(), nil, &j, nil, editorContext{})
	f.Set("mode", "notify")
	f.Set("recipient", "current-owner-or-author")
	f.Set("trigger", "issue-deadline")
	f.Set("trigger_issue", "01ARZ3NDEKTSV4RRFFQ69G5FAV")
	f.Set("lead", "5m")
	body, err := f.definition()
	if err != nil {
		t.Fatal(err)
	}
	if err = f.validate(body); err != nil {
		t.Fatal(string(body), err)
	}
}

type nativeWorkflowWriter struct{ newer records.Workflow }

func (w nativeWorkflowWriter) SaveWorkflow(_ context.Context, p records.Workflow) (records.Workflow, error) {
	return records.Workflow{}, &ConflictError{Pending: p, Newer: w.newer, Cause: errors.New("conflict")}
}
func (w nativeWorkflowWriter) ShowWorkflow(context.Context, string) (records.Workflow, error) {
	return records.Workflow{}, errors.New("readback should use current from conflict")
}
func TestCronWorkflowConflictUsesCurrent(t *testing.T) {
	j := nativeFixture().Workflows[0]
	j.DefinitionEventUID = "original"
	newer := j
	newer.DefinitionEventUID = "newer"
	newer.Name = "New name"
	f, err := newWorkflowForm(context.Background(), nativeWorkflowWriter{newer}, j)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	msg := f.save(true)().(workflowMsg)
	f.Update(msg)
	if f.newer.DefinitionEventUID != "newer" || f.newer.Name != newer.Name {
		t.Fatal("conflict handling lost daemon current", f.newer)
	}
	if f.workflow.DefinitionEventUID != "original" {
		t.Fatal("guard changed")
	}
}

func TestCronWorkflowCleanupPreservesForeignFile(t *testing.T) {
	f, err := newWorkflowForm(context.Background(), nativeWorkflowWriter{}, nativeFixture().Workflows[0])
	if err != nil {
		t.Fatal(err)
	}
	path := f.path
	replacement := filepath.Join(t.TempDir(), "foreign.json")
	if err := os.WriteFile(replacement, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path) //nolint:gosec // G304: this is the temporary draft owned by the test.
	if err != nil || string(body) != "foreign" {
		t.Fatal("removed a file owned by another editor", string(body), err)
	}
	_ = os.Remove(path)
}

type formHTTPError int

func (e formHTTPError) Error() string   { return "native request rejected" }
func (e formHTTPError) HTTPStatus() int { return int(e) }

func TestCronUncertainCreateRetriesConfirmedAbsence(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			j := formTestJob()
			if !existing {
				j.DefinitionEventUID = ""
			}
			var attempts []records.Job
			w := nativeFormWriter{save: func(j records.Job) (records.Job, error) {
				attempts = append(attempts, j)
				if len(attempts) == 1 {
					return records.Job{}, errors.New("transport lost before commit")
				}
				return j, nil
			}, show: func(string) (records.Job, error) { return records.Job{}, formHTTPError(404) }}
			f := newJobForm(context.Background(), w, &j, nil, editorContext{})
			f.creating = !existing
			f.Set("prompt", "Updated draft")
			f.Update(f.save()())
			if f.phase != phaseUncertain {
				t.Fatal(f.phase)
			}
			result := f.save()().(savedMsg)
			if existing {
				if len(attempts) != 1 {
					t.Fatal("missing guarded update became a create")
				}
				return
			}
			if result.err != nil || len(attempts) != 2 {
				t.Fatalf("confirmed absent create did not retry: %v, attempts %d", result.err, len(attempts))
			}
			if attempts[0].UID != attempts[1].UID || attempts[1].DefinitionEventUID != "" || string(attempts[0].Definition) != string(attempts[1].Definition) {
				t.Fatal("retry changed frozen identity or payload")
			}
		})
	}
}

func TestCronRecurringNoticePreservesOptions(t *testing.T) {
	j := formTestJob()
	j.Definition = []byte(strings.Replace(string(j.Definition), `"number":9007199254740993`, `"description":"Keep this description"`, 1))
	f := newJobForm(t.Context(), nil, &j, nil, editorContext{})
	f.Set("mode", "recurring")
	var lines []string
	body, _ := f.renderLines()
	for _, line := range body {
		lines = append(lines, ansi.Strip(line.Text))
	}
	text := strings.Join(lines, "\n")
	if strings.Contains(text, "dropped on save") || !strings.Contains(text, "Recurring jobs require empty options") {
		t.Fatal("recurring notice contradicts preserved native options", text)
	}
	definition, err := f.definition()
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Options map[string]string `json:"options"`
	}
	if err = json.Unmarshal(definition, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Options["description"] != "Keep this description" {
		t.Fatal("conversion lost opaque options", string(definition))
	}
}

func TestCronWriteDenialRetainsOperationProject(t *testing.T) {
	cases := []struct {
		name, key string
		msg       tea.Msg
	}{
		{"job save", "8", savedMsg{err: formHTTPError(403)}},
		{"workflow save", "8", workflowMsg{err: formHTTPError(403)}},
		{"pause", "7", jobPausedMsg{project: "7", err: formHTTPError(403)}},
		{"delete", "7", jobDeletedMsg{job: records.Job{ProjectUID: "7"}, err: formHTTPError(403)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newModel(Options{})
			m.editorProject = "8"
			m.busy = true
			cmd, handled := m.handleFormMessage(tc.msg)
			if !handled || cmd == nil {
				t.Fatal("denial not handled")
			}
			denied := cmd().(AccessLostMsg)
			if denied.Status != 403 || denied.ProjectKey != tc.key || m.busy {
				t.Fatal("write lost its captured project", denied, m.busy)
			}
		})
	}
}

type sequenceWorkflowWriter struct{ shown []records.Workflow }

func (w *sequenceWorkflowWriter) SaveWorkflow(context.Context, records.Workflow) (records.Workflow, error) {
	return records.Workflow{}, errors.New("connection reset")
}
func (w *sequenceWorkflowWriter) ShowWorkflow(context.Context, string) (records.Workflow, error) {
	current := w.shown[0]
	w.shown = w.shown[1:]
	return current, nil
}

func TestCronWorkflowRetryConflictRefreshesNewerCopy(t *testing.T) {
	j := nativeFixture().Workflows[0]
	j.DefinitionEventUID = "original"
	first, second := j, j
	first.DefinitionEventUID, first.Definition = "first", jsontext.Value(`{"version":1,"steps":[{"key":"first","kind":"command","command":"echo first"}]}`)
	second.DefinitionEventUID, second.Definition = "second", jsontext.Value(`{"version":1,"steps":[{"key":"second","kind":"command","command":"echo second"}]}`)
	f, err := newWorkflowForm(context.Background(), &sequenceWorkflowWriter{shown: []records.Workflow{first, second}}, j)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	f.Update(f.save(true)())
	f.Update(f.save(true)())
	body, err := os.ReadFile(f.newerPath) //nolint:gosec // G304: the sidecar belongs to the editor under test.
	if err != nil || f.newer.DefinitionEventUID != "second" || string(body) != string(second.Definition) {
		t.Fatalf("newer copy %q does not match newer revision %s: %v", body, f.newer.DefinitionEventUID, err)
	}
}
