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
		GitHubToken:            selfupdate.EnvironmentGitHubToken(),
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
			currentIsDevBuild := selfupdate.IsDevBuildVersion(version.Version)
			client, err := newSelfUpdateClient(version.Version)
			if err != nil {
				return err
			}
			opts := selfupdate.CheckOptions{Force: force || currentIsDevBuild}
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
			if currentOutputMode() == outputHuman && !yes {
				if err := printUpdateSummary(cmd, info); err != nil {
					return err
				}
			}
			if info.IsDevBuild && !force {
				return printDevBuildForceHint(cmd, info)
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

func printUpdateSummary(cmd *cobra.Command, info *selfupdate.Info) error {
	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(out, "\nCurrent version: %s\nLatest version:  %s\n",
		currentUpdateVersion(info), latestUpdateVersion(info)); err != nil {
		return err
	}
	if info.IsDevBuild {
		if _, err := fmt.Fprintln(out, "\nYou're running a dev build. Latest official release available."); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintln(out, "\nUpdate available."); err != nil {
		return err
	}
	if info.DownloadURL == "" && info.Size == 0 && info.Checksum == "" {
		_, err := fmt.Fprintln(out)
		return err
	}
	if _, err := fmt.Fprintln(out, "\nDownload:"); err != nil {
		return err
	}
	if info.DownloadURL != "" {
		if _, err := fmt.Fprintf(out, "  URL:    %s\n", info.DownloadURL); err != nil {
			return err
		}
	}
	if info.Size > 0 {
		if _, err := fmt.Fprintf(out, "  Size:   %s\n", selfupdate.FormatSize(info.Size)); err != nil {
			return err
		}
	}
	if info.Checksum != "" {
		if _, err := fmt.Fprintf(out, "  SHA256: %s\n", info.Checksum); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(out)
	return err
}

func printDevBuildForceHint(cmd *cobra.Command, info *selfupdate.Info) error {
	switch currentOutputMode() {
	case outputHuman:
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "Use 'kata update --force' to install the latest official release.")
		return err
	case outputAgent, outputJSON:
		return printUpdateResult(cmd, info)
	default:
		return nil
	}
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
		if info != nil && info.IsDevBuild {
			_, err := fmt.Fprintf(out, "dev build: %s\nlatest official release: %s\nUse 'kata update --force' to install the latest official release.\n", current, latest)
			return err
		}
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
