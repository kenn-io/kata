package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strconv"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func newMoveCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "move <issue-ref> <project>",
		Short: "move an issue to another project",
		Long: `Move an issue to another project.

The issue keeps its UID, comments, history, and all of its links
(parent, blocks/blocked-by, related) — relationships are never severed
by a move and may span projects afterward. A fresh short_id is
assigned in the target project.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMove(cmd, args[0], args[1], dryRun)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "validate and preview without mutating")
	addCommentFlag(cmd)
	return cmd
}

type moveIssueWire struct {
	ID        int64  `json:"id"`
	ProjectID int64  `json:"project_id"`
	ShortID   string `json:"short_id"`
	Revision  int64  `json:"revision"`
}

type moveResponseWire struct {
	Issue      moveIssueWire `json:"issue"`
	EventID    int64         `json:"event_id"`
	NewShortID string        `json:"new_short_id"`
	Changed    bool          `json:"changed"`
}

func runMove(cmd *cobra.Command, rawRef, targetProject string, dryRun bool) error {
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
	sourceIssue, err := fetchMoveIssue(ctx, client, baseURL, pid, ref.RefForAPI)
	if err != nil {
		return err
	}
	if dryRun {
		return printMovePreview(cmd, ref.ProjectName, sourceIssue.ShortID, target.Name)
	}
	actor, _ := resolveActor(ctx, flags.As, nil)
	apiClient, err := kataclient.NewWithHTTPClient(baseURL, client)
	if err != nil {
		return err
	}
	etag := fmt.Sprintf(`"rev-%d"`, sourceIssue.Revision)
	response, callErr := apiClient.MoveIssueWithResponse(ctx, &generated.MoveIssueRequestOptions{
		PathParams: &generated.MoveIssuePath{ProjectID: pid, Ref: ref.RefForAPI},
		Body:       &generated.MoveIssueBody{Actor: &actor, ToProjectUID: target.UID},
		Header:     &generated.MoveIssueHeaders{IfMatch: &etag},
	})
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
	if err := postFollowupComment(ctx, client, baseURL, moved.Issue.ProjectID, moved.Issue.ShortID, actor, comment, handle); err != nil {
		return err
	}
	return printMove(cmd, bs, ref.ProjectName, sourceIssue.ShortID, target.Name)
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
	if mode == outputAgent {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "OK move %s changed=%t dry_run=%t\n",
			agentValue(oldRef), b.Changed, false); err != nil {
			return err
		}
		return writeAgentKVRow(cmd.OutOrStdout(),
			agentRowField("from", oldRef),
			agentRowField("to", newRef),
			agentRowField("event_id", strconv.FormatInt(b.EventID, 10)),
		)
	}
	if flags.Quiet {
		return nil
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s %s to %s\n",
		"moved", textsafe.Line(oldRef), textsafe.Line(newRef))
	return err
}

func printMovePreview(cmd *cobra.Command, sourceProject, oldShortID, targetProject string) error {
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
		return writeAgentKVRow(cmd.OutOrStdout(),
			agentRowField("from", oldRef),
			agentRowField("to_project", targetProject),
		)
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(),
		"dry-run: would move %s to project %s; target short_id will be assigned by the daemon\n",
		textsafe.Line(oldRef), textsafe.Line(targetProject))
	return err
}

func moveQualifiedID(projectName, shortID string) string {
	return projectName + "#" + shortID
}
