package cron

import (
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"go.kenn.io/kata/internal/colormode"
)

func TestLayout(t *testing.T) {
	for _, c := range []struct {
		cols       []column
		avail, min int
		want       []int
	}{
		{[]column{{Width: 1}, {Width: 8}, {Flex: true}}, 24, 0, []int{1, 8, 13}},
		{[]column{{Width: 16}, {Width: 8, Shrink: 1}, {Width: 9, Shrink: 2}, {Flex: true}}, 25, 0, []int{13, 3, 3, 3}},
		{[]column{{Width: 1}, {Width: 8}, {Flex: true}}, 2, 0, []int{1, 3, 3}},
		{[]column{{Width: 9, Shrink: 2}, {Width: 9, Shrink: 1}}, 14, 0, []int{9, 4}},
	} {
		if got := layout(c.cols, c.avail, c.min); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Layout=%v want %v", got, c.want)
		}
	}
}
func TestMarquee(t *testing.T) {
	for _, c := range []struct {
		frame int
		want  string
	}{{0, "abcd"}, {6, "abcd"}, {7, "bcde"}, {23, "abcd"}} {
		if got := marquee("abcdefghij", 4, c.frame); got != c.want {
			t.Errorf("frame %d = %q", c.frame, got)
		}
	}
	for _, s := range []string{"你好世界", "e\u0301e\u0301long", "\x1b[31mabcdefgh"} {
		for frame := range 45 {
			got := marquee(s, 3, frame)
			if ansi.StringWidth(got) != 3 || strings.Contains(got, "\x1b") {
				t.Errorf("%q frame %d = %q", s, frame, got)
			}
		}
	}
	if marquee("text", 0, 7) != "" {
		t.Fatal("zero width")
	}
}
func TestEveryRowMarquees(t *testing.T) {
	for _, selected := range []bool{false, true} {
		for _, c := range []struct {
			frame int
			want  string
		}{{0, "  abcd ok"}, {6, "  abcd ok"}, {7, "  bcde ok"}} {
			got := tableRow([]cell{{Text: "abcdefghij"}, {Text: "ok"}}, []int{4, 2}, selected, c.frame, 0)
			text := ansi.Strip(got.Text)
			if selected {
				text = strings.Replace(text, "▶ ", "  ", 1)
			}
			if text != c.want {
				t.Error(text, c.want)
			}
			if !reflect.DeepEqual(got.Animations, []animationSpan{{2, 6}}) {
				t.Fatal(got.Animations)
			}
		}
	}
}
func TestSelectedRowBand(t *testing.T) {
	preserveTheme(t)
	for _, mode := range []colormode.Mode{colormode.Light, colormode.Dark, colormode.None} {
		applyColorMode(mode, false)
		got := tableRow([]cell{{Text: "failed", Style: failStyle}, {Text: "x", Style: titleStyle}}, []int{8, 1}, true, 0, 0).Text
		if ansi.Strip(got) != "▶ failed   x" {
			t.Fatal(got)
		}
		// Each visible character, including gaps and padding, must keep the band.
		band := "48;5;153"
		if mode == colormode.Dark {
			band = "48;5;24"
		}
		if mode == colormode.None {
			band = "7"
		}
		fg, bg, reverse := "", "", false
		for _, part := range ansiTokens(got) {
			if after, ok := strings.CutPrefix(part, "\x1b["); ok {
				params := strings.Split(strings.TrimSuffix(after, "m"), ";")
				for i := 0; i < len(params); i++ {
					switch params[i] {
					case "0", "":
						fg, bg, reverse = "", "", false
					case "7":
						reverse = true
					case "27":
						reverse = false
					case "38", "48":
						if i+2 < len(params) && params[i+1] == "5" {
							v := strings.Join(params[i:i+3], ";")
							if params[i] == "38" {
								fg = v
							} else {
								bg = v
							}
							i += 2
						}
					case "39":
						fg = ""
					case "49":
						bg = ""
					}
				}
				continue
			}
			for _, ch := range part {
				if mode == colormode.None {
					if !reverse {
						t.Errorf("unbanded %q in %q", ch, got)
					}
				} else if bg != band {
					t.Errorf("unbanded %q in %q", ch, got)
				}
				if fg != "" {
					t.Errorf("semantic foreground on %q", ch)
				}
			}
		}
	}
}
func ansiTokens(s string) []string {
	var out []string
	for len(s) > 0 {
		if strings.HasPrefix(s, "\x1b[") {
			i := strings.IndexByte(s, 'm')
			if i < 0 {
				break
			}
			out = append(out, s[:i+1])
			s = s[i+1:]
		} else {
			i := strings.Index(s, "\x1b[")
			if i < 0 {
				i = len(s)
			}
			out = append(out, s[:i])
			s = s[i:]
		}
	}
	return out
}

