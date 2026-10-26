package main

import (
	"context"
	"os"

	"github.com/spf13/cobra"

	"go.kenn.io/kata/internal/teammate"
)

type teammateResolutionKey struct{}
type teammateResolution struct {
	handle string
	err    error
}

// resolveTeammate keeps an explicitly empty flag distinct from inheritance.
func resolveTeammate(cmd *cobra.Command) (string, error) {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if cached, ok := ctx.Value(teammateResolutionKey{}).(teammateResolution); ok {
		return cached.handle, cached.err
	}
	var override *string
	if cmd.Flags().Changed("teammate") {
		value, _ := cmd.Flags().GetString("teammate")
		override = &value
	}
	value, err := teammate.Resolve(override, os.Getenv("KATA_TEAMMATE"))
	if err != nil {
		err = &cliError{Message: err.Error(), Kind: kindValidation, ExitCode: ExitValidation}
	}
	ctx = context.WithValue(ctx, teammateResolutionKey{}, teammateResolution{value, err})
	cmd.SetContext(teammate.WithRequest(ctx, value))
	return value, err
}

func preflightCommentTeammate(cmd *cobra.Command, handle string) error {
	if handle == "" {
		return nil
	}
	a, err := dialDaemon(cmd.Context())
	if err != nil {
		return err
	}
	return requireDaemonAPIVersion(a.ctx, a.client, a.baseURL, "0.18.0", "comment teammate attribution")
}

// Resolve once before the primary mutation, preserving attribution on retries.
func prepareFollowupComment(cmd *cobra.Command) (string, string, error) {
	body, err := commentFromFlag(cmd)
	if err != nil || body == "" {
		return body, "", err
	}
	handle, err := resolveTeammate(cmd)
	if err != nil {
		return "", "", err
	}
	if err := preflightCommentTeammate(cmd, handle); err != nil {
		return "", "", err
	}
	return body, handle, nil
}
