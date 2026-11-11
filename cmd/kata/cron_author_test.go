package main

import (
	"encoding/json/v2"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"pgregory.net/rapid"
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
	body, err := cronAuthorBuild(t, "", cronAuthorFixture())
	require.NoError(t, err)
	require.Equal(t, api.PutCronJobBody{Name: "Inspection", Definition: api.CronJobDefinition{
		Version: 1, Kind: "job", Enabled: false,
		Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 3600},
		Action:  cron.Action{Kind: "execute", Prompt: "Inspect the workspace"},
		Issue:   &cron.IssuePolicy{Kind: "per-run", Title: "Inspect"},
		Overlap: "forbid", Catchup: "latest", Options: api.JSONRawObject(`{"adapter":"example-adapter"}`),
	}}, body)
}

func TestCronAuthorNativeLeavesParentResolutionToExistingPath(t *testing.T) {
	body, err := cronAuthorBuild(t, "abcd", []string{
		"--name", " Check-ins ", "--title", "Check in", "--body", "Literal ${text}",
		"--enabled", "--catchup", "skip", "--overlap", "allow", "--cron", "0 9 * * 1-5",
		"--timezone", "America/New_York", "--notify", "--recipient", "worker", "--grace", "5m",
	})
	require.NoError(t, err)
	require.Equal(t, "Check-ins", body.Name)
	require.Equal(t, cron.Action{Kind: "notify", Recipient: "worker"}, body.Definition.Action)
	require.Equal(t, &cron.IssuePolicy{Kind: "per-run", Title: "Check in", Body: "Literal ${text}"}, body.Definition.Issue)
	require.Equal(t, int64(300), body.Definition.GraceSeconds)
	require.Empty(t, body.Definition.Issue.ParentUID, "a short ref must never be stored as a UID")
}

func TestCronAuthorRejectsInvalidSelections(t *testing.T) {
	for _, tc := range []struct {
		name, flag, value, want string
	}{
		{"two schedules", "--cron", "@daily", "exactly one"},
		{"fractional interval", "--every", "1500ms", "whole seconds"},
		{"zero interval", "--every", "0s", "positive"},
		{"overflow interval", "--every", "9223372036854775807s", "duration"},
		{"local zone", "--timezone", "Local", "timezone"},
		{"empty zone", "--timezone", "", "timezone"},
		{"bad zone", "--timezone", "Invalid/Zone", "timezone"},
		{"blank name", "--name", "  ", "name"},
		{"long name", "--name", strings.Repeat("n", 257), "name"},
		{"blank title", "--title", " ", "title"},
		{"blank prompt", "--prompt", " ", "prompt"},
		{"blank adapter", "--adapter", " ", "adapter"},
		{"bad catchup", "--catchup", "sometimes", "catchup"},
		{"bad overlap", "--overlap", "maybe", "overlap"},
		{"fractional grace", "--grace", "0.5s", "whole seconds"},
		{"negative timeout", "--timeout", "-1s", "nonnegative"},
		{"notify false", "--notify", "false", "notify"},
		{"mixed action", "--notify", "true", "exactly one"},
		{"native recipient on prompt", "--recipient", "worker", "notify"},
		{"native message on prompt", "--message", "hello", "notify"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append(cronAuthorFixture(), tc.flag+"="+tc.value)
			body, err := cronAuthorBuild(t, "", args)
			if err == nil {
				err = body.Definition.Native().Validate()
			}
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestCronAuthorRequiresExplicitChoices(t *testing.T) {
	for _, flag := range []string{"--name", "--title", "--enabled", "--catchup", "--overlap", "--every", "--prompt", "--adapter"} {
		t.Run(flag, func(t *testing.T) {
			args := cronAuthorFixture()
			for i, arg := range args {
				if arg == flag {
					args = append(args[:i], args[i+2:]...)
					break
				}
				if strings.HasPrefix(arg, flag+"=") {
					args = append(args[:i], args[i+1:]...)
					break
				}
			}
			_, err := cronAuthorBuild(t, "", args)
			require.Error(t, err)
		})
	}
}

func TestCronAuthorCronRequiresExplicitTimezone(t *testing.T) {
	args := cronAuthorFixture()
	for i := range args {
		if args[i] == "--every" {
			args[i], args[i+1] = "--cron", "@daily"
		}
	}
	_, err := cronAuthorBuild(t, "", args)
	require.ErrorContains(t, err, "timezone")
}

func TestCronAuthorCronHasOneTimezoneSource(t *testing.T) {
	for _, expression := range []string{
		"CRON_TZ=America/New_York 0 9 * * *", "TZ=UTC @daily", "  TZ=Local @daily", "@every 1h",
	} {
		args := cronAuthorFixture()
		for i := range args {
			if args[i] == "--every" {
				args[i], args[i+1] = "--cron", expression
			}
		}
		_, err := cronAuthorBuild(t, "", append(args, "--timezone=UTC"))
		require.Error(t, err, expression)
	}
}

func TestCronAuthorOncePreservesExplicitInstant(t *testing.T) {
	args := cronAuthorFixture()
	for i := range args {
		if args[i] == "--every" {
			args[i], args[i+1] = "--once", "2026-12-01T09:00:00+05:30"
		}
	}
	body, err := cronAuthorBuild(t, "", args)
	require.NoError(t, err)
	require.Equal(t, cron.Trigger{Kind: "once", At: "2026-12-01T09:00:00+05:30"}, body.Definition.Trigger)
}

func TestCronAuthorWholeSecondsContract(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		seconds := rapid.Int64().Draw(rt, "seconds")
		allowZero := rapid.Bool().Draw(rt, "allow zero")
		got, err := cronAuthorSeconds(fmt.Sprintf("%ds", seconds), "every", allowZero)
		valid := seconds >= 0 && seconds <= math.MaxInt64/int64(time.Second) && (allowZero || seconds > 0)
		if valid {
			require.NoError(t, err)
			require.Equal(t, seconds, got)
		} else {
			require.Error(t, err)
		}
	})
}

func TestCronAuthorDurationUsesNanosecondPrecision(t *testing.T) {
	seconds, err := cronAuthorSeconds("1.0000000001s", "grace", true)
	require.NoError(t, err)
	require.Equal(t, int64(1), seconds)
}

func TestCronAuthorEquivalentWholeSecondDurations(t *testing.T) {
	for _, value := range []string{"1h30m", "1.5h", "+5400s", "5400000ms", "5400000000us", "5400000000000ns", "0.5s500ms5399s"} {
		seconds, err := cronAuthorSeconds(value, "every", false)
		require.NoError(t, err, value)
		require.Equal(t, int64(5400), seconds, value)
	}
}

func TestCronAuthorPromptTextIsLiteral(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		text := "Inspect " + rapid.String().Draw(rt, "prompt")
		cmd := &cobra.Command{}
		var author cronAuthorOptions
		author.register(cmd)
		if err := cmd.Flags().Parse(append(cronAuthorFixture(), "--prompt="+text)); err != nil {
			rt.Fatal(err)
		}
		body, err := author.build(cmd, "")
		if err != nil {
			rt.Fatal(err)
		}
		raw, err := json.Marshal(body)
		if err != nil {
			rt.Fatal(err)
		}
		var readback api.PutCronJobBody
		if err := json.Unmarshal(raw, &readback); err != nil || readback.Definition.Action.Prompt != text {
			rt.Fatalf("literal prompt lost through JSON: error %v", err)
		}
	})
}
