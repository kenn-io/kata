package main

import (
	"strings"
	"testing"
	_ "time/tzdata"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
)

func cronAuthorFixture() []string {
	return []string{"--name", "Inspection", "--title", "Inspect", "--enabled=false", "--catchup", "latest", "--overlap", "forbid", "--every", "1h", "--prompt", "Inspect the workspace", "--adapter", "example-adapter"}
}

func cronAuthorBuild(t *testing.T, parent string, args []string) (api.PutCronJobBody, error) {
	t.Helper()
	cmd := &cobra.Command{}
	var author cronAuthorOptions
	author.register(cmd)
	require.NoError(t, cmd.Flags().Parse(args))
	return author.build(cmd, parent)
}

func TestCronAuthorPromptMatchesFileDefinition(t *testing.T) {
	_, err := cronAuthorBuild(t, "", append(cronAuthorFixture(), "--adapter-agent= "))
	require.ErrorContains(t, err, "--adapter-agent must not be empty")
	body, err := cronAuthorBuild(t, "", append(cronAuthorFixture(), "--adapter-agent=example-agent", "--checkout-key=example-checkout"))
	require.NoError(t, err)
	require.Equal(t, api.PutCronJobBody{Name: "Inspection", Definition: api.CronJobDefinition{
		Version: 1, Kind: "job", Enabled: false,
		Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 3600},
		Action:  cron.Action{Kind: "execute", Prompt: "Inspect the workspace"},
		Issue:   &cron.IssuePolicy{Kind: "per-run", Title: "Inspect"},
		Overlap: "forbid", Catchup: "latest", CheckoutKey: "example-checkout", Options: api.JSONRawObject(`{"adapter":"example-adapter","agent":"example-agent"}`),
	}}, body)
}

func TestCronAuthorNativeLeavesParentResolutionToExistingPath(t *testing.T) {
	body, err := cronAuthorBuild(t, "abcd", []string{
		"--name", " Check-ins ", "--title", "Check in", "--body", "Literal ${text}",
		"--enabled", "--catchup", "skip", "--overlap", "allow", "--cron", "0 9 * * 1-5",
		"--timezone", "America/New_York", "--notify", "--recipient", "worker", "--message", "Hello", "--grace", "5m",
	})
	require.NoError(t, err)
	require.Equal(t, " Check-ins ", body.Name)
	require.Equal(t, cron.Action{Kind: "notify", Recipient: "worker", Message: "Hello"}, body.Definition.Action)
	require.Equal(t, &cron.IssuePolicy{Kind: "per-run", Title: "Check in", Body: "Literal ${text}"}, body.Definition.Issue)
	require.Equal(t, int64(300), body.Definition.GraceSeconds)
	require.Empty(t, body.Definition.Issue.ParentUID, "a short ref must never be stored as a UID")
}

func TestCronAuthorRejectsInvalidSelections(t *testing.T) {
	for _, tc := range []struct {
		name, flag, value, want string
	}{
		{"two schedules", "--cron", "@daily", "exactly one"},
		{"blank adapter", "--adapter", " ", "--prompt requires an explicit --adapter"},
		{"fractional grace", "--grace", "0.5s", "--grace must use whole seconds"},
		{"notify false", "--notify", "false", "notify"},
		{"mixed action", "--notify", "true", "exactly one"},
		{"native recipient on prompt", "--recipient", "worker", "notify"},
		{"native message on prompt", "--message", "hello", "notify"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append(cronAuthorFixture(), tc.flag+"="+tc.value)
			_, err := cronAuthorBuild(t, "", args)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestCronAuthorRequiresExplicitChoices(t *testing.T) {
	for _, tc := range []struct{ flag, want string }{
		{"--name", "typed creation requires explicit --name"},
		{"--title", "typed creation requires explicit --title"},
		{"--enabled", "typed creation requires explicit --enabled"},
		{"--catchup", "typed creation requires explicit --catchup"},
		{"--overlap", "typed creation requires explicit --overlap"},
		{"--every", "choose exactly one of --cron, --every or --once"},
		{"--prompt", "choose exactly one of --notify or --prompt"},
		{"--adapter", "--prompt requires an explicit --adapter"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			args := cronAuthorFixture()
			for i, arg := range args {
				if arg == tc.flag {
					args = append(args[:i], args[i+2:]...)
					break
				}
				if strings.HasPrefix(arg, tc.flag+"=") {
					args = append(args[:i], args[i+1:]...)
					break
				}
			}
			_, err := cronAuthorBuild(t, "", args)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestCronAuthorTrigger(t *testing.T) {
	for _, tc := range []struct {
		name, flag, value, timezone, want string
		trigger                           cron.Trigger
	}{
		{name: "missing timezone", flag: "--cron", value: "@daily", want: "--timezone requires an explicit IANA timezone"},
		{name: "local timezone", flag: "--cron", value: "@daily", timezone: "Local", want: "--timezone requires an explicit IANA timezone"},
		{name: "empty timezone", flag: "--cron", value: "@daily", timezone: "=", want: "--timezone requires an explicit IANA timezone"},
		{name: "timezone directive", flag: "--cron", value: "CRON_TZ=America/New_York 0 9 * * *", timezone: "UTC", want: "timezone directives remain available through --file"},
		{name: "trimmed timezone directive", flag: "--cron", value: "  TZ=Local @daily", timezone: "UTC", want: "timezone directives remain available through --file"},
		{name: "interval descriptor", flag: "--cron", value: "@every 1h", timezone: "UTC", want: "use --every for an interval"},
		{name: "once", flag: "--once", value: "2026-12-01T09:00:00+05:30", trigger: cron.Trigger{Kind: "once", At: "2026-12-01T09:00:00+05:30"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := cronAuthorFixture()
			for i := range args {
				if args[i] == "--every" {
					args[i], args[i+1] = tc.flag, tc.value
					break
				}
			}
			if tc.timezone != "" {
				args = append(args, "--timezone="+strings.TrimPrefix(tc.timezone, "="))
			}
			body, err := cronAuthorBuild(t, "", args)
			if tc.want != "" {
				require.ErrorContains(t, err, tc.want)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.trigger, body.Definition.Trigger)
			}
		})
	}
}

func TestCronAuthorWholeSecondsContract(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int64
	}{
		{"1h30m", 5400}, {"1.0000000001s", 1},
	} {
		got, err := cronAuthorSeconds(tc.value, "every")
		require.NoError(t, err)
		require.Equal(t, tc.want, got)
	}
}
