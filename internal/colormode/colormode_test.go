package colormode

import "testing"

func TestResolve(t *testing.T) {
	for _, c := range []struct {
		no, mode string
		want     Mode
	}{{"", "", Auto}, {"", "LIGHT", Light}, {"", "dark", Dark}, {"", "none", None}, {"1", "dark", None}, {"", " dark", Auto}, {"", "unexpected", Auto}} {
		t.Setenv("NO_COLOR", c.no)
		t.Setenv("KATA_COLOR_MODE", c.mode)
		if got := Resolve(); got != c.want {
			t.Errorf("%q/%q = %v, want %v", c.no, c.mode, got, c.want)
		}
	}
}
