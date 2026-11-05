package editorcmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolve(t *testing.T) {
	for _, tt := range []struct {
		name, visual, editor string
		want                 []string
	}{
		{"default", "", "", []string{"vi"}},
		{"editor arguments", "", "nvim --wait", []string{"nvim", "--wait"}},
		{"visual precedence", "code --wait", "nvim", []string{"code", "--wait"}},
		{"blank visual", " \t\n", "emacs -nw", []string{"emacs", "-nw"}},
		{"blank values", " \t", "\n ", []string{"vi"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("VISUAL", tt.visual)
			t.Setenv("EDITOR", tt.editor)
			require.Equal(t, tt.want, Resolve())
		})
	}
}
