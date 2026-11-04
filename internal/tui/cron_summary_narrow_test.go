package tui

import (
	"fmt"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestCronSummaryNarrowDetail(t *testing.T) {
	for _, width := range []int{12, 16} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m := cronSummaryFixture(t)
			m.width = width
			_, _, lines := cronDetail(m, cronRef{cronRuns, m.cron.data.runs[0].UID})
			for _, line := range lines {
				require.LessOrEqual(t, ansi.StringWidth(line.text), width-2, "row %q", line.text)
			}
		})
	}
}
