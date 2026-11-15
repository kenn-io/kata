package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"pgregory.net/rapid"
)

// The explicit contract is that edit owns the revision and actor, keeps
// drafts on failure, and never writes until a conflict has been acknowledged.
type cronEditTestSession struct {
	calls int
	path  string
}

func cronEditTestEditor(t *testing.T, edit func(int, []byte) ([]byte, error)) *cronEditTestSession {
	t.Helper()
	previousEditor, previousTerminal := cronEditEditor, cronEditTerminal
	session := &cronEditTestSession{}
	cronEditTerminal = func(*cobra.Command) bool { return true }
	cronEditEditor = func(ctx context.Context, _ *cobra.Command, path string) error {
		require.NoError(t, ctx.Err())
		session.calls++
		require.LessOrEqual(t, session.calls, 4, "an unchanged invalid draft must not reopen forever")
		session.path = path
		info, err := os.Stat(path)
		require.NoError(t, err)
		if runtime.GOOS != "windows" {
			require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		}
		raw, err := os.ReadFile(path) //nolint:gosec // The test editor reads only the private draft created by edit.
		require.NoError(t, err)
		next, err := edit(session.calls, raw)
		if next != nil {
			require.NoError(t, os.WriteFile(path, next, 0o600))
		}
		return err
	}
	t.Cleanup(func() {
		cronEditEditor, cronEditTerminal = previousEditor, previousTerminal
		if session.path != "" {
			_ = os.Remove(session.path)
		}
	})
	return session
}

func cronEditTestDefinition(t *testing.T, resource string) (*testenv.Env, int64, string, string) {
	t.Helper()
	t.Setenv("KATA_AUTHOR", "worker")
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	id := cronCLIUID(t)
	options := jsontext.Value(`{"empty":{},"number":9007199254740993,"decimal":1.2300e+02,"text":"// keep"}`)
	if resource == "job" {
		definition := cronCLIJob(t).Definition.Native()
		definition.Options = options
		job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{UID: id, ProjectID: project.ID, Name: "Inspect", Definition: definition, Actor: "worker"})
		require.NoError(t, err)
		return env, project.ID, id, job.DefinitionEventUID
	}
	workflow, _, err := env.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{
		UID: id, ProjectID: project.ID, Name: "Inspect", Actor: "worker",
		Definition: cron.WorkflowDefinition{Version: 1, Options: options, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "git status", Options: jsontext.Value(`{}`)}}},
	})
	require.NoError(t, err)
	return env, project.ID, id, workflow.DefinitionEventUID
}

func cronEditTestRename(raw []byte, name string) []byte {
	body, _ := cronEditSplit(raw)
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(body, &fields); err != nil {
		panic(err)
	}
	fields["name"], _ = json.Marshal(name)
	result, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return result
}

func requireCronEditExit(t *testing.T, err error, code int) {
	t.Helper()
	ce, ok := errors.AsType[*cliError](err)
	require.True(t, ok, "expected CLI error, got %v", err)
	require.Equal(t, code, ce.ExitCode)
}

func cronEditTestStaleJob(t *testing.T, env *testenv.Env, project int64, id string) {
	t.Helper()
	job, err := env.DB.CronJob(t.Context(), project, id)
	require.NoError(t, err)
	job.Definition.Trigger = cron.Trigger{Kind: "cron", Cron: "0 0 30 2 *"}
	raw, err := json.Marshal(job.Definition)
	require.NoError(t, err)
	_, err = env.DB.ExecContext(t.Context(), `UPDATE cron_jobs SET definition_json=? WHERE uid=?`, string(raw), id)
	require.NoError(t, err)
}

func TestCronEditStaleJobRepair(t *testing.T) {
	for _, repair := range []bool{false, true} {
		t.Run(fmt.Sprintf("repair=%t", repair), func(t *testing.T) {
			env, project, id, original := cronEditTestDefinition(t, "job")
			cronEditTestStaleJob(t, env, project, id)
			session := cronEditTestEditor(t, func(_ int, raw []byte) ([]byte, error) {
				if repair {
					return bytes.ReplaceAll(raw, []byte("0 0 30 2 *"), []byte("0 0 28 2 *")), nil
				}
				return nil, nil
			})
			out, _, err := executeCronCLI(t, env.URL, nil, "job", "edit", id)
			if repair {
				require.NoError(t, err)
				require.Equal(t, 1, session.calls)
				require.Contains(t, out, "9007199254740993")
				require.Contains(t, out, "1.2300e+02")
			} else {
				requireCronEditExit(t, err, ExitValidation)
				require.Equal(t, 2, session.calls)
				require.ErrorContains(t, err, "never fires")
			}
			stored, err := env.DB.CronJob(t.Context(), project, id)
			require.NoError(t, err)
			if repair {
				require.NotEqual(t, original, stored.DefinitionEventUID)
				require.Equal(t, "0 0 28 2 *", stored.Definition.Trigger.Cron)
				require.Contains(t, string(stored.Definition.Options), `"empty":{}`)
			} else {
				require.Equal(t, original, stored.DefinitionEventUID)
			}
		})
	}
	// Stored-read tolerance must never swallow unrelated domain errors. These
	// rules precede occurrence validation, so removing tolerance does not fail
	// these cases; accepting every ErrInvalid does.
	for _, fault := range []string{"version", "overlap"} {
		t.Run(fault, func(t *testing.T) {
			raw := cronEditValidTestBody("job", jsontext.Value(`{}`))
			var envelope map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(raw, &envelope))
			var definition map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(envelope["definition"], &definition))
			definition["trigger"] = jsontext.Value(`{"kind":"cron","cron":"0 0 30 2 *"}`)
			if fault == "version" {
				definition["version"] = jsontext.Value(`2`)
			} else {
				definition["overlap"] = jsontext.Value(`"unsupported"`)
			}
			envelope["definition"], _ = json.Marshal(definition)
			raw, _ = json.Marshal(envelope)
			for _, stored := range []bool{false, true} {
				_, err := parseCronEdit("job", raw, stored)
				require.ErrorContains(t, err, fault)
			}
		})
	}
}

