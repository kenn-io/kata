package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/cron/conformance"
)

func executeConformanceCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	resetFlags(t)
	command := newRootCmd()
	var out, diagnostic bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&diagnostic)
	command.SetContext(t.Context())
	command.SetArgs(append([]string{"--json", "cron", "conformance"}, args...))
	err := command.Execute()
	return out.String(), err
}
func conformanceAnswers(t *testing.T, mutate func([]conformance.Answer) []conformance.Answer) string {
	t.Helper()
	d := conformance.Reference{}.Declare()
	suite, err := conformance.Suite(t.Context(), d)
	require.NoError(t, err)
	answers := []conformance.Answer{}
	for _, s := range suite.Scenarios {
		a, err := conformance.Execute(t.Context(), conformance.Reference{}, s)
		require.NoError(t, err)
		answers = append(answers, a)
	}
	if mutate != nil {
		answers = mutate(answers)
	}
	header := struct {
		Schema      string                  `json:"schema"`
		Declaration conformance.Declaration `json:"declaration"`
	}{conformance.Schema, d}
	raw, err := json.Marshal(header)
	require.NoError(t, err)
	content := string(raw) + "\n"
	for _, a := range answers {
		raw, err := json.Marshal(a)
		require.NoError(t, err)
		content += string(raw) + "\n"
	}
	path := filepath.Join(t.TempDir(), "answers.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(content), 0600))
	return path
}

func TestCronConformanceOfflineExportAndCheck(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "must remain offline", 500) }))
	t.Cleanup(server.Close)
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_SERVER", server.URL)
	dir := filepath.Join(t.TempDir(), "vectors")
	_, err := executeConformanceCLI(t, "export", "--dir", dir)
	require.NoError(t, err)
	files, err := conformance.ExportFiles(t.Context(), conformance.Reference{}.Declare())
	require.NoError(t, err)
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		require.Equal(t, string(want), string(got))
	}
	_, err = executeConformanceCLI(t, "export", "--dir", dir)
	require.ErrorContains(t, err, "exists")
	out, err := executeConformanceCLI(t, "check", "--answers", conformanceAnswers(t, nil))
	require.NoError(t, err)
	var report conformance.Report
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	require.Empty(t, report.Failed)
	require.GreaterOrEqual(t, report.Passed, 40)
	require.Zero(t, calls.Load())
}

func TestCronConformanceNamedFailures(t *testing.T) {
	for name, mutate := range map[string]func([]conformance.Answer) []conformance.Answer{
		"changed":   func(a []conformance.Answer) []conformance.Answer { a[0].Outputs = []jsontext.Value{}; return a },
		"missing":   func(a []conformance.Answer) []conformance.Answer { return a[1:] },
		"duplicate": func(a []conformance.Answer) []conformance.Answer { return append(a, a[0]) },
		"unknown": func(a []conformance.Answer) []conformance.Answer {
			return append(a, conformance.Answer{Name: "unknown", Outputs: []jsontext.Value{}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := executeConformanceCLI(t, "check", "--answers", conformanceAnswers(t, mutate))
			require.Error(t, err)
			var ce *cliError
			require.ErrorAs(t, err, &ce)
			require.Equal(t, 1, ce.ExitCode)
			var report conformance.Report
			require.NoError(t, json.Unmarshal([]byte(out), &report))
			require.NotEmpty(t, report.Failed)
			require.NotEmpty(t, report.Failed[0].Name)
		})
	}
}

func TestCronConformanceMalformedAnswers(t *testing.T) {
	for _, raw := range []string{"", "not-json\n", `{"schema":"wrong","declaration":{}}`, `{"schema":"kata.cron.adapter-conformance/v1","declaration":{},"extra":true}`} {
		file := filepath.Join(t.TempDir(), "invalid.jsonl")
		require.NoError(t, os.WriteFile(file, []byte(raw), 0600))
		_, err := executeConformanceCLI(t, "check", "--answers", file)
		require.Error(t, err)
		var ce *cliError
		require.ErrorAs(t, err, &ce)
		require.Equal(t, ExitValidation, ce.ExitCode)
	}
}

func TestCronConformanceExportPreflightsAllFiles(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "timeout.json")
	require.NoError(t, os.WriteFile(sentinel, []byte("keep"), 0600))
	_, err := executeConformanceCLI(t, "export", "--dir", dir)
	require.Error(t, err)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "a late collision must not leave partial exported files")
	raw, err := os.ReadFile(sentinel)
	require.NoError(t, err)
	require.Equal(t, "keep", string(raw))
	_, err = executeConformanceCLI(t, "export", "--dir", "")
	require.Error(t, err)
}

func TestCronConformanceDeclaration(t *testing.T) {
	d := conformance.Reference{}.Declare()
	d.CatchupLimit = 100
	d.FailurePolicy = "stop"
	raw, err := json.Marshal(d)
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "policy.json")
	require.NoError(t, os.WriteFile(file, raw, 0600))
	dir := filepath.Join(t.TempDir(), "vectors")
	out, err := executeConformanceCLI(t, "export", "--dir", dir, "--declaration", file)
	require.NoError(t, err)
	require.True(t, strings.Contains(out, conformance.Schema))
	got, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	require.NoError(t, err)
	require.Contains(t, string(got), `"catchup_limit": 100`)
}

