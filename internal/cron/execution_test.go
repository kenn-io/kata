package cron_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"hegel.dev/go/hegel"
)

func TestParseExecuteMode(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  cron.ExecuteMode
		ok    bool
	}{
		{input: "none", want: cron.ModeNone, ok: true},
		{input: "owned", want: cron.ModeOwned, ok: true},
		{input: "all", want: cron.ModeAll, ok: true},
		{input: "", ok: false},
		{input: "future", ok: false},
	} {
		got, ok := cron.ParseExecuteMode(tc.input)
		require.Equal(t, tc.want, got)
		require.Equal(t, tc.ok, ok)
	}
}

// The replica execution contract defines all and none independently of identity.
func TestExecutionEligibleUnconditionalModes(t *testing.T) {
	for _, mode := range []cron.ExecuteMode{"none", "all"} {
		t.Run(string(mode), func(t *testing.T) {
			hegel.Test(t, func(ht *hegel.T) {
				actor, author := hegel.Draw(ht, hegel.Text()), hegel.Draw(ht, hegel.Text())
				require.Equal(ht, mode == "all", cron.ExecutionEligible(mode, actor, author))
			}, hegel.WithTestCases(1000))
		})
	}
}

func TestExecutionEligibleOwnedExactNonblankIdentity(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		actor := hegel.Draw(ht, hegel.OneOf(hegel.Text(), hegel.SampledFrom([]string{"", " ", "\t\n", "\u2003", "member-a", "Member-A"})))
		author := hegel.Draw(ht, hegel.OneOf(hegel.Text(), hegel.Just(actor), hegel.Just(strings.ToUpper(actor))))
		require.Equal(ht, strings.TrimSpace(actor) != "" && actor == author, cron.ExecutionEligible("owned", actor, author))
	}, hegel.WithTestCases(1000))
}

func TestExecutionEligibleUnknownModesDeny(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		mode := hegel.Draw(ht, hegel.Text())
		if mode == "none" || mode == "owned" || mode == "all" {
			return
		}
		actor := hegel.Draw(ht, hegel.Text())
		require.False(ht, cron.ExecutionEligible(cron.ExecuteMode(mode), actor, actor))
	}, hegel.WithTestCases(1000))
}