func TestCronEditSavesDefinitions(t *testing.T) {
	for _, resource := range []string{"job", "workflow"} {
		t.Run(resource, func(t *testing.T) {
			env, project, id, original := cronEditTestDefinition(t, resource)
			var originalStepOptions jsontext.Value
			if resource == "workflow" {
				before, err := env.DB.CronWorkflow(t.Context(), project, id)
				require.NoError(t, err)
				originalStepOptions = before.Definition.Steps[0].Options.Clone()
			}
			session := cronEditTestEditor(t, func(_ int, raw []byte) ([]byte, error) {
				require.Contains(t, string(raw), original)
				return cronEditTestRename(raw, "  Updated inspection  "), nil
			})
			out, _, err := executeCronCLI(t, env.URL, nil, resource, "edit", strings.ToLower(id))
			require.NoError(t, err)
			require.Equal(t, 1, session.calls)
			require.Contains(t, out, "9007199254740993")
			require.Contains(t, out, "1.2300e+02")
			if resource == "job" {
				job, err := env.DB.CronJob(t.Context(), project, id)
				require.NoError(t, err)
				require.Equal(t, "Updated inspection", job.Name)
				require.NotEqual(t, original, job.DefinitionEventUID)
				require.Contains(t, string(job.Definition.Options), `"empty":{}`)
			} else {
				workflow, err := env.DB.CronWorkflow(t.Context(), project, id)
				require.NoError(t, err)
				require.Equal(t, "Updated inspection", workflow.Name)
				require.NotEqual(t, original, workflow.DefinitionEventUID)
				require.Equal(t, originalStepOptions, workflow.Definition.Steps[0].Options)
				require.Contains(t, string(workflow.Definition.Options), `"empty":{}`)
			}
			require.NoFileExists(t, session.path)
		})
	}
}

func TestCronEditAbortsWithoutEvents(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprint(empty), func(t *testing.T) {
			env, _, id, _ := cronEditTestDefinition(t, "job")
			before, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			session := cronEditTestEditor(t, func(_ int, _ []byte) ([]byte, error) {
				if empty {
					return []byte("// abort\n \n"), nil
				}
				return nil, nil
			})
			out, diagnostic, err := executeCronCLI(t, env.URL, nil, "job", "edit", id)
			require.NoError(t, err)
			require.Empty(t, out)
			if empty {
				require.Contains(t, diagnostic, "aborted")
			} else {
				require.Contains(t, diagnostic, "no changes")
			}
			after, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.NoFileExists(t, session.path)
		})
	}
}

func TestCronEditNeedsTerminalBeforeNetwork(t *testing.T) {
	previous := cronEditTerminal
	cronEditTerminal = func(*cobra.Command) bool { return false }
	t.Cleanup(func() { cronEditTerminal = previous })
	_, _, err := executeCronCLI(t, "http://127.0.0.1:1", nil, "job", "edit", cronCLIUID(t))
	requireCronEditExit(t, err, ExitValidation)
	require.ErrorContains(t, err, "terminal")
	require.ErrorContains(t, err, "update --file")
}

func TestCronEditValidationReopensAndStopsOrRepairs(t *testing.T) {
	for _, repair := range []bool{false, true} {
		t.Run(fmt.Sprint(repair), func(t *testing.T) {
			env, project, id, original := cronEditTestDefinition(t, "job")
			var good []byte
			session := cronEditTestEditor(t, func(call int, raw []byte) ([]byte, error) {
				if call == 1 {
					good = cronEditTestRename(raw, "Updated inspection")
					return bytes.Replace(good, []byte(`"version":1`), []byte(`"version":99`), 1), nil
				}
				require.Contains(t, string(raw), "// error:")
				if repair {
					return good, nil
				}
				return nil, nil
			})
			_, diagnostic, err := executeCronCLI(t, env.URL, nil, "job", "edit", id)
			if repair {
				require.NoError(t, err)
				require.NoFileExists(t, session.path)
			} else {
				requireCronEditExit(t, err, ExitValidation)
				require.FileExists(t, session.path)
				require.Contains(t, diagnostic, "draft kept: "+session.path)
				job, readErr := env.DB.CronJob(t.Context(), project, id)
				require.NoError(t, readErr)
				require.Equal(t, original, job.DefinitionEventUID)
			}
			require.Equal(t, 2, session.calls)
		})
	}
}

func TestCronEditDaemonValidationStopsUnchangedRetry(t *testing.T) {
	env, project, id, original := cronEditTestDefinition(t, "job")
	missing := cronCLIUID(t)
	session := cronEditTestEditor(t, func(call int, raw []byte) ([]byte, error) {
		if call == 1 {
			body := cronEditTestRename(raw, "Needs workflow")
			return bytes.Replace(body, []byte(`"action":{"kind":"execute","prompt":"Inspect the workspace"}`), []byte(`"action":{"kind":"execute","workflow_uid":"`+missing+`"}`), 1), nil
		}
		require.Contains(t, string(raw), "// error:")
		require.Contains(t, string(raw), "workflow")
		return nil, nil
	})
	_, _, err := executeCronCLI(t, env.URL, nil, "job", "edit", id)
	requireCronEditExit(t, err, ExitValidation)
	require.Equal(t, 2, session.calls)
	require.FileExists(t, session.path)
	job, err := env.DB.CronJob(t.Context(), project, id)
	require.NoError(t, err)
	require.Equal(t, original, job.DefinitionEventUID)
}

