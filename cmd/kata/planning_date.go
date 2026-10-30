package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"

	"github.com/spf13/cobra"
)

func newDeadlineCmd() *cobra.Command {
	var ifMatch string
	cmd := &cobra.Command{
		Use:   "deadline <issue-ref> <date-or-time|->",
		Short: "set or clear a due date (does not hide from ready)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateMetaIfMatchFlag(cmd, ifMatch); err != nil {
				return err
			}
			return runPlanningDate(cmd, args, "deadline_on", ifMatch)
		},
	}
	cmd.Flags().StringVar(&ifMatch, "if-match", "", "expected issue revision (N or rev-N)")
	cmd.Long = `Set deadline_on. A deadline does not park the issue; use kata schedule for that.
"-" clears it. Formats: YYYY-MM-DD, local YYYY-MM-DDTHH:MM[:SS], or RFC 3339
UTC ending in Z. Relative words like "monday" are rejected; compute the date.
When it is reached, the owner (or author if unowned) gets an inbox request.`
	cmd.Example = `  kata deadline abc4 2026-11-15 --agent
  kata deadline abc4 - --agent`
	return cmd
}

func newScheduleCmd() *cobra.Command {
	var ifMatch, repeat, timezone string
	cmd := &cobra.Command{
		Use:   "schedule <issue-ref> [<date-or-time|->]",
		Short: "park an issue until a date (hidden from ready until then)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateMetaIfMatchFlag(cmd, ifMatch); err != nil {
				return err
			}
			if !cmd.Flags().Changed("repeat") {
				if timezone != "" {
					return scheduleRepeatUsage("--timezone only applies with --repeat")
				}
				if len(args) != 2 {
					return scheduleRepeatUsage("kata schedule needs <issue-ref> and <date-or-time|->")
				}
				return runPlanningDate(cmd, args, "scheduled_on", ifMatch)
			}
			return runScheduleRepeat(cmd, args, scheduleRepeatOptions{Rule: repeat, Timezone: timezone, IfMatch: ifMatch})
		},
		Long: `Set scheduled_on. Until that time the issue is left out of kata ready and
kata next. "-" clears it. For no date at all: kata meta set <ref> someday
true --json-value.
Formats: YYYY-MM-DD, local YYYY-MM-DDTHH:MM[:SS], or RFC 3339 UTC ending
in Z. Relative words like "monday" are rejected; compute the date.
When it is reached, the owner (or author if unowned) gets an inbox request.

--repeat '<rrule>' makes the issue the first occurrence of a recurrence that
starts on <date>, or changes the rule, start and zone of the one it already
belongs to. The rule is sent as written and the daemon validates it. The
zone is --timezone, else the recurrence's own zone when changing, else the
issue's timezone metadata, else your own IANA zone (TZ, then the
/etc/localtime link); with none of those, pass --timezone.
Starting rewrites scheduled_on to the first occurrence, so a future date
takes the issue out of kata ready until then. --repeat - stops the
recurrence; occurrences already created stay, and the issue cannot start a
new recurrence afterwards: start it from a new issue.
An issue that parents a recurring cron job (cron.progress.* metadata) is
refused: closing it as done would pause the job. Use kata cron job create
--parent for work that repeats under an issue.`,
		Example: `  kata schedule abc4 2026-11-02 --agent
  kata schedule abc4 2026-11-02T09:00 --agent
  kata schedule abc4 - --agent
  kata schedule abc4 2026-11-02 --repeat 'FREQ=WEEKLY;BYDAY=MO' --timezone Europe/Paris --agent
  kata schedule abc4 --repeat - --agent`,
	}
	cmd.Flags().StringVar(&ifMatch, "if-match", "", "expected issue revision (N or rev-N)")
	cmd.Flags().StringVar(&repeat, "repeat", "", "RRULE to repeat on from <date>, or - to stop repeating")
	cmd.Flags().StringVar(&timezone, "timezone", "", "IANA zone for --repeat (default: the recurrence's zone, issue timezone metadata, then your zone)")
	return cmd
}

func runPlanningDate(cmd *cobra.Command, args []string, metadataKey, ifMatch string) error {
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
}
