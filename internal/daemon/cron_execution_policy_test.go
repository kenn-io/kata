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
		{name: "none", mode: "none", member: "member-a", author: "member-a"},
		{name: "owned", mode: "owned", member: "member-a", author: "member-a", want: true},
		{name: "other author", mode: "owned", member: "member-a", author: "member-b"},
		{name: "case differs", mode: "owned", member: "member-a", author: "Member-A"},
		{name: "empty member", mode: "owned"},
		{name: "blank member", mode: "owned", member: "\u2003", author: "\u2003"},
		{name: "exact spaces", mode: "owned", member: " member-a ", author: " member-a ", want: true},
		{name: "spaces differ", mode: "owned", member: " member-a ", author: "member-a"},
		{name: "unknown mode", mode: "future", member: "member-a", author: "member-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, executionEligible(config.CronExecuteMode(tc.mode), tc.member, tc.author))
		})
	}
}
