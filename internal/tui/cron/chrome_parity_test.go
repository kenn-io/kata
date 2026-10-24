package cron

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestSourceTabLabels(t *testing.T) {
	for active := range 4 {
		for _, width := range []int{80, 120, 160} {
			text := ansi.Strip(Tabs(active, width))
			if !strings.Contains(text, "│ kata カタ │ jobs │ runs │ workflows │") {
				t.Fatalf("source labels missing: %s", text)
			}
			for x, want := range map[int]int{3: 0, 15: 1, 22: 2, 30: 3} {
				got, ok := TabAt(active, width, x, 1)
				if !ok || got != want {
					t.Fatalf("tab at %d: %d %v; want %d", x, got, ok, want)
				}
			}
		}
	}
}

func TestSourceTabStatus(t *testing.T) {
	preserveTheme(t)
	m := newModel(Options{})
	d := nativeFixture()
	d.Runs[0].Status = "running"
	m.SetData(d)
	for _, width := range []int{80, 120, 160} {
		text := ansi.Strip(m.Tabs(1, width))
		line := strings.Split(text, "\n")[1]
		if !strings.HasSuffix(line, "1 running") || ansi.StringWidth(line) != width {
			t.Fatalf("right-aligned source status missing: %q", line)
		}
	}
	d.Problems = []string{"snapshot error"}
	m.SetData(d)
	if text := ansi.Strip(m.Tabs(1, 80)); !strings.Contains(text, "snapshot error") {
		t.Fatal(text)
	}
	d.Runs[0].Status = "succeeded"
	d.Problems = nil
	m.SetData(d)
	if text := ansi.Strip(m.Tabs(1, 80)); strings.Contains(text, "running") {
		t.Fatal("completed observation counted", text)
	}
}

func TestSourceDetailAndEditorUseFullHeight(t *testing.T) {
	m := newModel(Options{})
	m.SetData(nativeFixture())
	if !m.TabsVisible() {
		t.Fatal("list tabs hidden")
	}
	m.pushDetail(viewJob, "7", "job-1")
	if m.TabsVisible() {
		t.Fatal("source detail has no outer tabs")
	}
	m.editor = newJobForm(t.Context(), nil, nil, nil, editorContext{})
	if m.TabsVisible() {
		t.Fatal("source editor has no outer tabs")
	}
}

func TestSourcePageRowsReserveSplitAndConfirmation(t *testing.T) {
	m := newModel(Options{})
	m.SetData(nativeFixture())
	m.Resize(160, 37)
	want := max(3, 37-2-len(jobsHelp().Lines(160))-len(m.footer())-2)
	if got := m.pageRows(); got != want {
		t.Fatalf("split page rows %d, want %d", got, want)
	}
	m.confirm = append(m.confirm, m.data.Jobs[0])
	want -= m.jobBoxHeight()
	if got := m.pageRows(); got != want {
		t.Fatalf("confirmation page rows %d, want %d", got, want)
	}
}
