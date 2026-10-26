package cron

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"go.kenn.io/kata/internal/colormode"
	"hegel.dev/go/hegel"
)

// The selected run's own summary stays visible in the peek at every snapshot
// size and theme.
func TestCronPeekSummaryAtEverySize(t *testing.T) {
	for _, size := range [][2]int{{80, 10}, {80, 24}, {120, 40}, {160, 40}} {
		for _, mode := range []colormode.Mode{colormode.None, colormode.Dark, colormode.Light} {
			t.Run(fmt.Sprintf("%dx%d/theme-%d", size[0], size[1], mode), func(t *testing.T) {
				preserveTheme(t)
				m := newModel(Options{})
				applyColorMode(mode, true)
				d := nativeFixture()
				d.Runs[0].Summary = summary("own summary marker")
				m.SetData(d)
				m.SelectTab(1)
				m.Resize(size[0], size[1])
				text := ansi.Strip(m.View().Content)
				if !strings.Contains(text, "own summary marker") {
					t.Fatalf("selected-run peek lost its own summary:\n%s", text)
				}
			})
		}
	}
}

// Selection is project/run identity, not a row ordinal. Reordering history must
// keep the same recorded evidence selected. Resource size is capped only when
// materializing generated values; the generator retains the full uint8 domain.
func TestCronSelectionSurvivesHistoryOrderingProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		count := 2 + int(hegel.Draw(ht, hegel.Integers[uint8](0, 255)))%15
		selected := int(hegel.Draw(ht, hegel.Integers[uint8](0, 255))) % count
		d := nativeFixture()
		first := d.Runs[0]
		d.Runs = nil
		for i := range count {
			r := first
			r.UID = fmt.Sprintf("run-%03d", i)
			d.Runs = append(d.Runs, r)
		}
		m := newModel(Options{})
		m.SetData(d)
		m.SelectTab(1)
		m.move(selected)
		before, ok := m.selected()
		if !ok {
			ht.Fatal("fixture has no selected run")
		}
		for i, j := 0, len(d.Runs)-1; i < j; i, j = i+1, j-1 {
			d.Runs[i], d.Runs[j] = d.Runs[j], d.Runs[i]
		}
		newer := first
		newer.UID, newer.CreatedAt = "new-head", first.CreatedAt.Add(time.Hour)
		d.Runs = append(d.Runs, newer)
		m.SetData(d)
		after, ok := m.selected()
		if !ok || after.identity() != before.identity() {
			ht.Fatalf("history reorder changed selection %q -> %q", before.identity(), after.identity())
		}
	}, hegel.WithTestCases(1000))
}

func TestCronRunDetailKeepsTokenSummary(t *testing.T) {
	d := nativeFixture()
	d.Runs[0].Summary.InputTokens, d.Runs[0].Summary.OutputTokens = 17, 23
	m := newModel(Options{})
	m.SetData(d)
	m.Resize(160, 80)
	m.pushDetail(viewRun, "7", "run-1")
	if text := ansi.Strip(m.View().Content); !strings.Contains(text, "17 input · 23 output") {
		t.Fatalf("existing recorded token summary was lost:\n%s", text)
	}
}

func TestCronPeekWithoutSummaryOrIssue(t *testing.T) {
	d := nativeFixture()
	d.Runs[0].Summary.Message = ""
	m := newModel(Options{})
	m.Resize(120, 30)
	m.SetData(d)
	m.SelectTab(1)
	text := ansi.Strip(m.View().Content)
	if !strings.Contains(text, "No summary recorded") || !strings.Contains(text, "No issue linked") || strings.Contains(text, "i open issue") {
		t.Fatalf("unlinked run without a summary lacks explicit states:\n%s", text)
	}
}
