package cron

import (
	"strings"
	"testing"

	"go.kenn.io/kata/internal/colormode"
)

func TestColorlessViewResetsColors(t *testing.T) {
	preserveTheme(t)
	m := newModel(Options{})
	m.Resize(120, 30)
	m.SetData(nativeFixture())
	applyColorMode(colormode.Dark, true)
	colored := m.View().Content
	if !strings.Contains(colored, "38;5;") && !strings.Contains(colored, "38;2;") && !strings.Contains(colored, "48;5;") && !strings.Contains(colored, "48;2;") {
		t.Fatal("colored view did not exercise color rendering")
	}
	applyColorMode(colormode.None, true)
	plain := m.View().Content
	if strings.Contains(plain, "38;5;") || strings.Contains(plain, "48;5;") || strings.Contains(plain, "38;2;") || strings.Contains(plain, "48;2;") {
		t.Fatal("color survived colorless mode", plain)
	}
	if !strings.Contains(plain, "Daily summary") || !strings.Contains(colored, "Daily summary") {
		t.Fatal("mode switch lost visible job")
	}
}
