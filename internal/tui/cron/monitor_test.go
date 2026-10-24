package cron

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	native "go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/shortid"
	"go.kenn.io/kata/internal/tui/cron/records"
)

func TestCronIssueReferenceKeepsOpaqueUID(t *testing.T) {
	const uid = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	m := newModel(Options{})
	d := nativeFixture()
	d.Runs[0].IssueUID = new(uid)
	m.SetData(d)
	if got := m.runIssueRef(d.Runs[0]); !strings.Contains(got, uid) {
		t.Fatal("invented collision-prone short ID", got)
	}
	for _, ref := range []string{m.runIssueRef(d.Runs[0]), m.listIssueRef("example-project", uid)} {
		if _, err := shortid.Parse(ref); err != nil {
			t.Fatal("displayed UID cannot be used as an issue reference", ref, err)
		}
	}
}

type monitorWorkflowWriter struct {
	saveErr, readErr error
	validate         bool
}

func (w monitorWorkflowWriter) SaveWorkflow(_ context.Context, job records.Workflow) (records.Workflow, error) {
	if w.validate {
		_, err := native.ParseWorkflow(job.Definition)
		return records.Workflow{}, err
	}
	return records.Workflow{}, w.saveErr
}
func (w monitorWorkflowWriter) ShowWorkflow(context.Context, string) (records.Workflow, error) {
	return records.Workflow{}, w.readErr
}

func TestCronReadbackDenialRetainsStatus(t *testing.T) {
	for _, status := range []int{401, 403} {
		j := formTestJob()
		f := newJobForm(t.Context(), nativeFormWriter{save: func(records.Job) (records.Job, error) { return records.Job{}, errors.New("transport failed") }, show: func(string) (records.Job, error) { return records.Job{}, formHTTPError(status) }}, &j, nil, editorContext{})
		f.Set("prompt", "Changed")
		if result := f.save()().(savedMsg); records.Status(result.err) != status {
			t.Error("job readback denial hidden", result.err)
		}
		wf, err := newWorkflowForm(t.Context(), monitorWorkflowWriter{saveErr: errors.New("transport failed"), readErr: formHTTPError(status)}, nativeFixture().Workflows[0])
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = wf.Close() })
		if result := wf.save(true)().(workflowMsg); records.Status(result.err) != status {
			t.Error("workflow readback denial hidden", result.err)
		}
	}
}

