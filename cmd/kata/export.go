package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/jsonl"
)

func newExportCmd() *cobra.Command {
	var output string
	var projectID int64
	var includeDeleted bool
	var allowRunningDaemon bool
	cmd := &cobra.Command{
		Use:   "export",
		Short: "export the kata database as JSONL",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if !allowRunningDaemon {
				if err := refuseRunningDaemon(ctx); err != nil {
					return err
				}
			}
			dbPath, err := config.KataDB()
			if err != nil {
				return err
			}
			if output == "" {
				output = "kata-export-" + time.Now().UTC().Format("20060102T150405Z") + ".jsonl"
			}
			d, err := db.OpenReadOnly(ctx, dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = d.Close() }()
			projectID, err = resolveExportProject(ctx, d, projectID)
			if err != nil {
				return err
			}
			if err := writeExportOutput(ctx, d, output, jsonl.ExportOptions{
				ProjectID:      projectID,
				IncludeDeleted: includeDeleted,
			}); err != nil {
				return err
			}
			if flags.Quiet || flags.JSON {
				return nil
			}
			switch currentOutputMode() {
			case outputAgent:
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "OK export output=%s\n", agentValue(output))
			default:
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "exported %s\n", output)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&output, "output", "", "path to write JSONL export")
	cmd.Flags().Int64Var(&projectID, "project-id", 0, "export only one project id")
	cmd.Flags().BoolVar(&includeDeleted, "include-deleted", true, "include soft-deleted rows")
	cmd.Flags().BoolVar(&allowRunningDaemon, "allow-running-daemon", false, "export even if a daemon is running")
	return cmd
}

func writeExportOutput(ctx context.Context, d *db.DB, output string, opts jsonl.ExportOptions) error {
	dir := filepath.Dir(output)
	base := filepath.Base(output)
	f, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return fmt.Errorf("create export output: %w", err)
	}
	tmpName := f.Name()
	committed := false
	defer func() {
		if !committed {
			_ = f.Close()
			_ = os.Remove(tmpName)
		}
	}()

	bw := bufio.NewWriter(f)
	if err := jsonl.Export(ctx, d, bw, opts); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("flush export output: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync export output: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close export output: %w", err)
	}
	if err := replaceExportOutput(tmpName, output); err != nil {
		return err
	}
	committed = true
	return nil
}

// resolveExportProject reconciles the global --project NAME flag with the
// local --project-id N flag. NAME is looked up against the projects table
// using the read-only export handle. Conflicts and unknown names surface as
// validation errors so scripts get a clean failure rather than a silent
// full-DB export.
func resolveExportProject(ctx context.Context, d *db.DB, projectID int64) (int64, error) {
	name := strings.TrimSpace(flags.Project)
	if name == "" {
		return projectID, nil
	}
	p, err := d.ProjectByName(ctx, name)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return 0, &cliError{
				Message:  fmt.Sprintf("project %q not found", name),
				Kind:     kindNotFound,
				ExitCode: ExitNotFound,
			}
		}
		return 0, fmt.Errorf("resolve --project: %w", err)
	}
	if projectID != 0 && projectID != p.ID {
		return 0, &cliError{
			Message:  fmt.Sprintf("--project %q resolves to id %d, conflicts with --project-id %d", name, p.ID, projectID),
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	return p.ID, nil
}

func refuseRunningDaemon(ctx context.Context) error {
	return refuseRunningDaemonWithMessage(ctx,
		"daemon is running for this database; stop it or pass --allow-running-daemon")
}

func refuseRunningDaemonWithMessage(ctx context.Context, message string) error {
	ns, err := daemon.NewNamespace()
	if err != nil {
		return err
	}
	if _, ok := client.Discover(ctx, ns.DataDir); ok {
		return &cliError{
			Message:  message,
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	return nil
}
