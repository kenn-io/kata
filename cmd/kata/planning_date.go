package main

import (
	"encoding/json"

	"github.com/spf13/cobra"
)

func newDeadlineCmd() *cobra.Command {
	return newPlanningDateCmd("deadline", "deadline_on", "deadline")
}

func newScheduleCmd() *cobra.Command {
	return newPlanningDateCmd("schedule", "scheduled_on", "schedule")
}

func newPlanningDateCmd(name, metadataKey, noun string) *cobra.Command {
	var ifMatch string
	cmd := &cobra.Command{
		Use:   name + " <issue-ref> <date-or-time|->",
		Short: "set or clear an issue " + noun,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateMetaIfMatchFlag(cmd, ifMatch); err != nil {
				return err
			}
			value := json.RawMessage("null")
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