func TestCronEditConflictsRequireAcknowledgment(t *testing.T) {
	for _, accept := range []bool{false, true} {
		t.Run(fmt.Sprint(accept), func(t *testing.T) {
			env, project, id, original := cronEditTestDefinition(t, "job")
			var currentEvent string
			session := cronEditTestEditor(t, func(call int, raw []byte) ([]byte, error) {
				if call == 1 {
					job, err := env.DB.CronJob(t.Context(), project, id)
					require.NoError(t, err)
					current, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{UID: id, ProjectID: project, Name: "Other writer", Definition: job.Definition, ExpectedEventUID: original, Actor: "other"})
					require.NoError(t, err)
					currentEvent = current.DefinitionEventUID
					return cronEditTestRename(raw, "My draft"), nil
				}
				require.Contains(t, string(raw), "// CONFLICT:")
				require.Contains(t, string(raw), "Other writer")
				require.Contains(t, string(raw), currentEvent)
				require.Contains(t, string(raw), "My draft")
				if accept {
					lines := bytes.Split(raw, []byte("\n"))
					for i, line := range lines {
						if bytes.HasPrefix(line, []byte("// CONFLICT:")) {
							lines[i] = []byte("// acknowledged")
						}
					}
					return bytes.Join(lines, []byte("\n")), nil
				}
				return nil, nil
			})
			_, diagnostic, err := executeCronCLI(t, env.URL, nil, "job", "edit", id)
			job, readErr := env.DB.CronJob(t.Context(), project, id)
			require.NoError(t, readErr)
			if accept {
				require.NoError(t, err)
				require.Equal(t, "My draft", job.Name)
				require.NotEqual(t, currentEvent, job.DefinitionEventUID)
				require.NoFileExists(t, session.path)
			} else {
				requireCronEditExit(t, err, ExitConflict)
				require.Equal(t, "Other writer", job.Name)
				require.Equal(t, currentEvent, job.DefinitionEventUID)
				require.Contains(t, diagnostic, "draft kept: "+session.path)
			}
			require.Equal(t, 2, session.calls)
		})
	}
}

func TestCronEditRecognizesEqualLiveCurrent(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprint(deleted), func(t *testing.T) {
			env, project, id, original := cronEditTestDefinition(t, "workflow")
			session := cronEditTestEditor(t, func(_ int, raw []byte) ([]byte, error) {
				workflow, err := env.DB.CronWorkflow(t.Context(), project, id)
				require.NoError(t, err)
				_, _, err = env.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{UID: id, ProjectID: project, Name: "My draft", Definition: workflow.Definition, ExpectedEventUID: original, Actor: "other", Deleted: deleted})
				require.NoError(t, err)
				return cronEditTestRename(raw, "My draft"), nil
			})
			out, _, err := executeCronCLI(t, env.URL, nil, "workflow", "edit", id)
			if deleted {
				requireCronEditExit(t, err, ExitConflict)
				require.FileExists(t, session.path)
			} else {
				require.NoError(t, err)
				require.Contains(t, out, "My draft")
				require.NoFileExists(t, session.path)
			}
			require.Equal(t, 1, session.calls)
		})
	}
}

func TestCronEditRetainsDraftOnEditorFailure(t *testing.T) {
	env, _, id, _ := cronEditTestDefinition(t, "job")
	session := cronEditTestEditor(t, func(_ int, raw []byte) ([]byte, error) {
		return cronEditTestRename(raw, "Unsaved draft"), errors.New("editor failed")
	})
	_, diagnostic, err := executeCronCLI(t, env.URL, nil, "job", "edit", id)
	requireCronEditExit(t, err, ExitValidation)
	require.Contains(t, diagnostic, "draft kept: "+session.path)
	raw, err := os.ReadFile(session.path)
	require.NoError(t, err)
	require.Contains(t, string(raw), "Unsaved draft")
}

func TestCronEditBufferRoundTripProperty(t *testing.T) {
	// Rapid configures case counts per process; keep this test's existing budget.
	previousChecks := flag.Lookup("rapid.checks").Value.String()
	require.NoError(t, flag.Set("rapid.checks", "1000"))
	defer func() { require.NoError(t, flag.Set("rapid.checks", previousChecks)) }()
	rapid.Check(t, func(rt *rapid.T) {
		text := rapid.String().Draw(rt, "text")
		body, err := json.Marshal(map[string]any{"name": text, "definition": map[string]any{"value": text, "number": jsontext.Value(`9007199254740993`)}})
		if err != nil {
			rt.Fatal(err)
		}
		for _, resource := range []string{"job", "workflow"} {
			buffer := cronEditBuffer(resource, "01J00000000000000000000001", "01J00000000000000000000002", body, []string{"error: example\n// nested"})
			got, conflict := cronEditSplit(buffer)
			if !bytes.Equal(got, body) || conflict {
				rt.Fatalf("buffer changed body: %q != %q (conflict %v)", got, body, conflict)
			}
		}
	})
}

func TestCronEditHeaderStripsOnlyLeadingComments(t *testing.T) {
	raw := []byte("// header\n{\n// not a header\n\"text\":\"// literal\"\n}")
	body, conflict := cronEditSplit(raw)
	require.False(t, conflict)
	require.Equal(t, "{\n// not a header\n\"text\":\"// literal\"\n}", string(body))
}

func TestCronEditUnknownFieldsProperty(t *testing.T) {
	previousChecks := flag.Lookup("rapid.checks").Value.String()
	require.NoError(t, flag.Set("rapid.checks", "1000"))
	defer func() { require.NoError(t, flag.Set("rapid.checks", previousChecks)) }()
	rapid.Check(t, func(rt *rapid.T) {
		field := "extra_" + rapid.String().Draw(rt, "text")
		for _, resource := range []string{"job", "workflow"} {
			definition := jsontext.Value(`{"version":1,"steps":[{"key":"inspect","kind":"command","command":"git status"}]}`)
			if resource == "job" {
				definition = jsontext.Value(`{"version":1,"kind":"job","enabled":false,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"all"}`)
			}
			raw, err := json.Marshal(map[string]any{"name": "Inspection", "definition": definition, field: true})
			if err != nil {
				rt.Fatal(err)
			}
			if _, err := cronEditParse(resource, raw); err == nil {
				rt.Fatalf("%s accepted unknown field %q", resource, field)
			}
		}
	})
}

