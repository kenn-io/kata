package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/db/storeopen"
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
		"keep the target database's new identity instead of reusing the source identity; useful when restoring into a separate copy")
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
	targetExists, err := sqliteFileSetExists(target)
	if err != nil {
		return fmt.Errorf("stat import target: %w", err)
	}
	if targetExists && !force {
		return &cliError{
			Message:  "target already exists; pass --force to replace it",
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	in, err := os.Open(input) //nolint:gosec // import path is user-provided CLI input
	if err != nil {
		return fmt.Errorf("open import input: %w", err)
	}
	defer func() { _ = in.Close() }()
	tmpTarget, cleanupTmp, err := prepareImportTempTarget(target)
	if err != nil {
		return err
	}
	installed := false
	defer func() {
		if !installed {
			cleanupTmp()
		}
	}()
	d, err := storeopen.Open(cmd.Context(), tmpTarget)
	if err != nil {
		return err
	}
	if err := jsonl.ImportWithOptions(cmd.Context(), in, d, jsonl.ImportOptions{
		NewInstance: newInstance,
	}); err != nil {
		_ = d.Close()
		return err
	}
	if err := d.Close(); err != nil {
		return fmt.Errorf("close import target: %w", err)
	}
	if err := installImportedTarget(tmpTarget, target, force); err != nil {
		return err
	}
	installed = true
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

func prepareImportTempTarget(target string) (string, func(), error) {
	dir := filepath.Dir(target)
	base := filepath.Base(target)
	f, err := os.CreateTemp(dir, "."+base+".import-*")
	if err != nil {
		return "", nil, fmt.Errorf("create import target: %w", err)
	}
	tmpTarget := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpTarget) //nolint:gosec // tmpTarget comes from os.CreateTemp above.
		return "", nil, fmt.Errorf("close import target placeholder: %w", err)
	}
	_ = removeSQLiteFileSetMain(tmpTarget)
	return tmpTarget, func() { _ = removeSQLiteFileSetMain(tmpTarget) }, nil
}

func installImportedTarget(tmpTarget, target string, force bool) error {
	if !force {
		targetExists, err := sqliteFileSetExists(target)
		if err != nil {
			return fmt.Errorf("stat import target before install: %w", err)
		}
		if targetExists {
			return fmt.Errorf("target already exists; pass --force to replace it")
		}
		if _, err := moveSQLiteFileSet(tmpTarget, target); err != nil {
			return fmt.Errorf("install import target: %w", err)
		}
		return nil
	}

	backupTarget, err := prepareImportBackupTarget(target)
	if err != nil {
		return err
	}
	backupMade, err := moveSQLiteFileSet(target, backupTarget)
	if err != nil {
		return errors.Join(
			fmt.Errorf("backup import target: %w", err),
			restoreImportedTargetBackup(backupTarget, target, backupMade),
		)
	}
	if _, err := moveSQLiteFileSet(tmpTarget, target); err != nil {
		return errors.Join(
			fmt.Errorf("install import target: %w", err),
			restoreImportedTargetBackup(backupTarget, target, backupMade),
		)
	}
	if err := removeSQLiteFileSetMain(backupTarget); err != nil {
		return fmt.Errorf("remove import target backup: %w", err)
	}
	return nil
}

func prepareImportBackupTarget(target string) (string, error) {
	dir := filepath.Dir(target)
	base := filepath.Base(target)
	f, err := os.CreateTemp(dir, "."+base+".replace-*")
	if err != nil {
		return "", fmt.Errorf("create import target backup: %w", err)
	}
	backupTarget := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(backupTarget) //nolint:gosec // backupTarget comes from os.CreateTemp above.
		return "", fmt.Errorf("close import target backup placeholder: %w", err)
	}
	if err := os.Remove(backupTarget); err != nil { //nolint:gosec // backupTarget comes from os.CreateTemp above.
		return "", fmt.Errorf("remove import target backup placeholder: %w", err)
	}
	if exists, err := sqliteFileSetExists(backupTarget); err != nil {
		return "", fmt.Errorf("stat import target backup: %w", err)
	} else if exists {
		return "", fmt.Errorf("import target backup already exists: %s", backupTarget)
	}
	return backupTarget, nil
}

func restoreImportedTargetBackup(backupTarget, target string, backupMade bool) error {
	if !backupMade {
		return nil
	}
	if _, err := moveSQLiteFileSet(backupTarget, target); err != nil {
		return fmt.Errorf("restore import target backup: %w", err)
	}
	return nil
}

func moveSQLiteFileSet(from, to string) (bool, error) {
	moved := make([]string, 0, 3)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		src := from + suffix
		dst := to + suffix
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return len(moved) > 0, fmt.Errorf("stat %s: %w", src, err)
		}
		if err := os.Rename(src, dst); err != nil { //nolint:gosec // src/dst are SQLite files beside an explicit import target or temp DB.
			var rollbackErr error
			for i := len(moved) - 1; i >= 0; i-- {
				oldSrc := to + moved[i]
				oldDst := from + moved[i]
				if err := os.Rename(oldSrc, oldDst); err != nil { //nolint:gosec // rollback of the SQLite files just moved by this helper.
					rollbackErr = errors.Join(rollbackErr, fmt.Errorf("rollback %s: %w", moved[i], err))
				}
			}
			return len(moved) > 0, errors.Join(fmt.Errorf("rename %s: %w", suffix, err), rollbackErr)
		}
		moved = append(moved, suffix)
	}
	return len(moved) > 0, nil
}

func sqliteFileSetExists(path string) (bool, error) {
	for _, name := range sqliteFileSetPaths(path) {
		if _, err := os.Stat(name); err == nil { //nolint:gosec // path is an explicit import target or temp/backup path, plus SQLite sidecars.
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

func removeSQLiteFileSetMain(path string) error {
	var out error
	for _, name := range sqliteFileSetPaths(path) {
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) { //nolint:gosec // path is os.CreateTemp output or a suffix of explicit --target for import replacement.
			out = errors.Join(out, err)
		}
	}
	return out
}

func sqliteFileSetPaths(path string) []string {
	return []string{path, path + "-wal", path + "-shm"}
}
