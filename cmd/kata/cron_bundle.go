package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
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
	if s.all && len(s.jobs)+len(s.workflows) > 0 || !s.all && len(s.jobs)+len(s.workflows) == 0 {
		return cronCLIValidation("select --job and/or --workflow, or --all alone")
	}
	for _, list := range [][]string{s.jobs, s.workflows} {
		for i := range list {
			id, err := normalizedCronCLIUID(list[i])
			if err != nil {
				return err
			}
			list[i] = id
		}
	}
	return nil
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

type cronBundleRecord struct {
	db.CronDefinition
	Definition jsontext.Value `json:"definition"`
}

func cronBundleRead(ctx context.Context, a daemonAPI, projectID int64, kind, operation, id string) ([]cronBundleRecord, error) {
	path := fmt.Sprintf("/api/v1/projects/%d/cron/%ss", projectID, kind)
	if operation == "show" {
		path += "/" + url.PathEscape(id)
	}
	status, raw, err := cronBundleHTTP(ctx, a, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if err := externalCLIResponseError(status, raw, nil); err != nil {
		return nil, err
	}

	var response struct {
		Jobs      []cronBundleRecord `json:"jobs"`
		Workflows []cronBundleRecord `json:"workflows"`
		Job       cronBundleRecord   `json:"job"`
		Workflow  cronBundleRecord   `json:"workflow"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	if operation == "show" {
		if kind == "job" {
			return []cronBundleRecord{response.Job}, nil
		}
		return []cronBundleRecord{response.Workflow}, nil
	}
	if kind == "job" {
		return response.Jobs, nil
	}
	return response.Workflows, nil
}

func cronExportBundle(ctx context.Context, a daemonAPI, project projectRef, selection cronBundleSelection) (cron.Bundle, error) {
	bundle := cron.Bundle{Format: "kata.cron.bundle", Version: 1, Source: cron.BundleSource{ProjectUID: project.UID, ProjectName: project.Name, ExportedAt: time.Now().UTC().Format(time.RFC3339Nano)}, Workflows: []cron.BundleDefinition{}, Jobs: []cron.BundleDefinition{}}
	jobs, workflows := map[string]cron.BundleDefinition{}, map[string]cron.BundleDefinition{}
	add := func(kind string, records []cronBundleRecord) error {
		for _, record := range records {
			if record.DeletedAt != nil {
				return cronCLIValidation(fmt.Sprintf("cannot export deleted %s %s", kind, record.UID))
			}
			entry := cron.BundleDefinition{UID: record.UID, DefinitionEventUID: record.DefinitionEventUID, Name: record.Name, Definition: record.Definition}
			if kind == "job" {
				jobs[entry.UID] = entry
			} else {
				workflows[entry.UID] = entry
			}
			if len(jobs)+len(workflows) > 1000 {
				return cronCLIValidation("bundle exceeds 1000 definitions")
			}
		}
		return nil
	}
	for _, group := range []struct {
		kind string
		ids  []string
	}{{"workflow", selection.workflows}, {"job", selection.jobs}} {
		if selection.all {
			records, err := cronBundleRead(ctx, a, project.ID, group.kind, "list", "")
			if err != nil {
				return bundle, err
			}
			if err := add(group.kind, records); err != nil {
				return bundle, err
			}
		} else {
			seen := map[string]bool{}
			for _, id := range group.ids {
				if seen[id] {
					continue
				}
				seen[id] = true
				records, err := cronBundleRead(ctx, a, project.ID, group.kind, "show", id)
				if err != nil {
					return bundle, err
				}
				if err := add(group.kind, records); err != nil {
					return bundle, err
				}
			}
		}
	}
	// Sort before dependency reads as well, so the first reported failure is stable.
	ids := make([]string, 0, len(jobs))
	for id := range jobs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		job, err := cron.DecodeJob(jobs[id].Definition)
		if err != nil {
			return bundle, err
		}
		ref := job.Action.WorkflowUID
		if ref == "" {
			continue
		}
		if _, ok := workflows[ref]; ok {
			continue
		}
		records, err := cronBundleRead(ctx, a, project.ID, "workflow", "show", ref)
		if err != nil {
			return bundle, err
		}
		if err := add("workflow", records); err != nil {
			return bundle, err
		}
	}
	for _, entry := range workflows {
		bundle.Workflows = append(bundle.Workflows, entry)
	}
	for _, entry := range jobs {
		bundle.Jobs = append(bundle.Jobs, entry)
	}
	for _, entries := range [][]cron.BundleDefinition{bundle.Workflows, bundle.Jobs} {
		slices.SortFunc(entries, func(a, b cron.BundleDefinition) int { return strings.Compare(a.UID, b.UID) })
	}
	return bundle, bundle.Validate()
}
