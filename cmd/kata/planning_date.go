package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"

	"github.com/spf13/cobra"
)

func newDeadlineCmd() *cobra.Command {
	cmd := newPlanningDateCmd("deadline", "deadline_on")
	cmd.Long = `Set deadline_on. A deadline does not park the issue; use kata schedule for that.
"-" clears it. Formats: YYYY-MM-DD, local YYYY-MM-DDTHH:MM[:SS], or RFC 3339
UTC ending in Z. Relative words like "monday" are rejected; compute the date.
When it is reached, the owner (or author if unowned) gets an inbox request.`
	cmd.Example = `  kata deadline abc4 2026-11-15 --agent
  kata deadline abc4 - --agent`
	return cmd
}

func newScheduleCmd() *cobra.Command {
	cmd := newPlanningDateCmd("schedule", "scheduled_on")
	cmd.Long = `Set scheduled_on. Until that time the issue is left out of kata ready and
kata next. "-" clears it. For no date at all: kata meta set <ref> someday
true --json-value.
Formats: YYYY-MM-DD, local YYYY-MM-DDTHH:MM[:SS], or RFC 3339 UTC ending
in Z. Relative words like "monday" are rejected; compute the date.
When it is reached, the owner (or author if unowned) gets an inbox request.

--repeat '<rrule>' makes the issue the first occurrence of a recurrence that
starts on <date>, or changes the rule, start and zone of the one it already
belongs to. The rule is sent as written and the daemon validates it. The
zone is --timezone, else the issue's timezone metadata, else your own IANA
zone (TZ, then the /etc/localtime link); with none of those, pass --timezone.
Starting rewrites scheduled_on to the first occurrence, so a future date
takes the issue out of kata ready until then. --repeat - stops the
recurrence; occurrences already created stay.
An issue that parents a recurring cron job (cron.progress.* metadata) is
refused: closing it as done would pause the job. Use kata cron job create
--parent for work that repeats under an issue.`
	cmd.Example = `  kata schedule abc4 2026-11-02 --agent
  kata schedule abc4 2026-11-02T09:00 --agent
  kata schedule abc4 - --agent
  kata schedule abc4 2026-11-02 --repeat 'FREQ=WEEKLY;BYDAY=MO' --timezone Europe/Paris --agent
  kata schedule abc4 --repeat - --agent`
	cmd.Use = "schedule <issue-ref> [<date-or-time|->]"
	cmd.Args = cobra.RangeArgs(1, 2)
	var repeat, timezone string
	cmd.Flags().StringVar(&repeat, "repeat", "", "RRULE to repeat on from <date>, or - to stop repeating")
	cmd.Flags().StringVar(&timezone, "timezone", "", "IANA zone for --repeat (default: issue timezone metadata, then your zone)")
	setDate := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		ifMatch, _ := cmd.Flags().GetString("if-match")
		if !cmd.Flags().Changed("repeat") {
			if timezone != "" {
				return scheduleRepeatUsage("--timezone only applies with --repeat")
			}
			if len(args) != 2 {
				return scheduleRepeatUsage("kata schedule needs <issue-ref> and <date-or-time|->")
			}
			return setDate(cmd, args)
		}
		if err := validateMetaIfMatchFlag(cmd, ifMatch); err != nil {
			return err
		}
		return runScheduleRepeat(cmd, args, scheduleRepeatOptions{Rule: repeat, Timezone: timezone, IfMatch: ifMatch})
	}
	return cmd
}

func newPlanningDateCmd(name, metadataKey string) *cobra.Command {
	var ifMatch string
	short := "set or clear a due date (does not hide from ready)"
	if name == "schedule" {
		short = "park an issue until a date (hidden from ready until then)"
	}
	cmd := &cobra.Command{
		Use:   name + " <issue-ref> <date-or-time|->",
		Short: short,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateMetaIfMatchFlag(cmd, ifMatch); err != nil {
				return err
			}
			value := jsontext.Value("null")
			verb := "unset"
			if args[1] != "-" {
				encoded, err := json.Marshal(args[1])
				if err != nil {
					return err
				}
				value = encoded
				verb = "set"
			}
			return runMetaPatch(cmd, args[0], metadataKey, value, ifMatch, verb)
		},
	}
	cmd.Flags().StringVar(&ifMatch, "if-match", "", "expected issue revision (N or rev-N)")
	return cmd
}
