package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/version"
	"go.kenn.io/kit/selfupdate"
)

type updateClient interface {
	Check(context.Context, selfupdate.CheckOptions) (*selfupdate.Info, error)
	Install(context.Context, *selfupdate.Info, selfupdate.InstallOptions) error
}

var newSelfUpdateClient = func(current string) (updateClient, error) {
	home, err := config.KataHome()
	if err != nil {
		return nil, err
	}
	return selfupdate.Client{
		Owner:                  "kenn-io",
		Repo:                   "kata",
		BinaryName:             "kata",
		CurrentVersion:         current,
		CacheDir:               filepath.Join(home, "cache", "update"),
		AllowUnsignedChecksums: true,
	}, nil
}

var updateInfoNeedsRefetch = func(info *selfupdate.Info) bool {
	return info.NeedsRefetch()
}

func newUpdateCmd() *cobra.Command {
	var checkOnly bool
	var force bool
	var yes bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "check for and install kata updates",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newSelfUpdateClient(version.Version)
			if err != nil {
				return err
			}
			opts := selfupdate.CheckOptions{Force: force}
			info, err := client.Check(cmd.Context(), opts)
			if err != nil {
				return err
			}
			if info == nil {
				return printUpdateResult(cmd, nil)
			}
			if checkOnly {
				return printUpdateResult(cmd, info)
			}
			if updateInfoNeedsRefetch(info) {
				opts.Force = true
				info, err = client.Check(cmd.Context(), opts)
				if err != nil {
					return err
				}
				if info == nil {
					return printUpdateResult(cmd, nil)
				}
			}
			if !yes {
				if err := confirmUpdate(cmd, info); err != nil {
					return err
				}
			}
			if err := client.Install(cmd.Context(), info, selfupdate.InstallOptions{}); err != nil {
				return &cliError{
					Message:  "install update: " + err.Error(),
					Kind:     kindInternal,
					ExitCode: ExitInternal,
				}
			}
			return printUpdateInstallResult(cmd, info)
		},
	}
	cmd.Flags().BoolVar(&checkOnly, "check", false, "check for updates without installing")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "force a fresh update check")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "install without prompting")
	return cmd
}

func confirmUpdate(cmd *cobra.Command, info *selfupdate.Info) error {
	out := cmd.ErrOrStderr()
	if _, err := fmt.Fprintf(out, "Install kata update %s -> %s? [y/N] ", currentUpdateVersion(info), latestUpdateVersion(info)); err != nil {
		return err
	}
	reader := bufio.NewReader(cmd.InOrStdin())
	answer, _ := reader.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	default:
		return &cliError{
			Message:  "update cancelled",
			Kind:     kindConfirm,
			ExitCode: ExitConfirm,
		}
	}
}

func printUpdateInstallResult(cmd *cobra.Command, info *selfupdate.Info) error {
	out := cmd.OutOrStdout()
	current := currentUpdateVersion(info)
	latest := latestUpdateVersion(info)
	switch currentOutputMode() {
	case outputAgent:
		_, err := fmt.Fprintf(out, "OK update installed=true current=%s latest=%s\n",
			agentValue(current), agentValue(latest))
		return err
	case outputJSON:
		var buf bytes.Buffer
		payload := map[string]any{
			"current_version":  current,
			"latest_version":   latest,
			"update_available": true,
			"installed":        true,
			"asset_name":       info.AssetName,
			"is_dev_build":     info.IsDevBuild,
		}
		if err := emitJSON(&buf, payload); err != nil {
			return err
		}
		_, err := fmt.Fprint(out, buf.String())
		return err
	default:
		_, err := fmt.Fprintf(out, "installed kata %s\n", latest)
		return err
	}
}

func printUpdateResult(cmd *cobra.Command, info *selfupdate.Info) error {
	out := cmd.OutOrStdout()
	current := version.Version
	latest := ""
	updateAvailable := info != nil
	assetName := ""
	isDevBuild := false
	if info != nil {
		current = currentUpdateVersion(info)
		latest = latestUpdateVersion(info)
		assetName = info.AssetName
		isDevBuild = info.IsDevBuild
	}
	switch currentOutputMode() {
	case outputAgent:
		_, err := fmt.Fprintf(out, "OK update update_available=%t current=%s latest=%s\n",
			updateAvailable, agentValue(current), agentValue(latest))
		return err
	case outputJSON:
		var buf bytes.Buffer
		payload := map[string]any{
			"current_version":  current,
			"latest_version":   latest,
			"update_available": updateAvailable,
			"asset_name":       assetName,
			"is_dev_build":     isDevBuild,
		}
		if err := emitJSON(&buf, payload); err != nil {
			return err
		}
		_, err := fmt.Fprint(out, buf.String())
		return err
	default:
		if info == nil {
			_, err := fmt.Fprintf(out, "kata is up to date (%s)\n", current)
			return err
		}
		_, err := fmt.Fprintf(out, "update available: %s -> %s\n", current, latest)
		return err
	}
}

func currentUpdateVersion(info *selfupdate.Info) string {
	if info != nil && info.CurrentVersion != "" {
		return info.CurrentVersion
	}
	return version.Version
}

func latestUpdateVersion(info *selfupdate.Info) string {
	if info != nil {
		return info.LatestVersion
	}
	return ""
}