func TestCronWorkflowLocalRejectionRemainsEditable(t *testing.T) {
	f, err := newWorkflowForm(t.Context(), monitorWorkflowWriter{validate: true, readErr: errors.New("not found")}, nativeFixture().Workflows[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if err = os.WriteFile(f.path, []byte(`{"version":1,"steps":[{"key":"step","kind":"prompt","prompt":"work","unknown":true}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	result := f.save(true)().(workflowMsg)
	f.Update(result)
	if result.err == nil || result.uncertain || f.frozen != nil {
		t.Fatal("local rejection froze an unsent draft", result.err, result.uncertain)
	}
	_, cmd := f.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})
	if cmd == nil {
		t.Fatal("rejected draft cannot be edited")
	}
}

func TestCronPersistedRecurringBindingCannotBeEdited(t *testing.T) {
	j := formTestJob()
	j.Definition = []byte(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"notify","recipient":"worker"},"issue":{"kind":"per-run","parent_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","title":"Summary"},"overlap":"allow","catchup":"latest"}`)
	f := newJobForm(t.Context(), nil, &j, nil, editorContext{})
	for _, key := range []string{"mode", "parent"} {
		found := false
		for i, field := range f.fields() {
			if field.Key == key {
				found = true
				f.index = i
				f.input(tea.KeyPressMsg{Code: tea.KeyEnter})
				if f.active != "" || !strings.Contains(f.status, "read-only") {
					t.Fatal("persisted recurring binding accepted edit", key, f.active, f.status)
				}
			}
		}
		if !found {
			t.Fatal("recurring binding field disappeared", key)
		}
	}
	creating := newJobForm(t.Context(), nil, nil, nil, editorContext{})
	creating.Set("mode", "recurring")
	for i, field := range creating.fields() {
		if field.Key == "parent" {
			creating.index = i
			creating.input(tea.KeyPressMsg{Code: tea.KeyEnter})
			if creating.active != "parent" {
				t.Fatal("creation cannot choose its parent")
			}
			return
		}
	}
	t.Fatal("creation has no parent control")
}

func TestCronUnqualifiedIssueUIDKeepsProjectVisible(t *testing.T) {
	m := newModel(Options{Project: "first"})
	d := nativeFixture()
	d.Projects = map[string]Data{"7": {Project: "first"}, "8": {Project: "second"}}
	d.Jobs[0].ProjectUID = "8"
	d.Jobs[0].Project = "second"
	d.Runs[0].ProjectKey = "8"
	d.Runs[0].IssueUID = new("01ARZ3NDEKTSV4RRFFQ69G5FAV")
	m.SetData(d)
	for _, tab := range []int{0, 1} {
		m.SelectTab(tab)
		if text := m.View().Content; !strings.Contains(text, "[second]") {
			t.Fatal("linked UID hid project ownership", tab, text)
		}
	}
}

type monitorService struct{ Service }

func (monitorService) SaveJob(_ context.Context, j records.Job, _ string) (records.Job, error) {
	return j, nil
}

func TestCronDiscardedEditorCannotCloseReplacement(t *testing.T) {
	m := newModel(Options{Services: map[string]Service{"7": monitorService{}, "8": monitorService{}}})
	j := formTestJob()
	m.openJobEditor("7", &j)
	m.editor.(*jobForm).Set("prompt", "First draft")
	cmd, _ := m.formKey(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("save did not start")
	}
	late := cmd()
	m.ForgetProject("7")
	m.openJobEditor("8", &j)
	replacement := m.editor
	m.editor.(*jobForm).Set("prompt", "Second draft")
	if next, _ := m.handleFormMessage(late); next != nil {
		m.handleFormMessage(next())
	}
	if m.editor != replacement {
		t.Fatal("late save discarded the replacement draft")
	}
}

func TestCronEditorInterruptReachesProgram(t *testing.T) {
	m := newModel(Options{Services: map[string]Service{"7": monitorService{}}})
	m.openJobEditor("7", nil)
	cmd, _ := m.formKey(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("interrupt produced no command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("editor envelope swallowed the program quit")
	}
}

func TestCronPolicyConversionClearsIncompatibleScheduledOffset(t *testing.T) {
	for _, mode := range []string{"prompt", "notify", "recurring"} {
		t.Run(mode, func(t *testing.T) {
			j := formTestJob()
			j.Definition = []byte(strings.Replace(string(j.Definition), `"title":"Summary"`, `"title":"Summary","scheduled_offset_seconds":60`, 1))
			f := newJobForm(t.Context(), nil, &j, nil, editorContext{})
			f.Set("mode", mode)
			if mode == "prompt" {
				f.Set("issue_kind", "existing")
			}
			body, err := f.definition()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), "scheduled_offset_seconds") {
				t.Fatal("incompatible offset survived conversion", string(body))
			}
		})
	}
}

func TestCronRecurringCreationHonorsEnabled(t *testing.T) {
	f := newJobForm(t.Context(), nil, nil, nil, editorContext{})
	f.Set("mode", "recurring")
	chosen := f.Value("enabled") != "true"
	f.Set("enabled", fmt.Sprint(chosen))
	body, err := f.definition()
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Enabled != chosen {
		t.Fatal("form enabled value discarded", string(body))
	}
}

func TestCronProjectChooserKeepsSelectionVisible(t *testing.T) {
	for _, width := range []int{80, 160} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			services := map[string]Service{}
			projects := map[string]Data{}
			for i := 1; i <= 30; i++ {
				key := fmt.Sprint(i)
				services[key] = monitorService{}
				projects[key] = Data{Project: fmt.Sprintf("project-%02d", i)}
			}
			m := newModel(Options{Services: services})
			m.Resize(width, 10)
			m.SetData(Data{Projects: projects})
			m.scroll, m.follow = 100, false
			m.newJob()
			for i := range 30 {
				want := "▶ " + m.projectName(m.projectChoices[m.projectCursor])
				if text := plain(m.View().Content); !strings.Contains(text, want) {
					t.Fatalf("selection hidden at %d: %s", i, text)
				}
				m.formKey(tea.KeyPressMsg{Code: 'j', Text: "j"})
			}
			// Wrapping to the first choice must also reset the visible window.
			if text := plain(m.View().Content); !strings.Contains(text, "▶ project-01") {
				t.Fatal(text)
			}
		})
	}
}

