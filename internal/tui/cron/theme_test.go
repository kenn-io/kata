package cron

import (
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	for _, c := range []struct {
		no, mode string
		want     colorMode
	}{{"", "", colorAuto}, {"", "LIGHT", colorLight}, {"", "dark", colorDark}, {"", "none", colorNone}, {"1", "dark", colorNone}, {"", " dark", colorAuto}, {"", "unexpected", colorAuto}} {
		got := resolveColorMode(func(k string) string {
			if k == "NO_COLOR" {
				return c.no
			}
			if k == "KATA_COLOR_MODE" {
				return c.mode
			}
			t.Fatalf("unexpected key %s", k)
			return ""
		})
		if got != c.want {
			t.Errorf("%q/%q = %v, want %v", c.no, c.mode, got, c.want)
		}
	}
}

func TestColorlessViewResetsColors(t *testing.T) {
	preserveTheme(t)
	m := newModel(Options{})
	m.Resize(120, 30)
	m.SetData(nativeFixture())
	applyColorMode(colorDark, true)
	colored := m.View().Content
	if !strings.Contains(colored, "38;5;") && !strings.Contains(colored, "38;2;") && !strings.Contains(colored, "48;5;") && !strings.Contains(colored, "48;2;") {
		t.Fatal("colored view did not exercise color rendering")
	}
	applyColorMode(colorNone, true)
	plain := m.View().Content
	if strings.Contains(plain, "38;5;") || strings.Contains(plain, "48;5;") || strings.Contains(plain, "38;2;") || strings.Contains(plain, "48;2;") {
		t.Fatal("color survived colorless mode", plain)
	}
	if !strings.Contains(plain, "Daily summary") || !strings.Contains(colored, "Daily summary") {
		t.Fatal("mode switch lost visible job")
	}
}
