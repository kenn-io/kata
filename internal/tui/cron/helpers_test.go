package cron

import "testing"

func preserveTheme(t testing.TB) {
	t.Helper()
	mode, dark := activeColorMode, activeHasDarkBackground
	t.Cleanup(func() { applyColorMode(mode, dark) })
}
