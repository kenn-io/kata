package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strconv"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/cronbundle"
	"go.kenn.io/kit/atomicfile"
)

type cronBundleSelection struct {
	jobs, workflows []string
	all             bool
}

func (s *cronBundleSelection) flags(cmd *cobra.Command, allowAll bool) {
	cmd.Flags().StringArrayVar(&s.jobs, "job", nil, "job UID to include (repeatable; includes its workflow)")
	cmd.Flags().StringArrayVar(&s.workflows, "workflow", nil, "workflow UID to include (repeatable)")
	if allowAll {
		cmd.Flags().BoolVar(&s.all, "all", false, "include all live jobs and workflows")
	}
}
func (s *cronBundleSelection) validate() error {
	selection := cronbundle.Selection{Jobs: s.jobs, Workflows: s.workflows, All: s.all}
	return cronBundleCLIError(selection.Normalize())
}

func newCronExportCmd() *cobra.Command {
	var selection cronBundleSelection
	var output string
	cmd := &cobra.Command{Use: "export", Short: "export portable job and workflow definitions", Args: cobra.NoArgs,
		Long: `Export live definitions as a bounded kata.cron.bundle v1 JSON archive.
--all cannot be combined with selectors.
Runs, authors, revisions and deleted definitions are excluded. Stored historical
definitions remain exportable.
--output FILE replaces a file atomically.`,
		Example: "  kata cron export --job <job-uid>\n  kata cron export --all --output cron-bundle.json"}
	selection.flags(cmd, true)
	cmd.Flags().StringVar(&output, "output", "-", "bundle file, or - for stdout")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if err := selection.validate(); err != nil {
			return err
		}
		a, err := dialDaemon(cmd.Context())
		if err != nil {
			return err
		}
		project, err := cronBundleProject(a.ctx, a)
		if err != nil {
			return err
		}
		bundle, err := cronExportBundle(a.ctx, a, project, selection)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(bundle, json.Deterministic(true), jsontext.ReorderRawObjects(true))
		if err != nil {
			return err
		}
		raw = append(raw, '\n')
		if len(raw) > cron.BundleLimit {
			return cronCLIValidation("bundle exceeds size limit")
		}
		if output != "-" {
			return atomicfile.WriteFile(output, raw, atomicfile.WithPrivate())
		}
		_, err = cmd.OutOrStdout().Write(raw)
		return err
	}
	return cmd
}

// Workspace resolution normally needs only the numeric ID. Bundles also carry
// the immutable project UID, loaded through the ordinary project catalog.
func cronBundleProject(ctx context.Context, a daemonAPI) (projectRef, error) {
	a.ctx = ctx
	project, err := resolveProjectArgFlagOrWorkspace(a, nil)
	if err != nil {
		return project, err
	}
	if project.UID == "" {
		return resolveProjectSelector(a, strconv.FormatInt(project.ID, 10))
	}
	return project, nil
}

func cronExportBundle(ctx context.Context, a daemonAPI, project projectRef, selection cronBundleSelection) (cron.Bundle, error) {
	bundle, err := cronbundle.Export(ctx, cronBundleDaemon{a}, cronbundle.Project{ID: project.ID, UID: project.UID, Name: project.Name}, cronbundle.Selection{Jobs: selection.jobs, Workflows: selection.workflows, All: selection.all})
	return bundle, cronBundleCLIError(err)
}
