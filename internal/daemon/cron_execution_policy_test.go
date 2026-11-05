package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestExecutionEligible(t *testing.T) {
	for _, tc := range []struct {
		name, mode, member, author string
		want                       bool
	}{
		{name: "owned author", mode: "owned", member: "member-a", author: "member-a", want: true},
		{name: "spaces differ", mode: "owned", member: " member-a ", author: "member-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, executionEligible(config.CronExecuteMode(tc.mode), tc.member, tc.author))
		})
	}
}
