package cron

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"go.kenn.io/kata/internal/colormode"
)

var updatePortGoldens = flag.Bool("update-goldens", false, "update native cron UI snapshots")

func TestCronSnapshots(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 40}, {160, 40}, {80, 10}} {
		for _, screen := range []string{"jobs", "runs", "workflows", "job-detail", "run-detail", "workflow-detail", "job-form", "kata", "jobs-empty", "runs-empty", "workflows-empty", "jobs-filter", "runs-filter", "workflows-filter", "workflow-edit", "workflows-expanded", "confirm", "project-picker"} {
			for _, mode := range []colormode.Mode{colormode.None, colormode.Dark, colormode.Light} {
				t.Run(fmt.Sprintf("%s-%dx%d-%d", screen, size[0], size[1], mode), func(t *testing.T) {
					preserveTheme(t)
					m := newModel(Options{Now: func() time.Time { return time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC) }})
					applyColorMode(mode, true)
					m.Resize(size[0], size[1]-3)
					d := nativeFixture()
					d.Runs[0].StartedAt = &d.Runs[0].CreatedAt
					d.Runs[0].OccurrenceKey = new("scheduled-example")
					m.SetData(d)
					switch screen {
					case "runs", "runs-empty", "runs-filter":
						m.SelectTab(1)
					case "workflows", "workflows-empty", "workflows-filter", "workflows-expanded":
						m.SelectTab(2)
					case "job-detail":
						m.pushDetail(viewJob, "7", "job-1")
					case "run-detail":
						m.SelectTab(1)
						m.pushDetail(viewRun, "7", "run-1")
					case "workflow-detail":
						m.SelectTab(2)
						m.pushDetail(viewWorkflow, "7", "flow-1")
					case "job-form":
						j := nativeFixture().Jobs[0]
						m.editor = newJobForm(t.Context(), nil, &j, m.data.Workflows, editorContext{ProjectUID: "7", Project: "example-project"})
						m.editor.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1] - 3})
					}
					switch screen {
					case "jobs-empty", "runs-empty", "workflows-empty":
						m.SetData(Data{})
					case "jobs-filter", "runs-filter", "workflows-filter":
						m.query = "missing"
						m.searching = true
						m.rebuild()
					case "workflows-expanded":
						if r, ok := m.selected(); ok {
							m.expanded[workflowExpandKey(r)] = true
						}
					case "confirm":
						m.confirm = append(m.confirm, m.data.Jobs[0])
					case "project-picker":
						m.projectChoices = []string{"7"}
					case "workflow-edit":
						f, err := newWorkflowForm(t.Context(), nil, m.data.Workflows[0])
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { _ = f.Close() })
						m.editor = f
					}
					height := size[1]
					if m.TabsVisible() {
						height -= 3
					}
					m.Resize(size[0], height)
					text := m.View().Content
					if f, ok := m.editor.(*workflowForm); ok {
						text = strings.ReplaceAll(text, truncate(f.path, size[0]), "/tmp/example-workflow.json")
					}
					if m.TabsVisible() {
						text = m.Tabs(m.tab+1, size[0]) + "\n" + text
					}
					if screen == "kata" {
						text = m.Tabs(0, size[0])
					}
					if dir := os.Getenv("PARITY_OUT"); dir != "" {
						path := filepath.Join(dir, fmt.Sprintf("native-%s-%dx%d-%d.ansi", screen, size[0], size[1], mode))
						if err := os.WriteFile(path, []byte(text), 0600); err != nil { //nolint:gosec // G703: test operator explicitly chooses the optional artifact directory.
							t.Fatal(err)
						}
					}

					ansiPath := filepath.Join("testdata", "golden", fmt.Sprintf("%s-%dx%d-%d.ansi", screen, size[0], size[1], mode))
					if *updatePortGoldens {
						if err := os.WriteFile(ansiPath, []byte(text), 0600); err != nil {
							t.Fatal(err)
						}
					}
					wantANSI, err := os.ReadFile(ansiPath) //nolint:gosec // G304: neutral golden fixture path.
					if err != nil {
						t.Fatal(err)
					}
					if string(wantANSI) != text {
						t.Fatalf("ANSI snapshot mismatch %s", ansiPath)
					}
					lines := strings.Split(text, "\n")
					// List chrome spans the whole terminal at every size.
					if strings.HasPrefix(screen, "jobs") || strings.HasPrefix(screen, "runs") || strings.HasPrefix(screen, "workflows") {
						for _, i := range []int{2, 3, len(lines) - 1} {
							if ansi.StringWidth(lines[i]) != size[0] {
								t.Fatalf("list line %d stops short of the terminal width: %q", i, ansi.Strip(lines[i]))
							}
						}
					}
					if len(lines) > size[1] {
						t.Fatal("frame exceeded terminal height")
					}
					for _, line := range lines {
						if ansi.StringWidth(line) > size[0] {
							t.Fatal("frame exceeded terminal width", line)
						}
					}

				})
			}
		}
	}
}