func TestCronConformanceRejectsIncompleteDeclaration(t *testing.T) {
	file := filepath.Join(t.TempDir(), "empty-policy.json")
	require.NoError(t, os.WriteFile(file, []byte(`{}`), 0600))
	_, err := executeConformanceCLI(t, "export", "--dir", filepath.Join(t.TempDir(), "vectors"), "--declaration", file)
	require.Error(t, err)
}
func TestCronConformanceRejectsNullAnswer(t *testing.T) {
	header, _ := json.Marshal(conformanceHeader{Schema: conformance.Schema, Declaration: conformance.Reference{}.Declare()})
	file := filepath.Join(t.TempDir(), "null.jsonl")
	require.NoError(t, os.WriteFile(file, append(header, []byte("\nnull\n")...), 0600))
	_, err := executeConformanceCLI(t, "check", "--answers", file)
	require.Error(t, err)
	var ce *cliError
	require.ErrorAs(t, err, &ce)
	require.Equal(t, ExitValidation, ce.ExitCode)
}
func TestCronConformanceReadCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pipe, writer := io.Pipe()
	reader := &conformanceSignalledReader{ReadCloser: pipe, entered: make(chan struct{})}
	defer reader.Close()
	defer writer.Close()
	done := make(chan error, 1)
	go func() { _, err := readConformanceFile(ctx, reader, "-"); done <- err }()
	<-reader.entered
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("cancelled stdin read remained blocked")
	}
}

type conformanceSignalledReader struct {
	io.ReadCloser
	entered chan struct{}
	once    sync.Once
}

func (r *conformanceSignalledReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.entered) })
	return r.ReadCloser.Read(p)
}

func TestCronConformanceDropAnswersUseSharedChecker(t *testing.T) {
	d := conformance.Reference{}.Declare()
	d.DropRecords = true
	d.ForbidDeferral = "drop_after_grace"
	d.CatchupWindowSeconds = 86400
	suite, err := conformance.Suite(t.Context(), d)
	require.NoError(t, err)
	for _, mutation := range []string{"correct", "wrong-reason", "missing-instant", "extra-group"} {
		t.Run(mutation, func(t *testing.T) {
			answers := []conformance.Answer{}
			for _, s := range suite.Scenarios {
				a, err := conformance.Execute(t.Context(), conformance.Reference{Policy: d}, s)
				require.NoError(t, err)
				if s.Name == "drops/latest" && mutation != "correct" {
					var fields map[string]jsontext.Value
					require.NoError(t, json.Unmarshal(a.Outputs[0], &fields))
					var drops []conformance.Drop
					require.NoError(t, json.Unmarshal(fields["drops"], &drops))
					switch mutation {
					case "wrong-reason":
						drops[0].Reason = "wrong"
					case "missing-instant":
						drops[0].Instants = drops[0].Instants[1:]
					case "extra-group":
						drops = append(drops, drops[0])
					}
					fields["drops"], err = json.Marshal(drops)
					require.NoError(t, err)
					a.Outputs[0], err = json.Marshal(fields, json.Deterministic(true))
					require.NoError(t, err)
				}
				answers = append(answers, a)
			}
			want, err := conformance.Check(t.Context(), d, answers)
			require.NoError(t, err)
			header, err := json.Marshal(conformanceHeader{Schema: conformance.Schema, Declaration: d})
			require.NoError(t, err)
			var data bytes.Buffer
			data.Write(header)
			data.WriteByte('\n')
			for _, answer := range answers {
				raw, err := json.Marshal(answer)
				require.NoError(t, err)
				data.Write(raw)
				data.WriteByte('\n')
			}
			file := filepath.Join(t.TempDir(), "answers.jsonl")
			require.NoError(t, os.WriteFile(file, data.Bytes(), 0600))
			out, err := executeConformanceCLI(t, "check", "--answers", file)
			if mutation == "correct" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				var ce *cliError
				require.ErrorAs(t, err, &ce)
				require.Equal(t, 1, ce.ExitCode)
			}
			var got conformance.Report
			require.NoError(t, json.Unmarshal([]byte(out), &got))
			require.Equal(t, want, got)
		})
	}
	raw, err := json.Marshal(d)
	require.NoError(t, err)
	policy := filepath.Join(t.TempDir(), "policy.json")
	require.NoError(t, os.WriteFile(policy, raw, 0600))
	dir := filepath.Join(t.TempDir(), "vectors")
	_, err = executeConformanceCLI(t, "export", "--dir", dir, "--declaration", policy)
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, "drops.json"))
	require.NoError(t, err)
}

func TestCronConformanceSuppliedDeferralValidation(t *testing.T) {
	raw, err := json.Marshal(conformance.Reference{}.Declare())
	require.NoError(t, err)
	for _, supplied := range []string{`""`, `null`} {
		var fields map[string]jsontext.Value
		require.NoError(t, json.Unmarshal(raw, &fields))
		fields["forbid_deferral"] = []byte(supplied)
		declaration, err := json.Marshal(fields)
		require.NoError(t, err)
		file := filepath.Join(t.TempDir(), "declaration.json")
		require.NoError(t, os.WriteFile(file, declaration, 0600))
		_, err = executeConformanceCLI(t, "export", "--dir", filepath.Join(t.TempDir(), "vectors"), "--declaration", file)
		var ce *cliError
		require.ErrorAs(t, err, &ce)
		require.Equal(t, ExitValidation, ce.ExitCode)
		header, err := json.Marshal(map[string]any{"schema": conformance.Schema, "declaration": fields})
		require.NoError(t, err)
		answers := filepath.Join(t.TempDir(), "answers.jsonl")
		require.NoError(t, os.WriteFile(answers, append(header, '\n'), 0600))
		_, err = executeConformanceCLI(t, "check", "--answers", answers)
		require.ErrorAs(t, err, &ce)
		require.Equal(t, ExitValidation, ce.ExitCode)
	}
}