type monitorActionService struct {
	Service
	err error
}

func (s monitorActionService) Pause(j records.Job) (records.Job, error) { return j, s.err }
func (s monitorActionService) Delete(records.Job) error                 { return s.err }

func TestCronForgottenProjectRejectsLateActions(t *testing.T) {
	for _, action := range []string{"p", "D"} {
		for _, conflict := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/conflict=%t", action, conflict), func(t *testing.T) {
				d := nativeFixture()
				var err error
				if conflict {
					err = &ConflictError{Pending: d.Jobs[0], Newer: d.Jobs[0]}
				}
				m := newModel(Options{Services: map[string]Service{"7": monitorActionService{err: err}}})
				m.SetData(d)
				cmd := m.jobKeyAction(action)
				if action == "D" {
					cmd = m.confirmJobKey("y")
				}
				if cmd == nil {
					t.Fatal("action did not start")
				}
				late := cmd()
				m.ForgetProject("7")
				m.SetData(Data{})
				next, _ := m.handleFormMessage(late)
				if len(m.data.Jobs) != 0 || m.conflict != nil || next != nil {
					t.Fatal("late action restored forgotten project content")
				}
				if m.busy {
					t.Fatal("forgotten action retained global busy state")
				}
			})
		}
	}
}

func TestCronRestoredProjectIgnoresOldActionDenial(t *testing.T) {
	for _, action := range []string{"p", "D"} {
		t.Run(action, func(t *testing.T) {
			m := newModel(Options{Services: map[string]Service{"7": monitorActionService{err: formHTTPError(403)}}})
			d := nativeFixture()
			m.SetData(d)
			cmd := m.jobKeyAction(action)
			if action == "D" {
				cmd = m.confirmJobKey("y")
			}
			late := cmd()
			m.ForgetProject("7")
			m.SetData(d)
			m.SetServices(map[string]Service{"7": monitorService{}}, false, nil)
			m.openJobEditor("7", &d.Jobs[0])
			editor := m.editor
			next, _ := m.handleFormMessage(late)
			if next != nil || m.editor != editor {
				t.Fatal("old denial affected restored project authority")
			}
		})
	}
}

type monitorDeniedSaveService struct{ monitorService }

func (monitorDeniedSaveService) SaveJob(context.Context, records.Job, string) (records.Job, error) {
	return records.Job{}, formHTTPError(403)
}

