package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/harness"
	"go.kenn.io/kata/internal/teammate"
)

type teammateResolution struct {
	Handle, Source, Harness string
}

type teammateResolutionKey struct{}
type cachedTeammateResolution struct {
	resolution teammateResolution
	err        error
}

var detectTeammateHarness = func() (harness.Detection, harness.Outcome) {
	return harness.Detect(os.LookupEnv, harness.ProcessAncestry(), "")
}

func teammateValidation(err error) error {
	return &cliError{Message: err.Error(), Kind: kindValidation, ExitCode: ExitValidation}
}

func explicitTeammateResolution(cmd *cobra.Command) (teammateResolution, error) {
	value, present := os.LookupEnv("KATA_TEAMMATE")
	source := "env"
	if cmd.Flags().Changed("teammate") {
		value, _ = cmd.Flags().GetString("teammate")
		present, source = true, "flag"
	}
	if !present {
		return teammateResolution{Source: "none"}, nil
	}
	if err := teammate.Validate(value); err != nil {
		return teammateResolution{Handle: value, Source: source}, teammateValidation(err)
	}
	return teammateResolution{Handle: value, Source: source}, nil
}

// explicitTeammate selects reply/inbox addressing without harness inference.
func explicitTeammate(cmd *cobra.Command) (string, error) {
	if ctx := cmd.Context(); ctx != nil {
		if cached, ok := ctx.Value(teammateResolutionKey{}).(cachedTeammateResolution); ok {
			if cached.resolution.Source == "flag" || cached.resolution.Source == "env" {
				return cached.resolution.Handle, cached.err
			}
			return "", nil
		}
	}
	result, err := explicitTeammateResolution(cmd)
	return result.Handle, err
}

func teammateInferenceMode() (harness.Mode, error) {
	if value, present := os.LookupEnv("KATA_TEAMMATE_INFER"); present {
		mode, err := harness.ParseMode(value)
		if err != nil {
			return "", fmt.Errorf("KATA_TEAMMATE_INFER: %w", err)
		}
		return mode, nil
	}
	cfg, err := config.ReadTeammateConfig()
	if err != nil {
		return "", err
	}
	return harness.Mode(cfg.Infer), nil
}

// resolveTeammate freezes attribution once per command, including any error.
// Root pre-run caches errors for attribution consumers without blocking reads
// or configuration diagnostics that do not need teammate attribution.
func resolveTeammate(cmd *cobra.Command) (teammateResolution, error) {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if cached, ok := ctx.Value(teammateResolutionKey{}).(cachedTeammateResolution); ok {
		return cached.resolution, cached.err
	}
	result, err := resolveUncachedTeammate(cmd)
	cmd.SetContext(context.WithValue(ctx, teammateResolutionKey{}, cachedTeammateResolution{result, err}))
	return result, err
}

func resolveUncachedTeammate(cmd *cobra.Command) (teammateResolution, error) {
	explicit, err := explicitTeammateResolution(cmd)
	if err != nil || explicit.Source != "none" {
		return explicit, err
	}
	// An HTTP MCP process may serve unrelated clients; its host harness is not
	// a startup identity for those clients. Stdio has one invoking session.
	if cmd.Name() == "serve" && cmd.Parent() != nil && cmd.Parent().Name() == "mcp" {
		if address, _ := cmd.Flags().GetString("http"); address != "" {
			return explicit, nil
		}
	}
	mode, err := teammateInferenceMode()
	if err != nil {
		return teammateResolution{}, teammateValidation(err)
	}
	if mode == harness.ModeOff {
		return teammateResolution{Source: "disabled"}, nil
	}
	detection, outcome := detectTeammateHarness()
	switch outcome {
	case harness.Detected:
		return teammateResolution{Handle: harness.Handle(detection, mode), Source: "inferred", Harness: detection.Harness}, nil
	case harness.Ambiguous:
		return teammateResolution{Source: "ambiguous"}, nil
	default:
		return teammateResolution{Source: "none"}, nil
	}
}

func preflightCommentTeammate(cmd *cobra.Command, resolution teammateResolution) (string, error) {
	if resolution.Handle == "" {
		return "", nil
	}
	a, err := dialDaemon(cmd.Context())
	if err != nil {
		return "", err
	}
	err = requireDaemonAPIVersion(a.ctx, a.client, a.baseURL, "0.18.0", "comment teammate attribution")
	if err != nil && resolution.Source == "inferred" {
		var cli *cliError
		if errors.As(err, &cli) && cli.Code == "daemon_api_too_old" {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "teammate: daemon too old for teammate attribution; writing without it")
			return "", nil
		}
	}
	return resolution.Handle, err
}

// Resolve once before the primary mutation, preserving attribution on retries.
func prepareFollowupComment(cmd *cobra.Command) (string, string, error) {
	body, err := commentFromFlag(cmd)
	if err != nil || body == "" {
		return body, "", err
	}
	resolution, err := resolveTeammate(cmd)
	if err != nil {
		return "", "", err
	}
	handle, err := preflightCommentTeammate(cmd, resolution)
	if err != nil {
		return "", "", err
	}
	return body, handle, nil
}
