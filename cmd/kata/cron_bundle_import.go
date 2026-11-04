package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/cron"
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

type cronImportItem struct {
	Kind      string `json:"kind"`
	SourceUID string `json:"source_uid"`
	UID       string `json:"uid"`
	Name      string `json:"name"`
	Outcome   string `json:"outcome"`
}
type cronImportReport struct {
	Target struct {
		UID  string `json:"project_uid"`
		Name string `json:"project_name"`
	} `json:"target"`
	Items  []cronImportItem    `json:"items"`
	Review []cron.BundleReview `json:"review"`
	DryRun bool                `json:"dry_run"`
	Note   string              `json:"note,omitempty"`
	Error  string              `json:"error,omitempty"`
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
	report := cronImportReport{Items: []cronImportItem{}, Review: plan.Review, DryRun: o.dryRun}
	report.Target.UID = project.UID
	report.Target.Name = project.Name
	if o.dryRun {
		report.Note = "Predicted from current reads; spoke projections may be stale. Authorization, live dependencies, lifecycle and CAS are checked on writes."
	}
	for _, group := range []struct {
		kind    string
		entries []cron.PlannedDefinition
	}{{"workflow", plan.Workflows}, {"job", plan.Jobs}} {
		for _, entry := range group.entries {
			outcome, writeErr := o.importItem(ctx, a, project, group.kind, entry, actor)
			report.Items = append(report.Items, cronImportItem{group.kind, entry.SourceUID, entry.UID, entry.Name, outcome})
			if writeErr != nil {
				report.Error = writeErr.Error()
				if emitErr := emitJSON(cmd.OutOrStdout(), report); emitErr != nil {
					return emitErr
				}
				return writeErr
			}
		}
	}
	return emitJSON(cmd.OutOrStdout(), report)
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

func cronBundleDefinitionEqual(a, b jsontext.Value) (bool, error) {
	left, err := json.Marshal(a, jsontext.ReorderRawObjects(true))
	if err != nil {
		return false, err
	}
	right, err := json.Marshal(b, jsontext.ReorderRawObjects(true))
	if err != nil {
		return false, err
	}
	return bytes.Equal(left, right), nil
}

func (o cronImportOptions) importItem(ctx context.Context, a daemonAPI, project projectRef, kind string, entry cron.PlannedDefinition, actor string) (string, error) {
	path := fmt.Sprintf("/api/v1/projects/%d/cron/%ss/%s", project.ID, kind, url.PathEscape(entry.UID))
	var current cronBundleRecord
	if o.dryRun {
		status, raw, err := cronBundleHTTP(ctx, a, http.MethodGet, path, nil)
		if err != nil {
			return "failed", err
		}
		if status == 404 {
			return "created", nil
		}
		if err := externalCLIResponseError(status, raw, nil); err != nil {
			return "failed", err
		}
		var envelope map[string]jsontext.Value
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return "failed", err
		}
		if err := json.Unmarshal(envelope[kind], &current); err != nil {
			return "failed", err
		}
	} else {
		status, raw, err := cronImportPut(ctx, a, path, entry, actor, "")
		if err != nil {
			return "uncertain", err
		}
		if status >= 200 && status < 300 {
			if err := cronConfirmBundleWrite(status, raw, project, kind, entry); err != nil {
				return "uncertain", err
			}
			return "created", nil
		}
		if status != 409 {
			return cronImportErrorOutcome(status), externalCLIResponseError(status, raw, nil)
		}
		var envelope struct {
			Error struct {
				Code string `json:"code"`
				Data struct {
					Current jsontext.Value `json:"current"`
				} `json:"data"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return "failed", err
		}
		if envelope.Error.Code != "cron_conflict" {
			return "failed", externalCLIResponseError(status, raw, nil)
		}
		if err := json.Unmarshal(envelope.Error.Data.Current, &current); err != nil {
			return "conflict", cronCLIValidation("conflict response has no authoritative current definition")
		}
	}
	if err := cronValidateBundleCurrent(current, project, kind, entry.UID); err != nil {
		return "conflict", err
	}
	if current.DeletedAt != nil {
		return "tombstoned", cronCLIValidation("target " + kind + " is tombstoned; restore it explicitly")
	}

	equal, err := cronBundleDefinitionEqual(current.Definition, entry.Definition)
	if err != nil {
		return "conflict", err
	}
	if strings.TrimSpace(current.Name) == entry.Name && equal {
		return "unchanged", nil
	}
	if !o.replace {
		return "conflict", cronCLIValidation("target " + kind + " differs; use --replace to update it")
	}
	if o.dryRun {
		return "replaced", nil
	}
	status, raw, err := cronImportPut(ctx, a, path, entry, actor, current.DefinitionEventUID)
	if err != nil {
		return "uncertain", err
	}
	if err := externalCLIResponseError(status, raw, nil); err != nil {
		return cronImportErrorOutcome(status), err
	}
	if err := cronConfirmBundleWrite(status, raw, project, kind, entry); err != nil {
		return "uncertain", err
	}
	return "replaced", nil
}
func cronImportErrorOutcome(status int) string {
	if status >= 500 {
		return "uncertain"
	}
	if status == 409 {
		return "conflict"
	}
	return "failed"
}
func cronImportPut(ctx context.Context, a daemonAPI, path string, entry cron.PlannedDefinition, actor, expected string) (int, []byte, error) {
	body := struct {
		Actor      string         `json:"actor"`
		Name       string         `json:"name"`
		Definition jsontext.Value `json:"definition"`
		Expected   string         `json:"expected_event_uid,omitempty"`
	}{actor, entry.Name, entry.Definition, expected}
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	return cronBundleHTTP(ctx, a, http.MethodPut, path, raw)
}

// A historical current document need not pass today's admission rules, but it
// must contain the complete API shape for its resource before it can authorize CAS.
func cronValidateBundleCurrent(current cronBundleRecord, project projectRef, kind, expectedUID string) error {
	if current.UID != expectedUID || current.ProjectID != project.ID || !uid.Valid(current.DefinitionEventUID) || current.DefinitionEventUID != strings.ToUpper(current.DefinitionEventUID) {
		return fmt.Errorf("current %s identity does not match destination", kind)
	}
	// Tombstones never authorize replacement, even when their historical document
	// cannot be decoded by this client.
	if current.DeletedAt != nil {
		return nil
	}
	if name := strings.TrimSpace(current.Name); name == "" || len(name) > 256 {
		return fmt.Errorf("current %s name is missing or invalid", kind)
	}
	if current.Definition.Kind() != '{' {
		return fmt.Errorf("current %s definition must be an object", kind)
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(current.Definition, &fields); err != nil {
		return err
	}
	required := map[string]jsontext.Kind{"version": '0', "steps": '['}
	if kind == "job" {
		required = map[string]jsontext.Kind{"version": '0', "kind": '"', "trigger": '{', "action": '{', "overlap": '"', "catchup": '"'}
	}
	for field, want := range required {
		if fields[field].Kind() != want {
			return fmt.Errorf("current %s definition is missing valid %s", kind, field)
		}
	}
	if kind == "job" {
		enabled := fields["enabled"].Kind()
		if enabled != 't' && enabled != 'f' {
			return fmt.Errorf("current job definition is missing enabled")
		}
		for _, field := range []string{"trigger", "action", "issue"} {
			raw, present := fields[field]
			if !present && field == "issue" {
				continue
			}
			var object map[string]jsontext.Value
			if raw.Kind() != '{' || json.Unmarshal(raw, &object) != nil || object["kind"].Kind() != '"' {
				return fmt.Errorf("current job definition has incomplete %s", field)
			}
		}
		job, err := cron.DecodeJob(current.Definition)
		if err != nil {
			return err
		}
		if job.Kind != "job" {
			return fmt.Errorf("current definition has wrong job kind")
		}
	} else {
		_, err := cron.DecodeWorkflow(current.Definition)
		if err != nil {
			return err
		}
		var steps []map[string]jsontext.Value
		if err := json.Unmarshal(fields["steps"], &steps); err != nil {
			return err
		}
		for i, step := range steps {
			if step["key"].Kind() != '"' || step["kind"].Kind() != '"' {
				return fmt.Errorf("current workflow definition has incomplete step %d", i)
			}
		}
	}
	return nil
}

func cronConfirmBundleWrite(status int, raw []byte, project projectRef, kind string, entry cron.PlannedDefinition) error {
	if status != http.StatusOK && status != http.StatusCreated {
		return fmt.Errorf("unexpected cron write status %d; outcome is uncertain", status)
	}
	var envelope map[string]jsontext.Value
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("invalid cron write response; outcome is uncertain: %w", err)
	}
	var current cronBundleRecord
	if err := json.Unmarshal(envelope[kind], &current); err != nil {
		return fmt.Errorf("incomplete cron write response; outcome is uncertain: %w", err)
	}
	if err := cronValidateBundleCurrent(current, project, kind, entry.UID); err != nil {
		return fmt.Errorf("incomplete cron write response; outcome is uncertain: %w", err)
	}
	if current.DeletedAt != nil {
		return fmt.Errorf("cron write returned a tombstone; outcome is uncertain")
	}
	equal, err := cronBundleDefinitionEqual(current.Definition, entry.Definition)
	if err != nil {
		return err
	}
	if strings.TrimSpace(current.Name) != entry.Name || !equal {
		return fmt.Errorf("cron write returned different content; outcome is uncertain")
	}
	return nil
}