func TestCronEditNormalizationPreservesNumberTokens(t *testing.T) {
	require.True(t, cronEditEqual([]byte(`{"a":{},"n":9007199254740993}`), []byte("{\n\"n\":9007199254740993,\"a\":{}\n}")))
	require.False(t, cronEditEqual([]byte(`{"n":9007199254740993}`), []byte(`{"n":9007199254740992}`)))
	require.False(t, cronEditEqual([]byte(`{"n":1.2300e+02}`), []byte(`{"n":123}`)))
	for _, resource := range []string{"job", "workflow"} {
		for _, key := range []string{"actor", "expected_event_uid"} {
			_, err := cronEditParse(resource, []byte(`{"name":"Inspection","definition":{},"`+key+`":"worker"}`))
			require.ErrorContains(t, err, key)
		}
	}
}

func TestCronEditLargeIndentedDocumentAndNameBytes(t *testing.T) {
	definition := `{"version":1,"steps":[{"key":"inspect","kind":"command","command":"git status"}],"options":{"values":[` + strings.Repeat("0,", 39999) + `0]}}`
	raw := jsontext.Value(`{"name":"Inspection","definition":` + definition + `}`)
	require.NoError(t, raw.Indent(jsontext.WithIndent("  ")))
	require.Greater(t, len(raw), cron.DefinitionLimit)
	fields, err := cronEditParse("workflow", raw)
	require.NoError(t, err)
	require.Contains(t, string(fields["definition"]), strings.Repeat("0,", 100))
	for _, count := range []int{128, 129} {
		name, err := json.Marshal("  " + strings.Repeat("é", count) + "  ")
		require.NoError(t, err)
		body := []byte(`{"name":` + string(name) + `,"definition":{"version":1,"steps":[{"key":"inspect","kind":"command","command":"git status"}]}}`)
		_, err = cronEditParse("workflow", body)
		if count == 128 {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
	}
}

func TestCronEditLineDiff(t *testing.T) {
	require.Empty(t, cronLineDiff([]string{"same"}, []string{"same"}))
	require.Equal(t, []string{"- old", "+ new"}, cronLineDiff([]string{"before", "old", "after"}, []string{"before", "new", "after"}))
	large := strings.Split(strings.Repeat("old\n", 40000), "\n")
	other := strings.Split(strings.Repeat("new\n", 40000), "\n")
	diff := cronLineDiff(large, other)
	require.LessOrEqual(t, len(diff), 80)
	require.Contains(t, strings.Join(diff, "\n"), "more lines")
}

func TestCronEditLineDiffAccountsForChangesProperty(t *testing.T) {
	previousChecks := flag.Lookup("rapid.checks").Value.String()
	require.NoError(t, flag.Set("rapid.checks", "1000"))
	defer func() { require.NoError(t, flag.Set("rapid.checks", previousChecks)) }()
	rapid.Check(t, func(rt *rapid.T) {
		a := rapid.SliceOf(rapid.String()).Draw(rt, "lines")
		b := rapid.SliceOf(rapid.String()).Draw(rt, "lines")
		// Materialize small complete diffs; the separate scale probe covers
		// the intentionally truncated large-document path.
		for _, lines := range []*[]string{&a, &b} {
			if len(*lines) > 12 {
				*lines = (*lines)[:12]
			}
			for i, line := range *lines {
				text := []rune(line)
				if len(text) > 64 {
					text = text[:64]
				}
				(*lines)[i] = string(text)
			}
		}
		balance := map[string]int{}
		for _, line := range a {
			balance[line]++
		}
		for _, line := range b {
			balance[line]--
		}
		diff := cronLineDiff(a, b)
		// Independent exhaustive subsequence oracle for the small LCS path.
		best := 0
		for mask := 0; mask < 1<<len(a); mask++ {
			j, matched := 0, 0
			for i, line := range a {
				if mask&(1<<i) == 0 {
					continue
				}
				for j < len(b) && b[j] != line {
					j++
				}
				if j == len(b) {
					matched = -1
					break
				}
				j++
				matched++
			}
			best = max(best, matched)
		}
		if len(diff) != len(a)+len(b)-2*best {
			rt.Fatalf("nonminimal small diff: %d lines, LCS %d", len(diff), best)
		}
		for _, line := range diff {
			switch {
			case strings.HasPrefix(line, "- "):
				balance[line[2:]]--
			case strings.HasPrefix(line, "+ "):
				balance[line[2:]]++
			default:
				rt.Fatalf("unexpected diff line %q", line)
			}
		}
		for line, remaining := range balance {
			if remaining != 0 {
				rt.Fatalf("diff lost change %q: balance %d", line, remaining)
			}
		}
	})
}

// A PUT failure is deliberately not retried. The saved header is the CAS
// evidence needed to recover manually, even if a later writer moves state on.
func TestCronEditTransportFailureKeepsOriginalCAS(t *testing.T) {
	env, project, id, original := cronEditTestDefinition(t, "job")
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"code":"unavailable","message":"try later"}}`)
			return
		}
		request, err := http.NewRequestWithContext(r.Context(), r.Method, env.URL+r.URL.RequestURI(), r.Body) //nolint:gosec // URL is the owned fixture daemon plus the same relative path.
		require.NoError(t, err)
		response, err := env.HTTP.Do(request) //nolint:gosec // Request is restricted to the owned fixture daemon.
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	t.Cleanup(proxy.Close)
	session := cronEditTestEditor(t, func(_ int, raw []byte) ([]byte, error) {
		return cronEditTestRename(raw, "Unsaved draft"), nil
	})
	_, diagnostic, err := executeCronCLI(t, proxy.URL, nil, "job", "edit", id)
	require.Error(t, err)
	require.Equal(t, 1, session.calls)
	require.Contains(t, diagnostic, original)
	raw, err := os.ReadFile(session.path)
	require.NoError(t, err)
	require.Contains(t, string(raw), original)
	require.Contains(t, string(raw), "Unsaved draft")
	job, err := env.DB.CronJob(t.Context(), project, id)
	require.NoError(t, err)
	require.Equal(t, original, job.DefinitionEventUID)
}

func TestCronEditOwnsRequestFlags(t *testing.T) {
	for _, resource := range []string{"job", "workflow"} {
		command := newCronOperationCmd(resource, "edit")
		for _, flag := range []string{"file", "expected-event-uid", "parent"} {
			require.Nil(t, command.Flags().Lookup(flag))
		}
	}
}

func TestCronEditConflictMarkerWhitespaceProperty(t *testing.T) {
	previousChecks := flag.Lookup("rapid.checks").Value.String()
	require.NoError(t, flag.Set("rapid.checks", "100"))
	defer func() { require.NoError(t, flag.Set("rapid.checks", previousChecks)) }()
	rapid.Check(t, func(rt *rapid.T) {
		padding := strings.Repeat(" ", rapid.IntRange(1, 20).Draw(rt, "padding"))
		body, marked := cronEditSplit([]byte("// header\n//" + padding + strings.TrimPrefix(cronEditConflictMarker, "// ") + padding + "\r\n{}"))
		if !marked || string(body) != "{}" {
			rt.Fatalf("whitespace acknowledged conflict: %q, marked=%v", body, marked)
		}
	})
}

func TestCronEditRecoveryWriteReplacesDraftPrivately(t *testing.T) {
	path := t.TempDir() + "/draft.json"
	backup := path + ".backup"
	require.NoError(t, os.WriteFile(path, []byte("complete original draft"), 0o600))
	require.NoError(t, os.Link(path, backup))
	require.NoError(t, os.Chmod(path, 0o666)) //nolint:gosec // The permissive fixture checks that replacement restores private mode.
	require.NoError(t, writeCronEditFile(t.Context(), path, []byte("new header and draft")))
	raw, err := os.ReadFile(backup) //nolint:gosec // The backup is a hard link inside t.TempDir.
	require.NoError(t, err)
	require.Equal(t, "complete original draft", string(raw), "rewriting must replace the draft rather than truncate its inode")
	info, err := os.Stat(path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}

func TestCronEditOversizedDraftIsNotTruncated(t *testing.T) {
	path := t.TempDir() + "/draft.json"
	body := []byte(strings.Repeat("x", cronDefinitionRequestLimit+64*1024+1))
	require.NoError(t, os.WriteFile(path, body, 0o600))
	_, err := readCronEditFile(t.Context(), path)
	require.Error(t, err)
	got, err := os.ReadFile(path) //nolint:gosec // The draft path is inside t.TempDir.
	require.NoError(t, err)
	require.Equal(t, body, got)
}

func TestCronEditEachConflictNeedsAcknowledgment(t *testing.T) {
	for _, resource := range []string{"job", "workflow"} {
		t.Run(resource, func(t *testing.T) {
			env, project, id, _ := cronEditTestDefinition(t, resource)
			var latest string
			session := cronEditTestEditor(t, func(call int, raw []byte) ([]byte, error) {
				if call == 3 {
					require.Contains(t, string(raw), cronEditConflictMarker)
					require.Contains(t, string(raw), latest)
					return nil, nil
				}
				if resource == "job" {
					current, err := env.DB.CronJob(t.Context(), project, id)
					require.NoError(t, err)
					next, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{UID: id, ProjectID: project, Name: fmt.Sprintf("Writer %d", call), Definition: current.Definition, ExpectedEventUID: current.DefinitionEventUID, Actor: "other"})
					require.NoError(t, err)
					latest = next.DefinitionEventUID
				} else {
					current, err := env.DB.CronWorkflow(t.Context(), project, id)
					require.NoError(t, err)
					next, _, err := env.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{UID: id, ProjectID: project, Name: fmt.Sprintf("Writer %d", call), Definition: current.Definition, ExpectedEventUID: current.DefinitionEventUID, Actor: "other"})
					require.NoError(t, err)
					latest = next.DefinitionEventUID
				}
				return cronEditTestRename(raw, "My draft"), nil // Removes the first conflict header on call 2.
			})
			_, _, err := executeCronCLI(t, env.URL, nil, resource, "edit", id)
			requireCronEditExit(t, err, ExitConflict)
			require.Equal(t, 3, session.calls)
			require.FileExists(t, session.path)
		})
	}
}

func TestCronEditAcceptedSaveOutputFailureKeepsDraft(t *testing.T) {
	env, project, id, original := cronEditTestDefinition(t, "job")
	session := cronEditTestEditor(t, func(_ int, raw []byte) ([]byte, error) { return cronEditTestRename(raw, "Saved inspection"), nil })
	resetFlags(t)
	command := newRootCmd()
	var diagnostic bytes.Buffer
	command.SetOut(cronEditFailWriter{})
	command.SetErr(&diagnostic)
	command.SetContext(contextWithBaseURL(t.Context(), env.URL))
	command.SetArgs([]string{"--project", "spoke-project", "--json", "cron", "job", "edit", id})
	require.Error(t, command.Execute())
	require.Contains(t, diagnostic.String(), "definition saved")
	require.Contains(t, diagnostic.String(), "draft kept:")
	require.FileExists(t, session.path)
	job, err := env.DB.CronJob(t.Context(), project, id)
	require.NoError(t, err)
	require.Equal(t, "Saved inspection", job.Name)
	require.NotEqual(t, original, job.DefinitionEventUID)
}

type cronEditFailWriter struct{}

func (cronEditFailWriter) Write([]byte) (int, error) { return 0, errors.New("output unavailable") }

func TestCronEditPartialHeaderWritePreservesDraftProperty(t *testing.T) {
	previousChecks := flag.Lookup("rapid.checks").Value.String()
	require.NoError(t, flag.Set("rapid.checks", "100"))
	defer func() { require.NoError(t, flag.Set("rapid.checks", previousChecks)) }()
	rapid.Check(t, func(rt *rapid.T) {
		body := []byte("draft " + rapid.String().Draw(rt, "text"))
		path := t.TempDir() + "/draft.json"
		require.NoError(rt, os.WriteFile(path, body, 0o600))
		original := cronEditWriteTemp
		defer func() { cronEditWriteTemp = original }()
		cronEditWriteTemp = func(file *os.File, raw []byte) (int, error) {
			n, err := file.Write(raw[:len(raw)/2])
			if err != nil {
				return n, err
			}
			return n, io.ErrShortWrite
		}
		if err := writeCronEditFile(t.Context(), path, append([]byte("// recovery\n"), body...)); err == nil {
			rt.Fatal("partial header write succeeded")
		}
		got, err := os.ReadFile(path) //nolint:gosec // The draft path is inside t.TempDir.
		if err != nil || !bytes.Equal(body, got) {
			rt.Fatalf("partial header write lost draft: %q != %q (%v)", got, body, err)
		}
	})
}

func cronEditValidTestBody(resource string, options jsontext.Value) []byte {
	definition := jsontext.Value(`{"version":1,"steps":[{"key":"inspect","kind":"command","command":"git status"}]}`)
	if resource == "job" {
		definition = jsontext.Value(`{"version":1,"kind":"job","enabled":false,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"all"}`)
	}
	var fields map[string]jsontext.Value
	_ = json.Unmarshal(definition, &fields)
	fields["options"] = options
	definition, _ = json.Marshal(fields)
	body, _ := json.Marshal(map[string]any{"name": "Inspection", "definition": definition})
	return body
}

func TestCronEditValidOpaqueRoundTripProperty(t *testing.T) {
	previousChecks := flag.Lookup("rapid.checks").Value.String()
	require.NoError(t, flag.Set("rapid.checks", "1000"))
	defer func() { require.NoError(t, flag.Set("rapid.checks", previousChecks)) }()
	tokens := []string{"9007199254740993", "12345678901234567890123", "-0", "1.0", "1e0", "1E+00", "1e308", "1e-9999", "1e9999"}
	rapid.Check(t, func(rt *rapid.T) {
		text := rapid.String().Draw(rt, "text")
		token := rapid.SampledFrom(tokens).Draw(rt, "number token")
		options, err := json.Marshal(map[string]any{"text": text, "empty": jsontext.Value(`{}`), "values": jsontext.Value(`[null,[],{},true,` + token + `]`)})
		require.NoError(rt, err)
		for _, resource := range []string{"job", "workflow"} {
			body := cronEditValidTestBody(resource, options)
			rendered := cronEditBuffer(resource, "example", "revision", cronEditPretty(body), nil)
			stripped, _ := cronEditSplit(rendered)
			parsed, err := cronEditParse(resource, stripped)
			// Existing domain validation decodes options into float64 for its
			// portable-field checks, and rejects overflow. Edit shares that rule.
			if token == "1e9999" {
				require.Error(rt, err)
				continue
			}
			require.NoError(rt, err)
			var definition map[string]jsontext.Value
			require.NoError(rt, json.Unmarshal(parsed["definition"], &definition))
			expected, actual := jsontext.Value(options).Clone(), definition["options"].Clone()
			opts := []jsontext.Options{jsontext.CanonicalizeRawInts(false), jsontext.CanonicalizeRawFloats(false)}
			require.NoError(rt, expected.Canonicalize(opts...))
			require.NoError(rt, actual.Canonicalize(opts...))
			require.Equal(rt, expected, actual)
			normalized, err := json.Marshal(parsed)
			require.NoError(rt, err)
			second, err := cronEditParse(resource, normalized)
			require.NoError(rt, err)
			require.Equal(rt, parsed, second)
		}
	})
}

func TestCronEditRejectsMalformedEnvelopes(t *testing.T) {
	for _, resource := range []string{"job", "workflow"} {
		good := cronEditValidTestBody(resource, jsontext.Value(`{}`))
		_, err := cronEditParse(resource, good)
		require.NoError(t, err)
		var valid map[string]jsontext.Value
		require.NoError(t, json.Unmarshal(good, &valid))
		for _, name := range []string{"null", "4", `"x","name":"y"`} {
			raw := []byte(`{"name":` + name + `,"definition":` + string(valid["definition"]) + `}`)
			_, err := cronEditParse(resource, raw)
			require.Error(t, err, "%s accepted malformed name %s", resource, name)
		}
		for _, raw := range []string{"null", "[]", "true", string(good) + "{}", string(good[:len(good)-1]), `{"name":"x"}`} {
			_, err := cronEditParse(resource, []byte(raw))
			require.Error(t, err, "%s accepted %s", resource, raw)
		}
		for _, key := range []string{"actor", "expected_event_uid"} {
			for _, value := range []string{"null", `""`, `"worker"`} {
				raw := append(bytes.Clone(good[:len(good)-1]), []byte(`,"`+key+`":`+value+`}`)...)
				_, err := cronEditParse(resource, raw)
				require.Error(t, err)
			}
		}
	}
}

func TestCronEditTrimmedNameBytesProperty(t *testing.T) {
	previousChecks := flag.Lookup("rapid.checks").Value.String()
	require.NoError(t, flag.Set("rapid.checks", "1000"))
	defer func() { require.NoError(t, flag.Set("rapid.checks", previousChecks)) }()
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(0, 257).Draw(rt, "name bytes")
		name := strings.Repeat("é", n/2) + strings.Repeat("a", n%2)
		for _, resource := range []string{"job", "workflow"} {
			body := cronEditTestRename(cronEditValidTestBody(resource, jsontext.Value(`{}`)), "\u2003"+name+"\u2003")
			parsed, err := cronEditParse(resource, body)
			if n == 0 || n > 256 {
				require.Error(rt, err)
				continue
			}
			require.NoError(rt, err)
			var got string
			require.NoError(rt, json.Unmarshal(parsed["name"], &got))
			require.Equal(rt, name, got)
		}
	})
}

func TestCronEditBodyAndHeaderLimits(t *testing.T) {
	for _, resource := range []string{"job", "workflow"} {
		body := cronEditValidTestBody(resource, jsontext.Value(`{}`))
		for _, extra := range []int{-1, 0, 1} {
			padded := append(bytes.Clone(body), bytes.Repeat([]byte(" "), cronDefinitionRequestLimit-len(body)+extra)...)
			_, err := cronEditParse(resource, padded)
			if extra > 0 {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		}
		notes := strings.Split(strings.Repeat(strings.Repeat("é", 300)+"\n", 200), "\n")
		buffer := cronEditBuffer(resource, "example", "revision", body, notes)
		require.LessOrEqual(t, len(buffer)-len(body), 32*1024)
		stripped, _ := cronEditSplit(buffer)
		require.Equal(t, body, stripped)
		for line := range strings.SplitSeq(string(buffer[:len(buffer)-len(body)]), "\n") {
			require.LessOrEqual(t, len(strings.TrimPrefix(line, "// ")), 384)
			require.True(t, strings.ToValidUTF8(line, "") == line)
		}
	}
}

func cronEditTestProxy(t *testing.T, env *testenv.Env, transform func(*http.Request, int, []byte) (int, []byte)) *httptest.Server {
	t.Helper()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request, err := http.NewRequestWithContext(r.Context(), r.Method, env.URL+r.URL.RequestURI(), r.Body) //nolint:gosec // Owned fixture origin and its relative route.
		require.NoError(t, err)
		request.Header = r.Header.Clone()
		request.Header.Del("Accept-Encoding") // Let the fixture transport decompress before response rewriting.
		response, err := env.HTTP.Do(request) //nolint:gosec // Owned fixture origin.
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		raw, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		status, raw := transform(r, response.StatusCode, raw)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(raw)
	}))
	t.Cleanup(proxy.Close)
	return proxy
}

func TestCronEditInvalidInitialSnapshotNeverOpensEditor(t *testing.T) {
	for _, resource := range []string{"job", "workflow"} {
		for _, event := range []string{"", "invalid"} {
			t.Run(resource+event, func(t *testing.T) {
				env, _, id, _ := cronEditTestDefinition(t, resource)
				proxy := cronEditTestProxy(t, env, func(r *http.Request, status int, raw []byte) (int, []byte) {
					require.NotEqual(t, http.MethodPut, r.Method)
					if strings.HasSuffix(r.URL.Path, "/"+id) {
						var envelope map[string]map[string]jsontext.Value
						require.NoError(t, json.Unmarshal(raw, &envelope))
						envelope[resource]["definition_event_uid"], _ = json.Marshal(event)
						raw, _ = json.Marshal(envelope)
					}
					return status, raw
				})
				session := cronEditTestEditor(t, func(_ int, _ []byte) ([]byte, error) { t.Fatal("invalid authority opened editor"); return nil, nil })
				_, _, err := executeCronCLI(t, proxy.URL, nil, resource, "edit", id)
				require.Error(t, err)
				require.Zero(t, session.calls)
			})
		}
	}
}

func TestCronEditConflictFallsBackToShow(t *testing.T) {
	for _, resource := range []string{"job", "workflow"} {
		t.Run(resource, func(t *testing.T) {
			env, project, id, original := cronEditTestDefinition(t, resource)
			var current string
			puts := 0
			proxy := cronEditTestProxy(t, env, func(r *http.Request, status int, raw []byte) (int, []byte) {
				if r.Method == http.MethodPut {
					puts++
					if status == http.StatusConflict {
						return status, []byte(`{"error":{"code":"cron_conflict","message":"definition changed"}}`)
					}
				}
				return status, raw
			})
			session := cronEditTestEditor(t, func(call int, raw []byte) ([]byte, error) {
				if call == 1 {
					if resource == "job" {
						job, err := env.DB.CronJob(t.Context(), project, id)
						require.NoError(t, err)
						next, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{UID: id, ProjectID: project, Name: "Other writer", Definition: job.Definition, ExpectedEventUID: original, Actor: "other"})
						require.NoError(t, err)
						current = next.DefinitionEventUID
						cronEditTestStaleJob(t, env, project, id)
					} else {
						workflow, err := env.DB.CronWorkflow(t.Context(), project, id)
						require.NoError(t, err)
						next, _, err := env.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{UID: id, ProjectID: project, Name: "Other writer", Definition: workflow.Definition, ExpectedEventUID: original, Actor: "other"})
						require.NoError(t, err)
						current = next.DefinitionEventUID
					}
				} else {
					require.Contains(t, string(raw), current)
					require.Contains(t, string(raw), cronEditConflictMarker)
				}
				return cronEditTestRename(raw, "My draft"), nil
			})
			out, _, err := executeCronCLI(t, proxy.URL, nil, resource, "edit", id, "--as", "editor")
			require.NoError(t, err)
			require.Equal(t, 2, session.calls)
			require.Equal(t, 2, puts)
			require.Contains(t, out, "My draft")
		})
	}
}

func TestCronEditInitiallyDeletedDefinitionIsRefused(t *testing.T) {
	for _, resource := range []string{"job", "workflow"} {
		t.Run(resource, func(t *testing.T) {
			env, project, id, original := cronEditTestDefinition(t, resource)
			if resource == "job" {
				job, err := env.DB.CronJob(t.Context(), project, id)
				require.NoError(t, err)
				_, _, err = env.DB.PutCronJob(t.Context(), db.PutCronJob{UID: id, ProjectID: project, Name: job.Name, Definition: job.Definition, ExpectedEventUID: original, Actor: "worker", Deleted: true})
				require.NoError(t, err)
			} else {
				workflow, err := env.DB.CronWorkflow(t.Context(), project, id)
				require.NoError(t, err)
				_, _, err = env.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{UID: id, ProjectID: project, Name: workflow.Name, Definition: workflow.Definition, ExpectedEventUID: original, Actor: "worker", Deleted: true})
				require.NoError(t, err)
			}
			session := cronEditTestEditor(t, func(_ int, _ []byte) ([]byte, error) { t.Fatal("deleted definition opened editor"); return nil, nil })
			_, _, err := executeCronCLI(t, env.URL, nil, resource, "edit", id)
			requireCronEditExit(t, err, ExitValidation)
			require.ErrorContains(t, err, "restore it first")
			require.Zero(t, session.calls)
		})
	}
}

func TestCronEditCancellationRetainsDraft(t *testing.T) {
	env, project, id, original := cronEditTestDefinition(t, "job")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	session := cronEditTestEditor(t, func(_ int, raw []byte) ([]byte, error) {
		cancel()
		return cronEditTestRename(raw, "Canceled draft"), nil
	})
	resetFlags(t)
	command := newRootCmd()
	var out, diagnostic bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&diagnostic)
	command.SetContext(contextWithBaseURL(ctx, env.URL))
	command.SetArgs([]string{"--project", "spoke-project", "--json", "cron", "job", "edit", id})
	require.ErrorIs(t, command.Execute(), context.Canceled)
	require.Contains(t, diagnostic.String(), "draft kept:")
	raw, err := os.ReadFile(session.path)
	require.NoError(t, err)
	require.Contains(t, string(raw), "Canceled draft")
	job, err := env.DB.CronJob(t.Context(), project, id)
	require.NoError(t, err)
	require.Equal(t, original, job.DefinitionEventUID)
}

func TestCronEditLineDiffOmissionCounts(t *testing.T) {
	for _, n := range []int{0, 1, 79, 80, 81, 500, 501} {
		a, b := make([]string, n), make([]string, n)
		for i := range a {
			a[i], b[i] = fmt.Sprintf("old%d", i), fmt.Sprintf("new%d", i)
		}
		diff := cronLineDiff(a, b)
		if 2*n <= 79 {
			require.Len(t, diff, 2*n)
		} else {
			require.Len(t, diff, 80)
			require.Equal(t, fmt.Sprintf("... %d more lines; see kata cron <kind> show", 2*n-79), diff[79])
		}
	}
}

func TestCronEditFailedConflictHeaderKeepsRecoveryCAS(t *testing.T) {
	env, project, id, original := cronEditTestDefinition(t, "job")
	writer := cronEditWriteTemp
	t.Cleanup(func() { cronEditWriteTemp = writer })
	cronEditWriteTemp = func(file *os.File, raw []byte) (int, error) {
		if bytes.Contains(raw, []byte(cronEditConflictMarker)) {
			return 0, io.ErrShortWrite
		}
		return file.Write(raw)
	}
	session := cronEditTestEditor(t, func(_ int, raw []byte) ([]byte, error) {
		job, err := env.DB.CronJob(t.Context(), project, id)
		require.NoError(t, err)
		_, _, err = env.DB.PutCronJob(t.Context(), db.PutCronJob{UID: id, ProjectID: project, Name: "Other writer", Definition: job.Definition, ExpectedEventUID: original, Actor: "other"})
		require.NoError(t, err)
		return cronEditTestRename(raw, "My draft"), nil
	})
	_, diagnostic, err := executeCronCLI(t, env.URL, nil, "job", "edit", id)
	require.ErrorIs(t, err, io.ErrShortWrite)
	require.Equal(t, 1, session.calls)
	raw, err := os.ReadFile(session.path)
	require.NoError(t, err)
	require.Contains(t, string(raw), "event "+original)
	require.Contains(t, diagnostic, "--expected-event-uid "+original)
}

func TestCronEditCompactDefinitionBoundary(t *testing.T) {
	for _, resource := range []string{"job", "workflow"} {
		var fields map[string]jsontext.Value
		require.NoError(t, json.Unmarshal(cronEditValidTestBody(resource, jsontext.Value(`{"value":""}`)), &fields))
		for _, extra := range []int{-1, 0, 1} {
			definition := bytes.Replace(fields["definition"], []byte(`"value":""`), []byte(`"value":"`+strings.Repeat("x", cron.DefinitionLimit-len(fields["definition"])+extra)+`"`), 1)
			require.Len(t, definition, cron.DefinitionLimit+extra)
			raw, err := json.Marshal(map[string]any{"name": "Inspection", "definition": jsontext.Value(definition)})
			require.NoError(t, err)
			_, err = cronEditParse(resource, raw)
			if extra > 0 {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		}
	}
}

func TestCronEditRedirectIsNotASavedDefinition(t *testing.T) {
	for _, resource := range []string{"job", "workflow"} {
		for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect} {
			t.Run(fmt.Sprintf("%s/%d", resource, status), func(t *testing.T) {
				env, project, id, original := cronEditTestDefinition(t, resource)
				puts := 0
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPut {
						puts++
						if status == http.StatusTemporaryRedirect {
							w.Header().Set("Location", env.URL+r.URL.RequestURI())
						}
						w.WriteHeader(status)
						_, _ = io.WriteString(w, "redirected")
						return
					}
					request, err := http.NewRequestWithContext(r.Context(), r.Method, env.URL+r.URL.RequestURI(), nil) //nolint:gosec // Owned fixture origin and route.
					require.NoError(t, err)
					response, err := env.HTTP.Do(request) //nolint:gosec // Owned fixture origin.
					require.NoError(t, err)
					defer func() { _ = response.Body.Close() }()
					w.WriteHeader(response.StatusCode)
					_, _ = io.Copy(w, response.Body)
				}))
				t.Cleanup(proxy.Close)
				session := cronEditTestEditor(t, func(_ int, raw []byte) ([]byte, error) { return cronEditTestRename(raw, "Unsaved draft"), nil })
				_, diagnostic, err := executeCronCLI(t, proxy.URL, nil, resource, "edit", id)
				require.Error(t, err)
				require.NotContains(t, diagnostic, "definition saved")
				require.Contains(t, diagnostic, "draft kept:")
				require.FileExists(t, session.path)
				require.Equal(t, 1, puts)
				if resource == "job" {
					current, err := env.DB.CronJob(t.Context(), project, id)
					require.NoError(t, err)
					require.Equal(t, original, current.DefinitionEventUID)
				} else {
					current, err := env.DB.CronWorkflow(t.Context(), project, id)
					require.NoError(t, err)
					require.Equal(t, original, current.DefinitionEventUID)
				}
			})
		}
	}
}
