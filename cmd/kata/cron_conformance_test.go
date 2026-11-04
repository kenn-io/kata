package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.kenn.io/kata/pkg/cron/conformance"
)

func conformanceAnswers(t *testing.T, d conformance.Declaration) []byte {
	t.Helper()
	header, err := json.Marshal(conformanceHeader{Schema: conformance.Schema, Declaration: d})
	if err != nil {
		t.Fatal(err)
	}
	raw := append(header, '\n')
	suite, err := conformance.Suite(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range suite.Scenarios {
		answer, err := conformance.Execute(t.Context(), conformance.Reference{Policy: d}, s)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(answer)
		if err != nil {
			t.Fatal(err)
		}
		raw = append(append(raw, b...), '\n')
	}
	return raw
}

func runConformance(ctx context.Context, t *testing.T, in []byte, args ...string) ([]byte, error) {
	t.Helper()
	cmd := newRootCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetIn(bytes.NewReader(in))
	cmd.SetArgs(append([]string{"cron", "conformance"}, args...))
	err := cmd.ExecuteContext(ctx)
	return out.Bytes(), err
}

func TestCronConformanceOffline(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		t.Error("offline command contacted server")
		w.WriteHeader(500)
	}))
	defer server.Close()
	t.Setenv("KATA_SERVER", server.URL)
	t.Setenv("KATA_HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "vectors")
	raw, err := runConformance(t.Context(), t, nil, "export", "--dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(conformance.Schema)) {
		t.Fatalf("%s", raw)
	}
	files, err := conformance.ExportFiles(t.Context(), conformance.Reference{}.Declare())
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("changed %s", name)
		}
	}
	answers := conformanceAnswers(t, conformance.Reference{}.Declare())
	report, err := runConformance(t.Context(), t, answers, "check", "--answers", "-")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(report, []byte(`"failed":[]`)) {
		t.Fatalf("%s", report)
	}
	if calls != 0 {
		t.Fatalf("%d network calls", calls)
	}
}

func TestCronConformanceInputAndExitCodes(t *testing.T) {
	lines := bytes.Split(bytes.TrimSpace(conformanceAnswers(t, conformance.Reference{}.Declare())), []byte{'\n'})
	var answer conformance.Answer
	if err := json.Unmarshal(lines[1], &answer); err != nil {
		t.Fatal(err)
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(answer.Outputs[0], &fields); err != nil {
		t.Fatal(err)
	}
	fields["typo"] = jsontext.Value(`true`)
	var err error
	answer.Outputs[0], err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	lines[1], err = json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		input []byte
		args  []string
	}{
		{"missing answers", nil, []string{"check"}},
		{"malformed output", bytes.Join(lines, []byte{'\n'}), []string{"check", "--answers", "-"}},
		{"missing dir", nil, []string{"export"}},
		{"bad header", []byte(`{}`), []string{"check", "--answers", "-"}},
		{"incomplete declaration", []byte(`{"name":"example"}`), []string{"export", "--dir", t.TempDir(), "--declaration", "-"}},
		{"unknown declaration field", []byte(`{"name":"example","failure_policy":"continue","typo":1}`), []string{"export", "--dir", t.TempDir(), "--declaration", "-"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runConformance(t.Context(), t, tc.input, tc.args...)
			var cli *cliError
			if !errors.As(err, &cli) || cli.ExitCode != 3 {
				t.Fatalf("want exit 3, got %v", err)
			}
		})
	}
}

func TestCronConformanceNamedDropMismatch(t *testing.T) {
	d := conformance.Reference{}.Declare()
	d.DropRecords = true
	d.ForbidDeferral = "drop_after_grace"
	original := conformanceAnswers(t, d)
	lines := bytes.Split(bytes.TrimSpace(original), []byte{'\n'})
	var answers []conformance.Answer
	for i := 1; i < len(lines); i++ {
		var answer conformance.Answer
		if err := json.Unmarshal(lines[i], &answer); err != nil {
			t.Fatal(err)
		}
		if answer.Name == "drops/latest" {
			var output map[string]jsontext.Value
			if err := json.Unmarshal(answer.Outputs[0], &output); err != nil {
				t.Fatal(err)
			}
			output["drops"] = jsontext.Value(`[{"reason":"overlap_skipped","instants":["2030-01-01T01:00:00Z","2030-01-01T02:00:00Z"]}]`)
			answer.Outputs[0], _ = json.Marshal(output)
		}
		answers = append(answers, answer)
		lines[i], _ = json.Marshal(answer)
	}
	want, err := conformance.Check(t.Context(), d, answers)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := runConformance(t.Context(), t, bytes.Join(lines, []byte{'\n'}), "check", "--answers", "-")
	var cli *cliError
	if !errors.As(err, &cli) || cli.ExitCode != 1 {
		t.Fatalf("want exit 1, got %v", err)
	}
	var got conformance.Report
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(got, json.Deterministic(true))
	b, _ := json.Marshal(want, json.Deterministic(true))
	if !bytes.Equal(a, b) || len(got.Failed) != 1 || got.Failed[0].Name != "drops/latest" {
		t.Fatalf("Go/CLI mismatch %s / %s", a, b)
	}
}

// readNotification makes cancellation occur after the entry guard and after
// the cancellation callback is installed, while stdin is entering its pipe read.
type readNotification struct {
	io.ReadCloser
	started chan struct{}
	once    sync.Once
}

func (r *readNotification) Read(buffer []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	return r.ReadCloser.Read(buffer)
}

func TestConformanceInputCancellationAndLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		read func(context.Context, io.Reader) ([]byte, error)
	}{
		{"conformance", func(ctx context.Context, reader io.Reader) ([]byte, error) {
			return readConformanceFile(ctx, reader, "-")
		}},
		{"request", func(ctx context.Context, reader io.Reader) ([]byte, error) {
			cmd := newCronCmd()
			cmd.SetContext(ctx)
			cmd.SetIn(reader)
			return (&cronCLIOptions{file: "-", resource: "job", action: "create"}).readRequest(cmd)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			pipe, writer := io.Pipe()
			defer func() { _ = pipe.Close() }()
			defer func() { _ = writer.Close() }()
			reader := &readNotification{ReadCloser: pipe, started: make(chan struct{})}
			done := make(chan error, 1)
			go func() { _, err := tc.read(ctx, reader); done <- err }()
			select {
			case <-reader.started:
			case <-time.After(2 * time.Second):
				t.Fatal("stdin read never started")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cancelled stdin blocked")
			}
		})
	}
	_, err := readConformanceFile(t.Context(), strings.NewReader(strings.Repeat("x", conformanceInputLimit+1)), "-")
	if err == nil || err.Error() != "conformance input exceeds 16 MiB" {
		t.Fatalf("limit mapping: %v", err)
	}
}

func TestConformanceExportRollback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "b.json")
	if err := os.WriteFile(path, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	err := exportConformanceFiles(t.Context(), dir, map[string][]byte{"a.json": []byte("new"), "b.json": []byte("replacement")})
	if err == nil {
		t.Fatal("replaced existing")
	}
	if _, err := os.Stat(filepath.Join(dir, "a.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback left created file: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "existing" {
		t.Fatalf("existing file changed: %s %v", raw, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	dir = filepath.Join(dir, "cancelled")
	if err := exportConformanceFiles(ctx, dir, map[string][]byte{"a.json": []byte("new")}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled export created directory: %v", err)
	}
}