func TestStaticDetailRow(t *testing.T) {
	got := ansi.Strip(staticRow([]cell{{Text: "abcdefgh"}}, []int{4}, false))
	if got != "  abc…" {
		t.Fatal(got)
	}
}

func TestAlternatingRowBandCoversPadding(t *testing.T) {
	preserveTheme(t)
	for _, mode := range []colormode.Mode{colormode.Light, colormode.Dark} {
		applyColorMode(mode, false)
		// A scrolled odd row must shade the gutter, cells, padding, and gaps.
		got := tableRow([]cell{{Text: "failed", Style: failStyle}, {Text: "x", Style: titleStyle}}, []int{8, 3}, false, 0, 3).Text
		want := "48;5;254"
		if mode == colormode.Dark {
			want = "48;5;236"
		}
		bg := ""
		for _, token := range ansiTokens(got) {
			if after, ok := strings.CutPrefix(token, "\x1b["); ok {
				params := strings.TrimSuffix(after, "m")
				if params == "0" {
					bg = ""
				}
				if strings.Contains(params, want) {
					bg = want
				}
			} else if bg != want {
				t.Errorf("unshaded %q in %q", token, got)
			}
		}
		for _, index := range []int{0, 2} {
			got := tableRow([]cell{{Text: "normal"}}, []int{8}, false, 0, index).Text
			if strings.Contains(got, "48;") {
				t.Errorf("normal row %d has background: %q", index, got)
			}
		}
	}
}

func TestTableHeaderBand(t *testing.T) {
	preserveTheme(t)
	for _, tc := range []struct {
		mode colormode.Mode
		bg   string
	}{{colormode.Dark, "48;5;234"}, {colormode.Light, "48;5;253"}, {colormode.None, ""}} {
		applyColorMode(tc.mode, false)
		line := tableRow([]cell{{Text: "job", Style: headerStyle}, {Text: "state", Style: headerStyle}}, []int{6, 7}, false, 0, -1)
		if got := ansi.Strip(line.Text); got != "  job    state  " {
			t.Fatalf("header geometry: %q", got)
		}
		if len(line.Animations) > 0 {
			t.Fatal("header animated")
		}
		bg := ""
		for _, token := range ansiTokens(line.Text) {
			if after, ok := strings.CutPrefix(token, "\x1b["); ok {
				params := strings.TrimSuffix(after, "m")
				if params == "0" || params == "49" {
					bg = ""
				}
				for _, candidate := range []string{"48;5;234", "48;5;253"} {
					if strings.Contains(params, candidate) {
						bg = candidate
					}
				}
				if tc.mode == colormode.None && (strings.Contains(params, "38;") || strings.Contains(params, "48;")) {
					t.Fatalf("colorless header has colors: %q", line.Text)
				}
			} else if bg != tc.bg {
				t.Fatalf("unbanded header %q: %q", token, line.Text)
			}
		}
	}
}

func TestLabelText(t *testing.T) {
	for _, tc := range []struct {
		labels []string
		width  int
		want   string
	}{
		{[]string{"weekly", "daily"}, 40, "[daily] [weekly]"},
		{[]string{"weekly", "daily", "example"}, 16, "[daily] +2"},
		{[]string{"bug", "prio-1"}, 5, "[2 labels]"},
		{nil, 40, "(no labels)"},
		{[]string{"\x1b[31mカタ", "bug"}, 11, "[bug] +1"},
	} {
		if got := labelText(tc.labels, tc.width); got != tc.want {
			t.Errorf("labels %q at %d: got %q, want %q", tc.labels, tc.width, got, tc.want)
		}
	}
}
