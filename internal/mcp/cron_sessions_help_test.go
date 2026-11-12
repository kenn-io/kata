package mcpserver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The advertised tool must explain that a later summary replaces all evidence.
func TestCronRunObserveHelpExplainsSessionReplacement(t *testing.T) {
	f := newCronFixture(t)
	tools, err := f.session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	for _, tool := range tools.Tools {
		if tool.Name != "kata.cron_run_observe" {
			continue
		}
		help := strings.ToLower(tool.Description)
		require.Contains(t, help, "summary")
		require.Contains(t, help, "replac")
		require.Contains(t, help, "session")
		require.Contains(t, help, "omit")
		require.Contains(t, help, "clear")
		return
	}
	t.Fatal("cron run observation tool was not advertised")
}
