package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/textsafe"
)

type federationRebindCLIResult struct {
	Project   string `json:"project"`
	ProjectID int64  `json:"project_id"`
	OldOrigin string `json:"old_origin,omitempty"`
	NewOrigin string `json:"new_origin,omitempty"`
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
}

type federationRebindCLIOutput struct {
	Results []federationRebindCLIResult `json:"results"`
}

func federationRebindCmd() *cobra.Command {
	var hubCatalog string
	var all bool
	cmd := &cobra.Command{
		Use:   "rebind [project]",
		Short: "move spoke bindings to a configured HTTPS hub endpoint",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && strings.TrimSpace(flags.Project) != "" {
				return federationRebindSelectorError("positional project and --project are mutually exclusive")
			}
			if all && (len(args) > 0 || strings.TrimSpace(flags.Project) != "") {
				return federationRebindSelectorError("--all and a single project selector are mutually exclusive")
			}
			ctx := cmd.Context()
			baseURL, err := ensureDaemon(ctx)
			if err != nil {
				return err
			}
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			targets, err := federationRebindTargets(ctx, client, baseURL, args, all)
			if err != nil {
				return err
			}
			results := make([]federationRebindCLIResult, 0, len(targets))
			failed := 0
			for _, target := range targets {
				result, rebindErr := executeFederationRebind(ctx, client, baseURL, target, hubCatalog)
				if rebindErr != nil {
					if !all {
						return rebindErr
					}
					failed++
					result = federationRebindCLIResult{
						Project: target.ProjectName, ProjectID: target.ProjectID,
						State: "failed", Error: textsafe.Line(rebindErr.Error()),
					}
				}
				results = append(results, result)
			}
			if err := printFederationRebind(cmd, results, failed); err != nil {
				return err
			}
			if failed > 0 {
				return &cliError{
					Message: fmt.Sprintf("%d of %d federation rebind operations failed", failed, len(results)),
					Code:    "federation_rebind_partial_failure", Kind: kindConflict, ExitCode: ExitConflict,
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&hubCatalog, "hub", "", "named daemon catalog entry containing the replacement HTTPS hub URL")
	cmd.Flags().BoolVar(&all, "all", false, "rebind every local spoke independently")
	if err := cmd.MarkFlagRequired("hub"); err != nil {
		panic(err)
	}
	return cmd
}

func federationRebindSelectorError(message string) error {
	return &cliError{Message: message, Kind: kindUsage, ExitCode: ExitUsage}
}

func federationRebindTargets(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	args []string,
	all bool,
) ([]api.FederationProjectStatus, error) {
	status, body, err := httpDoJSON(ctx, client, http.MethodGet, baseURL+"/api/v1/federation/status", nil)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, apiErrFromBody(status, body)
	}
	var federationStatus api.FederationStatusBody
	if err := json.Unmarshal(body, &federationStatus); err != nil {
		return nil, err
	}
	if all {
		targets := make([]api.FederationProjectStatus, 0, len(federationStatus.Statuses))
		for _, candidate := range federationStatus.Statuses {
			if candidate.Role == string(db.FederationRoleSpoke) {
				targets = append(targets, candidate)
			}
		}
		sort.Slice(targets, func(i, j int) bool { return targets[i].ProjectID < targets[j].ProjectID })
		return targets, nil
	}

	name := strings.TrimSpace(flags.Project)
	if len(args) > 0 {
		name = strings.TrimSpace(args[0])
	}
	if name != "" {
		for _, candidate := range federationStatus.Statuses {
			if candidate.ProjectName != name {
				continue
			}
			if candidate.Role != string(db.FederationRoleSpoke) {
				return nil, federationRebindNotSpoke(name)
			}
			return []api.FederationProjectStatus{candidate}, nil
		}
		return nil, federationRebindNotSpoke(name)
	}

	project, err := resolveFederationProject(ctx, client, baseURL, nil, false)
	if err != nil {
		return nil, err
	}
	for _, candidate := range federationStatus.Statuses {
		if candidate.ProjectID != project.ID {
			continue
		}
		if candidate.Role != string(db.FederationRoleSpoke) {
			return nil, federationRebindNotSpoke(project.Name)
		}
		return []api.FederationProjectStatus{candidate}, nil
	}
	return nil, federationRebindNotSpoke(project.Name)
}

func federationRebindNotSpoke(project string) error {
	return &cliError{
		Message: fmt.Sprintf("project %s is not a federation spoke", textsafe.Line(project)),
		Code:    "not_a_spoke", Kind: kindConflict, ExitCode: ExitConflict,
	}
}

func executeFederationRebind(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	target api.FederationProjectStatus,
	hubCatalog string,
) (federationRebindCLIResult, error) {
	status, body, err := httpDoJSON(
		ctx,
		client,
		http.MethodPost,
		fmt.Sprintf("%s/api/v1/federation/replicas/%d/actions/rebind", baseURL, target.ProjectID),
		map[string]any{"hub_catalog": strings.TrimSpace(hubCatalog)},
	)
	if err != nil {
		return federationRebindCLIResult{}, err
	}
	if status >= 400 {
		return federationRebindCLIResult{}, apiErrFromBody(status, body)
	}
	var response api.RebindFederationReplicaResponseBody
	if err := json.Unmarshal(body, &response); err != nil {
		return federationRebindCLIResult{}, err
	}
	return federationRebindCLIResult{
		Project: response.Project.Name, ProjectID: response.Project.ID,
		OldOrigin: response.OldOrigin, NewOrigin: response.NewOrigin, State: response.State,
	}, nil
}

func printFederationRebind(cmd *cobra.Command, results []federationRebindCLIResult, failed int) error {
	switch currentOutputMode() {
	case outputJSON:
		var output bytes.Buffer
		if err := emitJSON(&output, federationRebindCLIOutput{Results: results}); err != nil {
			return err
		}
		_, err := fmt.Fprint(cmd.OutOrStdout(), output.String())
		return err
	case outputAgent:
		if failed == 0 {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "OK federation-rebind count=%d failed=0\n", len(results)); err != nil {
				return err
			}
		}
		for _, result := range results {
			fields := []agentField{
				agentRowField("project", result.Project),
				agentRowField("project_id", strconv.FormatInt(result.ProjectID, 10)),
				agentRowField("state", result.State),
			}
			if result.OldOrigin != "" {
				fields = append(fields, agentRowField("old_origin", result.OldOrigin))
			}
			if result.NewOrigin != "" {
				fields = append(fields, agentRowField("new_origin", result.NewOrigin))
			}
			if result.Error != "" {
				fields = append(fields, agentRowField("error", result.Error))
			}
			if err := writeAgentKVRow(cmd.OutOrStdout(), fields...); err != nil {
				return err
			}
		}
		return nil
	default:
		if flags.Quiet {
			return nil
		}
		for _, result := range results {
			switch result.State {
			case "rebound":
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "rebound federation endpoint for %s: %s -> %s\n",
					textsafe.Line(result.Project), textsafe.Line(result.OldOrigin), textsafe.Line(result.NewOrigin)); err != nil {
					return err
				}
			case "resumed":
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "resumed federation endpoint rebind for %s: %s -> %s\n",
					textsafe.Line(result.Project), textsafe.Line(result.OldOrigin), textsafe.Line(result.NewOrigin)); err != nil {
					return err
				}
			case "unchanged":
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "federation endpoint already uses %s for %s\n",
					textsafe.Line(result.NewOrigin), textsafe.Line(result.Project)); err != nil {
					return err
				}
			default:
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "federation endpoint rebind failed for %s: %s\n",
					textsafe.Line(result.Project), textsafe.Line(result.Error)); err != nil {
					return err
				}
			}
		}
		return nil
	}
}
