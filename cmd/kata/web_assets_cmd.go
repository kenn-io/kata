package main

import (
	"go.kenn.io/kata/internal/web"

	"github.com/spf13/cobra"
)

func newWebAssetsCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "_web-assets-check",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return web.ValidateEmbeddedRelease()
		},
	}
}
