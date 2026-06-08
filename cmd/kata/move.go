package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
)

func newMoveCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "move <issue-ref> <project>",
		Short: "move an issue to another project",
		Args:  cobra.ExactArgs(2),
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
	comment, err := commentFromFlag(cmd)
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
	target, err := resolveProjectSelector(ctx, client, baseURL, targetProject)
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
	status, bs, err := httpDoJSONHeaders(ctx, client, http.MethodPost,
		fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/move", baseURL, pid, url.PathEscape(ref.RefForAPI)),
		map[string]any{
			"actor":          actor,
			"to_project_uid": target.UID,
		},
		map[string]string{"If-Match": fmt.Sprintf(`"rev-%d"`, sourceIssue.Revision)})
	if err != nil {
		return err
	}
	if status >= 400 {
		return apiErrFromBody(status, bs)
	}
	var moved moveResponseWire
	if err := json.Unmarshal(bs, &moved); err != nil {
		return err
	}
	if err := postFollowupComment(ctx, client, baseURL, moved.Issue.ProjectID, moved.Issue.ShortID, actor, comment); err != nil {
		return err
	}
	return printMove(cmd, bs, ref.ProjectName, sourceIssue.ShortID, target.Name)
}

func fetchMoveIssue(ctx context.Context, client *http.Client, baseURL string, projectID int64, ref string) (moveIssueWire, error) {
	status, bs, err := httpDoJSON(ctx, client, http.MethodGet,
		fmt.Sprintf("%s/api/v1/projects/%d/issues/%s", baseURL, projectID, url.PathEscape(ref)), nil)
	if err != nil {
		return moveIssueWire{}, err
	}
	if status >= 400 {
		return moveIssueWire{}, apiErrFromBody(status, bs)
	}
	var out struct {
		Issue moveIssueWire `json:"issue"`
	}
	if err := json.Unmarshal(bs, &out); err != nil {
		return moveIssueWire{}, err
	}
	return out.Issue, nil
}

func httpDoJSONHeaders(ctx context.Context, client *http.Client, method, path string, body any, headers map[string]string) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		bs, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(bs)
	}
	req, err := http.NewRequestWithContext(ctx, method, path, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req) //nolint:gosec // daemon-local URL, same as httpDoJSON.
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	bs, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, bs, nil
}

func printMove(cmd *cobra.Command, bs []byte, sourceProject, oldShortID, targetProject string) error {
	mode := currentOutputMode()
	if mode == outputJSON {
		var buf bytes.Buffer
		if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
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
