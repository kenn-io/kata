package main

import (
	"github.com/spf13/cobra"
)

func newReopenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "reopen <issue-ref>",
		Short:   "reopen a closed issue",
		Long:    `Reopen a closed issue when work remains or a regression appears. Add --comment to explain why.`,
		Example: `  kata reopen abc4 --comment "Regressed in 0.15; repro attached." --agent`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAction(cmd, args[0], "reopen", nil)
		},
	}
	addCommentFlag(cmd)
	return cmd
}
