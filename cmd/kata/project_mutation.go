package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	kataclient "go.kenn.io/kata/pkg/client"
)

// projectMutation carries the locally known project selector to the daemon.
// Only operations which need canonical identity before submitting their body
// (relationship validation and path-only workspaces) resolve it separately.
type projectMutation struct {
	api      daemonAPI
	selector string
	name     string
	id       int64
	headers  map[string]string
	repair   func(string) error
}

func prepareProjectMutation(a daemonAPI, start string, resolveNow bool) (*projectMutation, error) {
	p := &projectMutation{api: a}
	if !resolveNow {
		body, repair, err := buildResolveRequest(a.ctx, start)
		if err != nil {
			return nil, err
		}
		if _, pathOnly := body["start_path"]; !pathOnly {
			p.name, _ = body["name"].(string)
			p.selector = "name:" + p.name
			p.repair = repair
			if alias, ok := body["alias"].(map[string]any); ok {
				identity, _ := alias["identity"].(string)
				kind, _ := alias["kind"].(string)
				p.headers = map[string]string{
					"X-Kata-Project-Alias":      identity,
					"X-Kata-Project-Alias-Kind": kind,
				}
			}
			return p, nil
		}
	}
	id, name, err := resolveProjectIDAndNameWithClient(a, start)
	if err != nil {
		return nil, err
	}
	p.id, p.name, p.selector = id, name, strconv.FormatInt(id, 10)
	return p, nil
}

func prepareIssueMutation(cmd *cobra.Command, ref string, resolveNow bool) (*projectMutation, resolvedIssueRef, error) {
	start, err := resolveStartPath(flags.Workspace)
	if err != nil {
		return nil, resolvedIssueRef{}, err
	}
	a, err := dialDaemon(cmd.Context())
	if err != nil {
		return nil, resolvedIssueRef{}, err
	}
	project := strings.TrimSpace(flags.Project)
	// ResolveRef fills ProjectName for both explicit and inherited names.
	// Preserve the source so workspace-bound refs retain alias resolution
	// and binding repair after a project rename or merge.
	workspaceBound := project == "" && !strings.Contains(ref, "#")
	if project == "" {
		project = workspaceProjectName(start)
	}
	parsed, err := ResolveRef(ref, project)
	if err != nil {
		return nil, resolvedIssueRef{}, &cliError{Message: err.Error(), Kind: kindValidation, ExitCode: ExitValidation}
	}
	if workspaceBound {
		p, err := prepareProjectMutation(a, start, resolveNow)
		if err != nil {
			return nil, resolvedIssueRef{}, err
		}
		return p, resolvedIssueRef{RefForAPI: parsed.RefForAPI, ProjectName: p.name}, nil
	}
	p := &projectMutation{api: a, name: parsed.ProjectName, selector: "name:" + parsed.ProjectName}
	if resolveNow {
		p.id, p.name, err = resolveProjectIDAndNameForRef(a, start, parsed.ProjectName, false)
		if err != nil {
			return nil, resolvedIssueRef{}, err
		}
		p.selector = strconv.FormatInt(p.id, 10)
	}
	return p, resolvedIssueRef{RefForAPI: parsed.RefForAPI, ProjectName: p.name}, nil
}

func (p *projectMutation) generatedClient() (*kataclient.Client, error) {
	return kataclient.NewWithHTTPClient(p.api.baseURL, p.api.client,
		kataclient.WithRequestEditor(func(_ context.Context, req *http.Request) error {
			for key, value := range p.headers {
				req.Header.Set(key, value)
			}
			return nil
		}))
}

func (p *projectMutation) finishMutation(resp *http.Response, data []byte, callErr error) ([]byte, error) {
	if err := externalCLITransportError(resp, callErr); err != nil {
		return nil, err
	}
	if err := externalCLIResponseError(resp.StatusCode, data, callErr); err != nil {
		return nil, err
	}
	if canonical := resp.Header.Get("X-Kata-Project-Name"); canonical != "" {
		p.name = canonical
	}
	if p.repair != nil {
		if err := p.repair(p.name); err != nil {
			return nil, &cliError{
				Message: fmt.Sprintf("issue mutation succeeded but workspace binding repair failed: %v; do not repeat the mutation", err),
				Kind:    kindInternal, Code: "workspace_repair_failed", ExitCode: ExitInternal,
			}
		}
	}
	return data, nil
}

// Follow-up comments use the project ID returned by the successful mutation,
// not another lookup of the name which might have changed in the meantime.
func (p *projectMutation) comment(data []byte, ref, actor, body, teammate string) error {
	if body == "" {
		return nil
	}
	var response struct {
		Issue struct {
			ProjectID int64 `json:"project_id"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return fmt.Errorf("issue mutation succeeded but reading its project for --comment failed: %w", err)
	}
	return postFollowupComment(p.api.ctx, p.api.client, p.api.baseURL, response.Issue.ProjectID, ref, actor, body, teammate)
}
