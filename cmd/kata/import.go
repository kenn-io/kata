package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/jsonl"
)

func newImportCmd() *cobra.Command {
	var input string
	var target string
	var force bool
	var newInstance bool
	var sourceFormat string
	cmd := &cobra.Command{
		Use:   "import",
		Short: "import a kata database export",
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := resolveImportSourceFormat(cmd, sourceFormat)
			if err != nil {
				return err
			}
			switch strings.TrimSpace(format) {
			case "", "kata":
				return runKataJSONLImport(cmd, input, target, force, newInstance)
			case "beads":
				if err := validateBeadsImportFlags(cmd); err != nil {
					return err
				}
				return runBeadsImport(cmd)
			default:
				return &cliError{
					Message:  fmt.Sprintf("unsupported import format %q", format),
					Kind:     kindValidation,
					ExitCode: ExitValidation,
				}
			}
		},
	}
	cmd.Flags().StringVar(&sourceFormat, "source-format", "kata", "import source format (kata or beads)")
	cmd.Flags().StringVar(&input, "input", "", "path to JSONL export")
	cmd.Flags().StringVar(&target, "target", "", "database path to create")
	cmd.Flags().BoolVar(&force, "force", false, "replace existing target database")
	cmd.Flags().BoolVar(&newInstance, "new-instance", false,
		"keep the target's fresh meta.instance_uid instead of overwriting it with the source's; preserves imported events' origin_instance_uid for federation loop-detection")
	return cmd
}

func resolveImportSourceFormat(cmd *cobra.Command, sourceFormat string) (string, error) {
	// During the import flag migration, root --format values human|json|agent
	// select output mode, while kata|beads are temporary legacy import source
	// values. The sets are intentionally disjoint so this fallback can be
	// removed after the deprecation window without ambiguity.
	legacy := legacyImportSourceFormat()
	if isImportLegacySourceFormat(legacy) {
		if cmd.Flags().Changed("source-format") {
			return "", &cliError{
				Message:  fmt.Sprintf("--format %s cannot be combined with --source-format; use --source-format only", legacy),
				Kind:     kindUsage,
				ExitCode: ExitUsage,
			}
		}
		return legacy, nil
	}
	return strings.TrimSpace(sourceFormat), nil
}

func legacyImportSourceFormat() string {
	for _, format := range flags.FormatValues {
		format = strings.TrimSpace(format)
		if isImportLegacySourceFormat(format) {
			return format
		}
	}
	return strings.TrimSpace(flags.Format)
}

func validateBeadsImportFlags(cmd *cobra.Command) error {
	for _, name := range []string{"input", "target", "force", "new-instance"} {
		if cmd.Flags().Changed(name) {
			return &cliError{
				Message:  fmt.Sprintf("--%s is not supported with --source-format beads", name),
				Kind:     kindValidation,
				ExitCode: ExitValidation,
			}
		}
	}
	return nil
}

func runKataJSONLImport(cmd *cobra.Command, input, target string, force, newInstance bool) error {
	if input == "" {
		return &cliError{Message: "import requires --input", Kind: kindValidation, ExitCode: ExitValidation}
	}
	if target == "" {
		return &cliError{Message: "import requires --target", Kind: kindValidation, ExitCode: ExitValidation}
	}
	if err := refuseRunningDaemonWithMessage(cmd.Context(),
		"daemon is running for this database; stop it before importing"); err != nil {
		return err
	}
	if _, err := os.Stat(target); err == nil && !force {
		return &cliError{
			Message:  "target already exists; pass --force to replace it",
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("stat import target: %w", err)
	}
	if force {
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove import target: %w", err)
		}
	}
	in, err := os.Open(input) //nolint:gosec // import path is user-provided CLI input
	if err != nil {
		return fmt.Errorf("open import input: %w", err)
	}
	defer func() { _ = in.Close() }()
	d, err := db.Open(cmd.Context(), target)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	if err := jsonl.ImportWithOptions(cmd.Context(), in, d, jsonl.ImportOptions{
		NewInstance: newInstance,
	}); err != nil {
		return err
	}
	if flags.Quiet || flags.JSON {
		return nil
	}
	switch currentOutputMode() {
	case outputAgent:
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "OK import source_format=kata target=%s\n", agentValue(target))
	default:
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "imported %s\n", target)
	}
	return err
}