func TestCronRestoredEditorIgnoresOldSaveDenial(t *testing.T) {
	m := newModel(Options{Services: map[string]Service{"7": monitorDeniedSaveService{}}})
	j := formTestJob()
	m.openJobEditor("7", &j)
	m.editor.(*jobForm).Set("prompt", "First draft")
	cmd, _ := m.formKey(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	late := cmd()
	m.ForgetProject("7")
	m.SetServices(map[string]Service{"7": monitorService{}}, false, nil)
	m.openJobEditor("7", &j)
	replacement := m.editor
	next, _ := m.handleFormMessage(late)
	if next != nil || m.editor != replacement {
		t.Fatal("discarded save denial affected replacement editor")
	}
}

type monitorAcknowledgedWorkflowWriter struct{ current records.Workflow }

func (w monitorAcknowledgedWorkflowWriter) SaveWorkflow(_ context.Context, pending records.Workflow) (records.Workflow, error) {
	pending.DefinitionEventUID = "acknowledged"
	return pending, nil
}
func (w monitorAcknowledgedWorkflowWriter) ShowWorkflow(context.Context, string) (records.Workflow, error) {
	return w.current, nil
}

func TestCronWorkflowAcknowledgementUpdatesCache(t *testing.T) {
	d := nativeFixture()
	f, err := newWorkflowForm(t.Context(), monitorAcknowledgedWorkflowWriter{}, d.Workflows[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	m := newModel(Options{})
	m.SetData(d)
	m.editor, m.editorProject = f, "7"
	result := f.save(true)()
	closed, _ := m.handleFormMessage(result)
	if closed == nil {
		t.Fatal("save did not close")
	}
	m.handleFormMessage(closed())
	if m.data.Workflows[0].DefinitionEventUID != "acknowledged" {
		t.Fatal("acknowledged workflow lost before refresh")
	}
}

type monitorWorkflowReadbackWriter struct{ current records.Workflow }

func (w monitorWorkflowReadbackWriter) SaveWorkflow(context.Context, records.Workflow) (records.Workflow, error) {
	return records.Workflow{}, errors.New("transport failed")
}
func (w monitorWorkflowReadbackWriter) ShowWorkflow(context.Context, string) (records.Workflow, error) {
	return w.current, nil
}
func TestCronWorkflowReadbackKeepsProjectLabel(t *testing.T) {
	original := nativeFixture().Workflows[0]
	current := original
	current.Project = ""
	current.DefinitionEventUID = "acknowledged"
	f, err := newWorkflowForm(t.Context(), monitorWorkflowReadbackWriter{current}, original)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	_, cmd := f.Update(f.save(true)())
	if cmd == nil {
		t.Fatal("readback did not finish save")
	}
	closed := cmd().(editorClosedMsg)
	if closed.SavedWorkflow == nil || closed.SavedWorkflow.Project != original.Project {
		t.Fatal("readback lost captured project label")
	}
}

func issueDateNotification(trigger string) records.Job {
	j := formTestJob()
	j.Definition = []byte(fmt.Sprintf(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":%q,"issue_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV"},"action":{"kind":"notify","recipient":"operator","message":"Attention"},"overlap":"forbid","catchup":"latest"}`, trigger))
	return j
}
func TestCronIssueDateNotificationExplicitTargetIsValid(t *testing.T) {
	for _, trigger := range []string{"issue-scheduled", "issue-deadline"} {
		t.Run(trigger, func(t *testing.T) {
			j := issueDateNotification(trigger)
			f := newJobForm(t.Context(), nil, &j, nil, editorContext{})
			f.Set("issue", "01ARZ3NDEKTSV4RRFFQ69G5FAW")
			body, err := f.definition()
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := native.ParseJob(body)
			if err != nil {
				t.Fatal("explicit notification target is invalid", err, string(body))
			}
			if decoded.Issue == nil || decoded.Issue.Kind != "existing" || decoded.Issue.UID != "01ARZ3NDEKTSV4RRFFQ69G5FAW" || decoded.Trigger.IssueUID != "01ARZ3NDEKTSV4RRFFQ69G5FAV" {
				t.Fatal("explicit target or original trigger was lost", string(body))
			}
		})
	}
}
func TestCronIssueDateNotificationOpensTriggerIssue(t *testing.T) {
	for _, trigger := range []string{"issue-scheduled", "issue-deadline"} {
		t.Run(trigger, func(t *testing.T) {
			j := issueDateNotification(trigger)
			j.ProjectUID = "7"
			m := newModel(Options{})
			m.SetData(Data{Projects: map[string]Data{"7": {Project: "example-project"}}, Jobs: []records.Job{j}})
			cmd := m.routeKey(press("i"))
			if cmd == nil {
				t.Fatal("notification issue key did nothing")
			}
			got := cmd().(IssueMsg)
			if got.ProjectKey != "7" || got.UID != "01ARZ3NDEKTSV4RRFFQ69G5FAV" {
				t.Fatal("wrong notification issue", got)
			}
			if issue := m.jobs.byKey[jobKey{"7", j.UID}].issue; issue != got.UID {
				t.Fatal("job display omitted notification issue", issue)
			}
		})
	}
}

func TestCronFilterEscapeClearsQuery(t *testing.T) {
	for _, tab := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(tab), func(t *testing.T) {
			m := newModel(Options{})
			m.SetData(nativeFixture())
			m.SelectTab(tab)
			m.routeKey(press("/"))
			m.routeKey(press("no-match"))
			if len(m.rows) != 0 {
				t.Fatal("filter did not apply")
			}
			m.routeKey(press("esc"))
			if m.query != "" || m.searching || len(m.rows) == 0 {
				t.Fatal("Escape did not clear filter")
			}
		})
	}
}

func TestCronDeleteDoesNotCloseAnotherJobDetail(t *testing.T) {
	d := nativeFixture()
	second := d.Jobs[0]
	second.UID, second.Name = "job-2", "Second job"
	d.Jobs = append(d.Jobs, second)
	m := newModel(Options{Services: map[string]Service{"7": monitorActionService{}}})
	m.SetData(d)
	m.jobKeyAction("D")
	pending := m.confirmJobKey("y")
	m.pushDetail(viewJob, "7", second.UID)
	m.handleFormMessage(pending())
	current := m.nav[len(m.nav)-1]
	if current.view != viewJob || current.projectUID != "7" || current.uid != second.UID {
		t.Fatal("delete completion closed another job detail", current)
	}
}

func TestCronExistingDuplicateNameCanBeRetained(t *testing.T) {
	j := formTestJob()
	j.ProjectUID, j.Name = "7", "Example job"
	duplicate := j
	duplicate.UID, duplicate.Name = "other-job", "example job"
	m := newModel(Options{})
	m.SetData(Data{Jobs: []records.Job{j, duplicate}})
	f := newJobForm(t.Context(), nil, &j, nil, m.editorContext("7"))
	f.Set("prompt", "Changed prompt")
	body, err := f.definition()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.validate(body); err != nil {
		t.Fatal("unchanged valid name blocked unrelated edit", err)
	}
	f.Set("name", duplicate.Name)
	if err := f.validate(body); err == nil {
		t.Fatal("actual rename bypassed existing name check")
	}
}

func TestCronJobDetailActionsFollowRecordAcrossTabs(t *testing.T) {
	for _, tab := range []int{0, 1, 2} {
		for _, action := range []string{"e", "p", "D"} {
			t.Run(fmt.Sprintf("tab=%d/action=%s", tab, action), func(t *testing.T) {
				m := newModel(Options{Services: map[string]Service{"7": monitorActionService{}}})
				m.SetData(nativeFixture())
				m.SelectTab(tab)
				m.open()
				switch tab {
				case 2:
					m.detailKey = detailItemKey("used-by", "7", "job-1")
					m.open()
				case 1:
					m.open()
				}
				if m.currentView() != viewJob {
					t.Fatal("navigation did not reach job detail")
				}
				cmd, _ := m.definitionKey(press(action))
				switch action {
				case "e":
					if _, ok := m.editor.(*jobForm); !ok {
						t.Fatal("job detail did not open job editor")
					}
				case "p":
					if cmd == nil {
						t.Fatal("job detail pause was ignored")
					}
					msg := cmd().(jobPausedMsg)
					if msg.project != "7" || msg.saved.UID != "job-1" {
						t.Fatal("pause targeted wrong job", msg)
					}
				case "D":
					if len(m.confirm) != 1 || m.confirm[0].UID != "job-1" {
						t.Fatal("job detail delete did not confirm selected job")
					}
				}
			})
		}
	}
}

func TestCronWorkflowEditorHonorsNativeEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, visual, editor string
		want                 []string
	}{
		{"visual", "example-visual --wait", "example-editor", []string{"example-visual", "--wait"}},
		{"editor", "", "example-editor --plain", []string{"example-editor", "--plain"}},
		{"blank-visual", "  ", "example-editor", []string{"example-editor"}},
		{"fallback", "", "", []string{"vi"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("VISUAL", tc.visual)
			t.Setenv("EDITOR", tc.editor)
			f, err := newWorkflowForm(t.Context(), monitorWorkflowWriter{}, nativeFixture().Workflows[0])
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = f.Close() })
			want := append(append([]string(nil), tc.want...), f.path)
			if got := f.editorCommand().Args; !reflect.DeepEqual(got, want) {
				t.Fatal("workflow editor command", got, "want", want)
			}
		})
	}
}
