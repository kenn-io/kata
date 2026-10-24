package cron

import (
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func longBody() bodyLines {
	b := bodyLines{}
	for i := range 5 {
		b = append(b, bodyLine{Text: strings.Repeat(string(rune('a'+i)), 8), Hit: &rowHit{Kind: "run", Key: string(rune('a' + i)), Index: i, X0: 0, X1: 8}, Animations: []animationSpan{{4, 8}}})
	}
	return b
}
func TestWindowFollow(t *testing.T) {
	b := longBody()
	b[4].Text = "▶ eeeeee"
	b[4].Selected = true
	scroll := 0
	out, n := window(b, 3, &scroll, false)
	if len(out) != 3 || n != 2 || out[2].Text != "↓ 3 more" || scroll != 0 {
		t.Fatal(out, n, scroll)
	}
	out, n = window(b, 3, &scroll, true)
	if scroll != 3 || n != 2 || !strings.Contains(out[1].Text, "▶") || out[2].Text != "↑ 3 more" {
		t.Fatal(out, n, scroll)
	}
	out, n = window(b, 1, &scroll, true)
	if len(out) != 1 || n != 1 || scroll != 4 {
		t.Fatal(out, n, scroll)
	}
}
func TestWindowFocusLine(t *testing.T) {
	scroll := 0
	out, _ := window(longBody(), 3, &scroll, true, focusLine(4))
	if scroll != 3 || out[1].Hit.Key != "e" {
		t.Fatal(scroll, out)
	}
}
func TestWindowFollowMargin(t *testing.T) {
	b := append(longBody(), longBody()...)
	b[3].Text = "▶ dddddd"
	b[3].Selected = true
	scroll := 0
	window(b, 5, &scroll, true)
	if scroll != 1 {
		t.Fatal(scroll)
	}
	scroll = 2
	window(b, 5, &scroll, true)
	if scroll != 2 {
		t.Fatal(scroll)
	}
	b[3].Text = "  dddddd"
	b[3].Selected = false
	b[0].Text = "▶ decoy"
	scroll = 2
	window(b, 5, &scroll, true)
	if scroll != 2 {
		t.Fatal("glyph without selection was followed", scroll)
	}
}
func TestPaneClip(t *testing.T) {
	for height := 1; height <= 8; height++ {
		scroll := 0
		f := (pane{Top: block("brand\ntop\ntabs\nbottom"), Body: longBody(), Bottom: block("status\nhelp\nextra\nextra"), Width: 5, Scroll: &scroll}).Render(height)
		lines := strings.Split(f.Text, "\n")
		if len(lines) != height {
			t.Fatal(height, lines)
		}
		for _, s := range lines {
			if ansi.StringWidth(s) > 5 || strings.Contains(s, "…") {
				t.Fatal(s)
			}
		}
		if height <= 4 && len(f.Hits.Rows) > 0 {
			t.Fatal(f.Hits)
		}
		if height == 5 && f.Hits.Rows[4].Key != "a" {
			t.Fatal(f.Hits)
		}
	}
	f := (pane{Body: block("short"), Bottom: block("help"), Width: 10}).Render(5)
	if strings.Split(f.Text, "\n")[4] != "help" {
		t.Fatal(f.Text)
	}
	if len(block("")) != 0 || len(block("a\n")) != 1 || len(block("a\n\n")) != 2 {
		t.Fatal("block trailing newline")
	}
}
func TestHits(t *testing.T) {
	scroll := 0
	f := (pane{Top: block("top"), Body: longBody(), Bottom: block("help"), Width: 5, Scroll: &scroll}).Render(5)
	for _, p := range [][2]int{{-1, 1}, {5, 1}, {0, -1}, {0, 0}, {0, 3}, {0, 4}, {0, 5}} {
		if _, ok := f.Hits.At(p[0], p[1]); ok {
			t.Fatal(p)
		}
	}
	if hit, ok := f.Hits.At(4, 1); !ok || hit.Key != "a" {
		t.Fatal(hit, ok)
	}

}
func TestVisibleAnimationSpans(t *testing.T) {
	for _, c := range []struct {
		width, height, scroll int
		want                  bool
	}{{4, 3, 0, false}, {5, 3, 0, true}, {5, 1, 4, true}} {
		s := c.scroll
		f := (pane{Body: longBody(), Width: c.width, Scroll: &s}).Render(c.height)
		if f.Animates != c.want {
			t.Fatal(c, f)
		}
	}
	b := longBody()
	for i := range b {
		b[i].Animations = nil
	}
	b[4].Animations = []animationSpan{{0, 2}}
	s := 0
	f := (pane{Body: b, Width: 8, Scroll: &s}).Render(3)
	if f.Animates {
		t.Fatal("hidden animation")
	}
	if !reflect.DeepEqual(b[4].Animations, []animationSpan{{0, 2}}) {
		t.Fatal("caller modified")
	}
}
func FuzzPaneBounds(f *testing.F) {
	f.Add(5, 3, 0)
	f.Add(1, 1, -5)
	f.Fuzz(func(t *testing.T, w, h, scroll int) {
		w = 1 + int(uint(w)%100)
		h = 1 + int(uint(h)%50)
		frame := (pane{Body: longBody(), Width: w, Scroll: &scroll}).Render(h)
		if len(strings.Split(frame.Text, "\n")) > h {
			t.Fatal(frame)
		}
		for y, hit := range frame.Hits.Rows {
			if y < 0 || y >= h || hit.X0 < 0 || hit.X1 > w {
				t.Fatal(frame.Hits)
			}
		}
	})
}

func TestWindowFollowsSelectedMetadata(t *testing.T) {
	body := longBody()
	body[0].Text = "▶ decoy"
	body[4].Text = "focused"
	body[4].Selected = true
	scroll := 0
	out, _ := window(body, 3, &scroll, true)
	if scroll != 3 || out[1].Hit.Key != "e" {
		t.Fatal(scroll, out)
	}
}

func TestWindowFollowsRenderedSelection(t *testing.T) {
	for _, source := range []string{"table", "detail-cells", "detail-render"} {
		t.Run(source, func(t *testing.T) {
			selected := tableRow([]cell{{Text: "focused"}}, []int{8}, true, 0, 0)
			if source != "table" {
				d := newDetail(40, 0, detailSelection{Key: "focused"}, keepMissing, func(string) bool { return true })
				d.Selectable("run")
				item := detailItem{Key: "focused", Cells: []cell{{Text: "focused"}}, Widths: []int{8}}
				if source == "detail-render" {
					item.Render = func(bool, int) string { return "focused" }
				}
				d.List("run", []detailItem{item}, -1)
				selected = d.Body()[0]
			}
			// Changing how a selected row is drawn must not change which row is followed.
			selected.Text = "focused"
			body := append(longBody(), selected)
			scroll := 0
			out, _ := window(body, 3, &scroll, true)
			if scroll != 4 || out[1].Text != "focused" {
				t.Fatal("renderer lost follow selection", scroll, out)
			}
		})
	}
}
