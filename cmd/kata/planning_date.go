package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"

	"github.com/spf13/cobra"
)

func newDeadlineCmd() *cobra.Command {
	return newPlanningDateCmd("deadline", "deadline_on")
}

func newScheduleCmd() *cobra.Command {
	return newPlanningDateCmd("schedule", "scheduled_on")
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
