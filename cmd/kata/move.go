package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func newMoveCmd() *cobra.Command {
	var dryRun, single bool
	cmd := &cobra.Command{
		Use:   "move <issue-ref> <project>",
		Short: "move an issue subtree to another project",
		Long: `Move an issue and all its descendants to another project.
Use --single to move only the selected issue.

The issue keeps its UID, comments, history, and all of its links
(parent, blocks/blocked-by, related) — relationships are never severed
by a move and may span projects afterward. A fresh short_id is
assigned in the target project.`,
		Example: `  kata move abc4 example-project --dry --agent`,
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMove(cmd, args[0], args[1], dryRun, single)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry", false, "validate and preview without mutating")
	cmd.Flags().BoolVar(&single, "single", false, "move only the selected issue")
	addCommentFlag(cmd)
	return cmd
}

type moveIssueWire struct {
	ID         int64  `json:"id"`
	ProjectID  int64  `json:"project_id"`
	ShortID    string `json:"short_id"`
	Revision   int64  `json:"revision"`
	UID        string `json:"uid"`
	Title      string `json:"title"`
	ProjectUID string `json:"project_uid"`
}

type moveResponseWire struct {
	Issue      moveIssueWire   `json:"issue"`
	Issues     []moveIssueWire `json:"issues"`
	EventID    int64           `json:"event_id"`
	NewShortID string          `json:"new_short_id"`
	Changed    bool            `json:"changed"`
}

func runMove(cmd *cobra.Command, rawRef, targetProject string, dryRun, single bool) error {
	comment, handle, err := prepareFollowupComment(cmd)
	if err != nil {
		return err
	}
	ctx, baseURL, pid, ref, err := resolveIssueRefForCommand(cmd, rawRef)
	if err != nil {
		return err
	}
	client, err := httpClientFor(ctx, baseURL)
	if err != nil {
		return err
	}
	if dryRun {
		if err := requireDaemonAPIVersion(ctx, client, baseURL, apiVersionMoveDryRun, "move --dry"); err != nil {
			return err
		}
	}

	// Older daemons move only the selected issue and reject the single field.
	// Never silently turn the default subtree move into a single-issue move.
	subtreeSupported := true
	if err := requireDaemonAPIVersion(ctx, client, baseURL, apiVersionMoveSubtree, "subtree move"); err != nil {
		compatibility, oldDaemon := errors.AsType[*cliError](err)
		if !single || !oldDaemon || compatibility.Code != "daemon_api_too_old" {
			return err
		}
		subtreeSupported = false
	}

	target, err := resolveProjectSelector(
		daemonAPI{ctx: ctx, client: client, baseURL: baseURL}, targetProject)
	if err != nil {
		return err
	}
	if target.UID == "" {
		return &cliError{
			Message:  fmt.Sprintf("project %q has no UID", target.Name),
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	// Showing an issue may refresh federation claims. Previews obtain the
	// source issue from the validation response instead, without that GET.
	sourceProject := ref.ProjectName
	var sourceIssue moveIssueWire
	if !dryRun {
		sourceIssue, err = fetchMoveIssue(ctx, client, baseURL, pid, ref.RefForAPI)
		if err != nil {
			return err
		}
	}
	if !dryRun {
		sourceProject, err = moveSourceProjectName(ctx, client, baseURL, pid, sourceIssue.ProjectID, sourceProject)
		if err != nil {
			return err
		}
	}
	actor, _ := resolveActor(ctx, flags.As, nil)
	apiClient, err := kataclient.NewWithHTTPClient(baseURL, client)
	if err != nil {
		return err
	}
	request := &generated.MoveIssueRequestOptions{
		PathParams: &generated.MoveIssuePath{ProjectID: pid, Ref: ref.RefForAPI},
		Body:       &generated.MoveIssueBody{Actor: &actor, ToProjectUID: target.UID},
	}
	if dryRun {
		request.Body.DryRun = &dryRun
	} else {
		etag := fmt.Sprintf(`"rev-%d"`, sourceIssue.Revision)
		request.Header = &generated.MoveIssueHeaders{IfMatch: &etag}
	}
	if single && subtreeSupported {
		request.Body.Single = &single
	}
	response, callErr := apiClient.MoveIssueWithResponse(ctx, request)
	if response == nil {
		return externalCLITransportError(response, callErr)
	}
	if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
		return err
	}
	bs := response.Body
	var moved moveResponseWire
	if err := json.Unmarshal(bs, &moved); err != nil {
		return err
	}
	if dryRun {
		sourceProject, err = moveSourceProjectName(ctx, client, baseURL, pid, moved.Issue.ProjectID, sourceProject)
		if err != nil {
			return err
		}
		names := map[int64]string{}
		if ref.ProjectName != "" {
			names[pid] = ref.ProjectName
		}
		refs, err := moveIssueRefs(ctx, client, baseURL, names, moved.Issues)
		if err != nil {
			return err
		}
		return printMovePreview(cmd, sourceProject, moved.Issue.ShortID, target.Name, moved.Issues, refs)
	}
	if err := postFollowupComment(ctx, client, baseURL, moved.Issue.ProjectID, moved.Issue.ShortID, actor, comment, handle); err != nil {
		return err
	}
	return printMove(cmd, bs, sourceProject, sourceIssue.ShortID, target.Name)
}

func moveSourceProjectName(ctx context.Context, client *http.Client, baseURL string, requestedProjectID, currentProjectID int64, requestedName string) (string, error) {
	if currentProjectID == 0 || currentProjectID == requestedProjectID {
		return requestedName, nil
	}
	project, err := resolveProjectSelector(daemonAPI{ctx: ctx, client: client, baseURL: baseURL}, strconv.FormatInt(currentProjectID, 10))
	if err != nil {
		return "", err
	}
	return project.Name, nil
}

// moveIssueRefs qualifies each issue by its current project. Subtrees can
// include children from other projects whose short IDs are ambiguous alone.
func moveIssueRefs(ctx context.Context, client *http.Client, baseURL string, names map[int64]string, issues []moveIssueWire) ([]string, error) {
	refs := make([]string, 0, len(issues))
	for _, issue := range issues {
		name, known := names[issue.ProjectID]
		if !known {
			project, err := resolveProjectSelector(daemonAPI{ctx: ctx, client: client, baseURL: baseURL}, strconv.FormatInt(issue.ProjectID, 10))
			if err != nil {
				return nil, err
			}
			name = project.Name
			names[issue.ProjectID] = name
		}
		refs = append(refs, moveQualifiedID(name, issue.ShortID))
	}
	return refs, nil
}

func fetchMoveIssue(ctx context.Context, client *http.Client, baseURL string, projectID int64, ref string) (moveIssueWire, error) {
	_, bs, err := fetchMetaIssue(ctx, client, baseURL, projectID, ref)
	if err != nil {
		return moveIssueWire{}, err
	}
	var out struct {
		Issue moveIssueWire `json:"issue"`
	}
	if err := json.Unmarshal(bs, &out); err != nil {
		return moveIssueWire{}, err
	}
	return out.Issue, nil
}

func printMove(cmd *cobra.Command, bs []byte, sourceProject, oldShortID, targetProject string) error {
	mode := currentOutputMode()
	if mode == outputJSON {
		var buf bytes.Buffer
		if err := emitJSON(&buf, jsontext.Value(bs)); err != nil {
			return err
		}
		_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
		return err
	}
	var b moveResponseWire
	if err := json.Unmarshal(bs, &b); err != nil {
		return err
	}
	oldRef := moveQualifiedID(sourceProject, oldShortID)
	newRef := moveQualifiedID(targetProject, b.NewShortID)
	descendants := make([]moveIssueWire, 0, len(b.Issues))
	for _, issue := range b.Issues {
		if issue.UID != b.Issue.UID {
			descendants = append(descendants, issue)
		}
	}
	if mode == outputAgent {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "OK move %s changed=%t dry_run=%t\n",
			agentValue(oldRef), b.Changed, false); err != nil {
			return err
		}
		if err := writeAgentKVRow(cmd.OutOrStdout(),
			agentRowField("from", oldRef),
			agentRowField("to", newRef),
			agentRowField("event_id", strconv.FormatInt(b.EventID, 10)),
		); err != nil {
			return err
		}
		for _, issue := range descendants {
			if err := writeAgentKVRow(cmd.OutOrStdout(), agentRowField("descendant_uid", issue.UID),
				agentRowField("to", moveQualifiedID(targetProject, issue.ShortID)), agentRowField("title", issue.Title)); err != nil {
				return err
			}
		}
		return nil
	}
	if flags.Quiet {
		return nil
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s %s to %s\n",
		"moved", textsafe.Line(oldRef), textsafe.Line(newRef)); err != nil {
		return err
	}
	for _, issue := range descendants {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "  %s %s %s\n", textsafe.Line(moveQualifiedID(targetProject, issue.ShortID)),
			textsafe.Line(issue.UID), textsafe.Line(issue.Title)); err != nil {
			return err
		}
	}
	return nil
}

