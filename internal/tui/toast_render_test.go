package tui

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRenderToastUsesErrorStyle(t *testing.T) {
	applyColorMode(colorDark, true)
	t.Cleanup(applyDefaultColorMode)
	message := "Job data changed; press p after refresh"
	require.Equal(t, errorStyle.Render(message), renderToast(&toast{text: message, level: toastError}))
	for _, level := range []toastLevel{toastInfo, toastSuccess} {
		require.Equal(t, toastStyle.Render(message), renderToast(&toast{text: message, level: level}))
	}
	require.Empty(t, renderToast(nil))
}
