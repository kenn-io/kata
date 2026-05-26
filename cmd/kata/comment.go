package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

func newCommentCmd() *cobra.Command {
	var src BodySources
	cmd := &cobra.Command{
		Use:   "comment <issue-ref>",
		Short: "append a comment to an issue",
		Args:  cobra.ExactArgs(1),
	}
	cmd.Flags().StringVar(&src.Body, "body", "", "comment body")
	cmd.Flags().StringVar(&src.File, "body-file", "", "read body from file")
	cmd.Flags().BoolVar(&src.Stdin, "body-stdin", false, "read body from stdin")

	// RunE is set after flag registration so we can reference cmd.Flags().Changed.
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		src.BodySet = cmd.Flags().Changed("body")
		src.FileSet = cmd.Flags().Changed("body-file")

		body, err := resolveBody(src, cmd.InOrStdin())
		if err != nil {
			code := ExitValidation
			if strings.HasPrefix(err.Error(), "must pass exactly one of") {
				code = ExitUsage
			}
			return &cliError{Message: err.Error(), Kind: kindForExit(code), ExitCode: code}
		}
		if strings.TrimSpace(body) == "" {
			return &cliError{Message: "comment body is required (--body, --body-file, --body-stdin)", Kind: kindValidation, ExitCode: ExitValidation}
		}
		ctx, baseURL, pid, issue, err := resolveIssueRefForCommand(cmd, args[0])
		if err != nil {
			return err
		}
		actor, _ := resolveActor(flags.As, nil)
		client, err := httpClientFor(ctx, baseURL)
		if err != nil {
			return err
		}
		status, bs, err := httpDoJSON(ctx, client, http.MethodPost,
			fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/comments", baseURL, pid, url.PathEscape(issue.RefForAPI)),
			map[string]any{"actor": actor, "body": body})
		if err != nil {
			return err
		}
		if status >= 400 {
			return apiErrFromBody(status, bs)
		}
		switch currentOutputMode() {
		case outputJSON:
			var buf bytes.Buffer
			if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
				return err
			}
			_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
			return err
		case outputAgent:
			return printAgentMutation(cmd, "comment", bs, func(w io.Writer, _ agentIssueMutation) error {
				return writeAgentField(w, "Comment", "appended")
			})
		}
		if !flags.Quiet {
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "comment appended")
			return err
		}
		return nil
	}
	return cmd
}