func printMovePreview(cmd *cobra.Command, sourceProject, oldShortID, targetProject string, issues []moveIssueWire, refs []string) error {
	if flags.Quiet {
		return nil
	}
	oldRef := moveQualifiedID(sourceProject, oldShortID)
	mode := currentOutputMode()
	if mode == outputJSON {
		var buf bytes.Buffer
		payload := map[string]any{
			"dry_run":         true,
			"changed":         false,
			"from":            oldRef,
			"to_project":      targetProject,
			"target_ref_note": "target short_id is assigned by the daemon during move",
			"issues":          issues,
		}
		if err := emitJSON(&buf, payload); err != nil {
			return err
		}
		_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
		return err
	}
	if mode == outputAgent {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "OK move %s changed=false dry_run=true\n", agentValue(oldRef)); err != nil {
			return err
		}
		for index, issue := range issues {
			if err := writeAgentKVRow(cmd.OutOrStdout(), agentRowField("uid", issue.UID), agentRowField("ref", refs[index]), agentRowField("short_id", issue.ShortID), agentRowField("project_uid", issue.ProjectUID), agentRowField("title", issue.Title)); err != nil {
				return err
			}
		}
		return writeAgentKVRow(cmd.OutOrStdout(), agentRowField("from", oldRef), agentRowField("to_project", targetProject))
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(),
		"dry-run: would move %s to project %s; target short_id will be assigned by the daemon\n",
		textsafe.Line(oldRef), textsafe.Line(targetProject))
	if err != nil {
		return err
	}
	for index, issue := range issues {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "  %s %s\n", textsafe.Line(refs[index]), textsafe.Line(issue.Title)); err != nil {
			return err
		}
	}
	return nil
}

func moveQualifiedID(projectName, shortID string) string {
	return projectName + "#" + shortID
}
