package main

import (
	"context"
	"encoding/json/v2"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/cronbundle"
	"go.kenn.io/kata/internal/uid"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

type cronImportOptions struct {
	mappings                     []string
	keepEnabled, replace, dryRun bool
}

func (o *cronImportOptions) flags(cmd *cobra.Command) {
	cmd.Flags().StringArrayVar(&o.mappings, "map-issue", nil, "source issue UID=target issue ref (repeatable; target project only)")
	cmd.Flags().BoolVar(&o.keepEnabled, "keep-enabled", false, "retain source enabled state (copies default to disabled)")
	cmd.Flags().BoolVar(&o.replace, "replace", false, "replace different live copies at their current revision")
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false, "predict outcomes from reads without writing")
}

const cronImportHelp = `Copies receive deterministic new UIDs, so repeating the same bundle and flags
resumes an import. Workflows are written before their jobs. The first unresolved
conflict, tombstone or error stops the import and retains completed results.
Cross-project issue references require --map-issue SOURCE_UID=TARGET_REF.
Explicit mappings override retained same-project references. Runtime-local
recipients, checkout keys, secret names and options need operator review.
--replace tries once, rejecting concurrent edits, deleted targets, and
recurring-job parent changes. Replacing a workflow can immediately affect
existing enabled jobs. Native copies start fresh parent progress; later enabling
catchup=all may backfill since the copy was created.
--dry-run uses the same plan but cannot guarantee permissions, live dependencies
or protection against concurrent edits; spoke projections may be stale.
Results are JSON, including partial progress and uncertain write outcomes.`

func newCronImportCmd() *cobra.Command {
	var file string
	var options cronImportOptions
	cmd := &cobra.Command{Use: "import", Short: "import independent copies from a cron bundle", Args: cobra.NoArgs, Long: "Import a kata.cron.bundle v1 file into the selected project.\n" + cronImportHelp, Example: "  kata cron import --file cron-bundle.json --project target-project\n  kata cron import --file - --project target-project --dry-run"}
	cmd.Flags().StringVar(&file, "file", "", "bundle file, or - for stdin")
	_ = cmd.MarkFlagRequired("file")
	options.flags(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		reader := cmd.InOrStdin()
		if file != "-" {
			f, err := os.Open(file) //nolint:gosec // The path is explicit user-provided CLI input.
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			reader = f
		}
		raw, err := io.ReadAll(io.LimitReader(reader, cron.BundleLimit+1))
		if err != nil {
			return err
		}
		bundle, err := cron.ParseBundle(raw)
		if err != nil {
			return cronCLIValidation(err.Error())
		}
		a, err := dialDaemon(cmd.Context())
		if err != nil {
			return err
		}
		project, err := cronBundleProject(cmd.Context(), a)
		if err != nil {
			return err
		}
		return options.importBundle(cmd.Context(), cmd, a, project, bundle)
	}
	return cmd
}

func newCronCopyCmd() *cobra.Command {
	var selection cronBundleSelection
	var target string
	var options cronImportOptions
	cmd := &cobra.Command{Use: "copy", Short: "copy jobs and workflows to another project on this daemon", Args: cobra.NoArgs, Long: "Copy selected definitions from --project/workspace to --to-project on the same daemon.\nAcross daemons, pipe cron export into cron import instead.\n" + cronImportHelp, Example: "  kata cron copy --job <job-uid> --to-project target-project"}
	selection.flags(cmd, false)
	options.flags(cmd)
	cmd.Flags().StringVar(&target, "to-project", "", "destination project on the selected daemon")
	_ = cmd.MarkFlagRequired("to-project")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if err := selection.validate(); err != nil {
			return err
		}
		a, err := dialDaemon(cmd.Context())
		if err != nil {
			return err
		}
		source, err := cronBundleProject(cmd.Context(), a)
		if err != nil {
			return err
		}
		destination, err := resolveProjectSelector(a, target)
		if err != nil {
			return err
		}
		bundle, err := cronExportBundle(cmd.Context(), a, source, selection)
		if err != nil {
			return err
		}
		return options.importBundle(cmd.Context(), cmd, a, destination, bundle)
	}
	return cmd
}

func (o cronImportOptions) importBundle(ctx context.Context, cmd *cobra.Command, a daemonAPI, project projectRef, bundle cron.Bundle) error {
	mappings, err := cronResolveMappings(ctx, a, project, o.mappings)
	if err != nil {
		return err
	}
	plan, err := cron.PlanBundle(bundle, cron.BundleTarget{ProjectUID: project.UID, IssueMappings: mappings, KeepEnabled: o.keepEnabled})
	if err != nil {
		return cronCLIValidation(err.Error())
	}
	if !o.dryRun {
		a.client, err = longRunningClientForResolved(ctx, a.resolved)
		if err != nil {
			return err
		}
	}
	actor, _ := resolveActor(ctx, flags.As, nil)
	report, importErr := cronbundle.Import(ctx, cronBundleDaemon{a}, cronbundle.Project{ID: project.ID, UID: project.UID, Name: project.Name}, plan, cronbundle.ImportOptions{Replace: o.replace, DryRun: o.dryRun, Actor: actor})
	if err := emitJSON(cmd.OutOrStdout(), report); err != nil {
		return err
	}
	return cronBundleCLIError(importErr)
}

func cronResolveMappings(ctx context.Context, a daemonAPI, project projectRef, values []string) (map[string]string, error) {
	a.ctx = ctx
	result := map[string]string{}
	refs := map[string]string{}
	for _, value := range values {
		source, ref, ok := strings.Cut(value, "=")
		if !ok || strings.TrimSpace(ref) == "" {
			return nil, cronCLIValidation("--map-issue requires SOURCE_UID=TARGET_REF")
		}
		source, err := normalizedCronCLIUID(source)
		if err != nil {
			return nil, err
		}
		ref = strings.TrimSpace(ref)
		if old, ok := refs[source]; ok && old != ref {
			return nil, cronCLIValidation("conflicting issue mappings for " + source)
		}
		refs[source] = ref
	}
	c, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
	if err != nil {
		return nil, err
	}
	for source, ref := range refs {
		if prefix, bare, qualified := strings.Cut(ref, "#"); qualified {
			selected, err := resolveProjectSelector(a, prefix)
			if err != nil {
				return nil, err
			}
			if selected.ID != project.ID {
				return nil, cronCLIValidation("issue mapping must resolve in target project " + project.Name)
			}
			ref = bare
		}
		response, callErr := c.ShowIssueWithResponse(ctx, &generated.ShowIssueRequestOptions{PathParams: &generated.ShowIssuePath{ProjectID: project.ID, Ref: ref}})
		if response == nil {
			return nil, externalCLITransportError(response, callErr)
		}
		if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
			return nil, err
		}
		var shown struct {
			Issue struct {
				UID       string `json:"uid"`
				ProjectID int64  `json:"project_id"`
			} `json:"issue"`
		}
		if err := json.Unmarshal(response.Body, &shown); err != nil {
			return nil, err
		}
		if shown.Issue.ProjectID != project.ID || !uid.Valid(shown.Issue.UID) {
			return nil, cronCLIValidation("issue mapping returned an invalid target identity")
		}
		result[source] = shown.Issue.UID
	}
	return result, nil
}
