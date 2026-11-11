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

func TestCronAuthorPromptDefinition(t *testing.T) {
	body, err := cronAuthorBuild(t, "", append(cronAuthorFixture(), "--adapter-agent=example-agent", "--checkout-key=example-checkout"))
	require.NoError(t, err)
	require.JSONEq(t, `{"adapter":"example-adapter","agent":"example-agent"}`, string(body.Definition.Options))
	body.Definition.Options = nil
	require.Equal(t, api.PutCronJobBody{Name: "Inspection", Definition: api.CronJobDefinition{
		Version: 1, Kind: "job", Enabled: false,
		Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 3600},
		Action:  cron.Action{Kind: "execute", Prompt: "Inspect the workspace"},
		Issue:   &cron.IssuePolicy{Kind: "per-run", Title: "Inspect"},
		Overlap: "forbid", Catchup: "latest", CheckoutKey: "example-checkout",
	}}, body)
}

func TestCronAuthorNativeDefinition(t *testing.T) {
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
}

func TestCronAuthorRejectsInvalidSelections(t *testing.T) {
	for _, tc := range []struct {
		name, flag, value, want string
		drop                    bool
	}{
		{name: "two schedules", flag: "--cron", value: "@daily", want: "choose exactly one of --cron, --every or --once"},
		{name: "padded adapter", flag: "--adapter", value: " example-adapter ", want: "--adapter must be a non-empty selector without surrounding spaces"},
		{name: "padded agent", flag: "--adapter-agent", value: " example-agent ", want: "--adapter-agent must be a non-empty selector without surrounding spaces"},
		{name: "mixed action", flag: "--notify", value: "true", want: "choose exactly one of --notify or --prompt"},
		{name: "native recipient on prompt", flag: "--recipient", value: "worker", want: "--parent, --recipient and --message require --notify"},
		{name: "native message on prompt", flag: "--message", value: "hello", want: "--parent, --recipient and --message require --notify"},
		{name: "missing name", flag: "--name", want: "typed creation requires explicit --name", drop: true},
		{name: "missing title", flag: "--title", want: "typed creation requires explicit --title", drop: true},
		{name: "missing enabled", flag: "--enabled", want: "typed creation requires explicit --enabled", drop: true},
		{name: "missing catchup", flag: "--catchup", want: "typed creation requires explicit --catchup", drop: true},
		{name: "missing overlap", flag: "--overlap", want: "typed creation requires explicit --overlap", drop: true},
		{name: "missing schedule", flag: "--every", want: "choose exactly one of --cron, --every or --once", drop: true},
		{name: "missing prompt", flag: "--prompt", want: "choose exactly one of --notify or --prompt", drop: true},
		{name: "missing adapter", flag: "--adapter", want: "--adapter must be a non-empty selector without surrounding spaces", drop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := cronAuthorFixture()
			if tc.drop {
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
			} else {
				args = append(args, tc.flag+"="+tc.value)
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
		{name: "empty timezone", flag: "--every", value: "1h", timezone: "=", want: "--timezone requires an explicit IANA timezone"},
		{name: "timezone directive", flag: "--cron", value: "CRON_TZ=America/New_York 0 9 * * *", timezone: "UTC", want: "timezone directives remain available through --file"},
		{name: "trimmed timezone directive", flag: "--cron", value: "  TZ=Local @daily", timezone: "UTC", want: "timezone directives remain available through --file"},
		{name: "interval descriptor", flag: "--cron", value: "@every 1h", timezone: "UTC", trigger: cron.Trigger{Kind: "cron", Cron: "@every 1h", Timezone: "UTC"}},
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
	for _, flag := range []string{"every", "grace"} {
		_, err := cronAuthorBuild(t, "", append(cronAuthorFixture(), "--"+flag+"=0.5s"))
		require.ErrorContains(t, err, "--"+flag+" must use whole seconds")
	}
}
