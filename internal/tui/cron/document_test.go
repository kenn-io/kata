package cron

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func bodyText(b bodyLines) string {
	out := []string{}
	for _, l := range b {
		out = append(out, ansi.Strip(l.Text))
	}
	return strings.Join(out, "\n")
}
func detailRows(n int) []detailItem {
	items := make([]detailItem, n)
	for i := range items {
		items[i] = detailItem{Key: fmt.Sprintf("run-%d", i), Cells: []cell{{Text: fmt.Sprintf("run %d long text", i)}}, Widths: []int{8}}
	}
	return items
}
func TestDetailKV(t *testing.T) {
	d := newDetail(40, 0, detailSelection{}, noSelection, nil)
	d.Title("a title", "held", attentionStyle)
	d.KV("empty", "")
	d.KV("name", "example")
	d.KVWrap("about", "one two three four five six seven eight nine ten")
	d.Section("RUNS")
	d.Lines("one\n\x1b[31mtwo")
	s := bodyText(d.Body())
	for _, want := range []string{"a title held", "\nname           example", "\n\nRuns\n  one\n  two"} {
		if !strings.Contains(s, want) {
			t.Error(want, s)
		}
	}
}

func TestDetailTitleKeepsIdentity(t *testing.T) {
	d := newDetail(80, 16, detailSelection{}, noSelection, nil)
	d.Title(strings.Repeat("Example job ", 10), "01K00000000000000000000001", subtleStyle)
	text := bodyText(d.Body())
	if !strings.HasSuffix(text, " 01K00000000000000000000001") || !strings.HasPrefix(text, "Example job") || ansi.StringWidth(text) > 80 {
		t.Fatal("long title hid the identity", text)
	}
}
func TestDetailSelectionAndLimit(t *testing.T) {
	d := newDetail(50, 16, detailSelection{}, keepMissing, func(string) bool { return true })
	d.Selectable("run")
	d.List("run", detailRows(30), 20)
	if len(d.Items()) != 20 || d.Selection().Key != "run-0" || !strings.Contains(bodyText(d.Body()), "  +10 older") {
		t.Fatal(d.Selection(), len(d.Items()), bodyText(d.Body()))
	}
	if !strings.HasPrefix(bodyText(d.Body()), "▶ run 0 l…") {
		t.Fatal(bodyText(d.Body()))
	}
	for _, line := range d.Body() {
		if len(line.Animations) != 0 {
			t.Fatal("detail animated")
		}
	}
}
func TestDetailSelectionPolicies(t *testing.T) {
	hits := []rowHit{{Key: "a"}, {Key: "b"}, {Key: "c"}}
	for _, policy := range []missingPolicy{keepMissing, nearestSurvivor} {
		s := resolveSelection(detailSelection{"hidden", 1}, hits, policy, func(string) bool { return true })
		if s.Key != "hidden" || s.Ordinal != 1 {
			t.Fatal(s)
		}
		s = resolveSelection(detailSelection{"hidden", 1}, hits, policy, func(string) bool { return false })
		want := "hidden"
		if policy == nearestSurvivor {
			want = "b"
		}
		if s.Key != want {
			t.Fatal(s)
		}
	}
	for _, c := range []struct {
		delta int
		key   string
	}{{0, "gone"}, {1, "b"}, {-1, "a"}, {3, "c"}, {-3, "a"}} {
		if s := moveSelection(detailSelection{"gone", 1}, hits, c.delta); s.Key != c.key {
			t.Fatal(c, s)
		}
	}
	for _, more := range []bool{false, true} {
		d := newDetail(40, 14, detailSelection{"run-20", 19}, nearestSurvivor, func(string) bool { return true })
		d.Selectable("run", "job")
		d.List("run", detailRows(30), 20, "Runs lists every retained run")
		d.Styled(bodyLine{Text: "  SUPPORT"})
		if more {
			d.List("job", []detailItem{{Key: "next", Cells: []cell{{Text: "next"}}, Widths: []int{5}}}, -1)
		}
		s := d.Selection()
		if s.Key != "run-20" || s.Ordinal != 20 {
			t.Fatal(s)
		}
		if moveSelection(s, d.Items(), -1).Key != "run-19" {
			t.Fatal("cap previous")
		}
		want := "run-19"
		if more {
			want = "next"
		}
		if moveSelection(s, d.Items(), 1).Key != want {
			t.Fatal("cap next")
		}
		if !strings.Contains(bodyText(d.Body()), "+10 older · Runs lists every retained run") {
			t.Fatal(bodyText(d.Body()))
		}
	}
}
func TestDetailWithoutSelection(t *testing.T) {
	d := newDetail(20, 14, detailSelection{Key: "a"}, noSelection, nil)
	d.Selectable("run")
	d.List("run", detailRows(3), -1)
	if len(d.Items()) != 0 || d.Selection().Key != "" {
		t.Fatal(d.Selection())
	}
}
func TestDetailStyledInsertion(t *testing.T) {
	d := newDetail(30, 14, detailSelection{}, keepMissing, nil)
	d.Selectable("run", "step")
	d.List("run", detailRows(1), -1)
	d.Styled(bodyLine{Text: "  \x1b[1mSUPPORT\x1b[0m", Hit: &rowHit{Key: "bad"}, Animations: []animationSpan{{0, 5}}})
	d.List("step", []detailItem{{Key: "step", Render: func(bool, int) string { return "custom" }, After: bodyLines{{Text: "    expanded", Hit: &rowHit{Key: "bad"}}}}}, -1)
	b := d.Body()
	if len(d.Items()) != 2 || b[1].Text != "  \x1b[1mSUPPORT\x1b[0m" || b[1].Hit != nil || len(b[1].Animations) > 0 || b[3].Text != "    expanded" || b[3].Hit != nil {
		t.Fatal(b, d.Items())
	}
	if !reflect.DeepEqual([]string{b[0].Hit.Key, b[2].Hit.Key}, []string{"run-0", "step"}) {
		t.Fatal(b)
	}
}
func TestDetailPinnedFooter(t *testing.T) {
	d := newDetail(80, 14, detailSelection{}, noSelection, nil)
	d.Title("long detail", "", titleStyle)
	d.Lines(strings.Repeat("line\n", 40))
	scroll := 0
	f := (pane{Body: d.Body(), Bottom: block("status\nhelp"), Width: 80, Scroll: &scroll}).Render(24)
	lines := strings.Split(ansi.Strip(f.Text), "\n")
	if lines[0] != "long detail" || lines[23] != "help" || lines[22] != "status" {
		t.Fatal(f.Text)
	}
}
