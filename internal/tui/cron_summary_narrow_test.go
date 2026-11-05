package tui

import (
	"fmt"
	"strings"
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

// Wide evidence needs two cells even when one remains beside the label column.
func TestCronSummaryNarrowUnicode(t *testing.T) {
	for _, width := range []int{15, 17} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m := cronSummaryFixture(t)
			m.width = width
			m.cron.data.runs[0].Summary.Usage.Model = "模型"
			m.cron.data.runs[0].Summary.Steps[0].Message = new("步骤")
			_, _, lines := cronDetail(m, cronRef{cronRuns, m.cron.data.runs[0].UID})
			var evidence strings.Builder
			for _, line := range lines {
				require.LessOrEqual(t, ansi.StringWidth(line.text), width-2, "row %q", line.text)
				evidence.WriteString(strings.Join(strings.Fields(line.text), ""))
			}
			require.Contains(t, evidence.String(), "模型")
			require.Contains(t, evidence.String(), "步骤")
		})
	}
}
