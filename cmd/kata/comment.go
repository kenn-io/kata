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
	var unsupportedRelationships commentRelationshipFlags
	cmd := &cobra.Command{
		Use:   "comment <issue-ref>",
		Short: "append a comment to an issue",
		Args:  cobra.ExactArgs(1),
	}
	cmd.Flags().StringVarP(&src.Body, "body", "m", "", "comment body")
	cmd.Flags().StringVar(&src.File, "body-file", "", "read body from file")
	cmd.Flags().BoolVar(&src.Stdin, "body-stdin", false, "read body from stdin")
	cmd.Flags().StringVar(&unsupportedRelationships.Parent, "parent", "", "unsupported on comment; use edit")
	cmd.Flags().StringVar(&unsupportedRelationships.Blocks, "blocks", "", "unsupported on comment; use edit")
	cmd.Flags().StringVar(&unsupportedRelationships.BlockedBy, "blocked-by", "", "unsupported on comment; use edit")
	cmd.Flags().StringVar(&unsupportedRelationships.Related, "related", "", "unsupported on comment; use edit")
	_ = cmd.Flags().MarkHidden("parent")
	_ = cmd.Flags().MarkHidden("blocks")
	_ = cmd.Flags().MarkHidden("blocked-by")
	_ = cmd.Flags().MarkHidden("related")

	// RunE is set after flag registration so we can reference cmd.Flags().Changed.
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := unsupportedRelationships.err(cmd, args[0]); err != nil {
			return err
		}
		src.BodySet = cmd.Flags().Changed("body")
		src.FileSet = cmd.Flags().Changed("body-file")

		body, err := resolveCommentBody(cmd, src)
		if err != nil {
			return err
		}
		ctx, baseURL, pid, issue, err := resolveIssueRefForCommand(cmd, args[0])
		if err != nil {
			return err
		}
		actor, _ := resolveActor(ctx, flags.As, nil)
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
	cmd.AddCommand(newCommentEditCmd())
	return cmd
}

func newCommentEditCmd() *cobra.Command {
	var src BodySources
	cmd := &cobra.Command{
		Use:   "edit <issue-ref> <comment-uid>",
		Short: "edit a comment body",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			src.BodySet = cmd.Flags().Changed("body")
			src.FileSet = cmd.Flags().Changed("body-file")
			body, err := resolveCommentBody(cmd, src)
			if err != nil {
				return err
			}
			commentRef := strings.TrimSpace(args[1])
			if commentRef == "" {
				return &cliError{Message: "comment uid is required", Kind: kindValidation, ExitCode: ExitValidation}
			}
			ctx, baseURL, pid, issue, err := resolveIssueRefForCommand(cmd, args[0])
			if err != nil {
				return err
			}
			actor, _ := resolveActor(ctx, flags.As, nil)
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			status, bs, err := httpDoJSON(ctx, client, http.MethodPatch,
				fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/comments/%s",
					baseURL, pid, url.PathEscape(issue.RefForAPI), url.PathEscape(commentRef)),
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
					return writeAgentField(w, "Comment", "edited")
				})
			}
			if !flags.Quiet {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "comment edited")
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&src.Body, "body", "m", "", "comment body")
	cmd.Flags().StringVar(&src.File, "body-file", "", "read body from file")
	cmd.Flags().BoolVar(&src.Stdin, "body-stdin", false, "read body from stdin")
	return cmd
}

func resolveCommentBody(cmd *cobra.Command, src BodySources) (string, error) {
	body, err := resolveBody(src, cmd.InOrStdin())
	if err != nil {
		code := ExitValidation
		if strings.HasPrefix(err.Error(), "must pass exactly one of") {
			code = ExitUsage
		}
		return "", &cliError{Message: err.Error(), Kind: kindForExit(code), ExitCode: code}
	}
	if strings.TrimSpace(body) == "" {
		return "", &cliError{Message: "comment body is required (--body, --body-file, --body-stdin)", Kind: kindValidation, ExitCode: ExitValidation}
	}
	return body, nil
}

type commentRelationshipFlags struct {
	Parent    string
	Blocks    string
	BlockedBy string
	Related   string
}

func (f commentRelationshipFlags) err(cmd *cobra.Command, issueRef string) error {
	for _, rel := range []struct {
		flag  string
		value string
	}{
		{flag: "parent", value: f.Parent},
		{flag: "blocks", value: f.Blocks},
		{flag: "blocked-by", value: f.BlockedBy},
		{flag: "related", value: f.Related},
	} {
		if !cmd.Flags().Changed(rel.flag) {
			continue
		}
		target := rel.value
		if strings.TrimSpace(target) == "" {
			target = "<target-ref>"
		}
		return &cliError{
			Message: fmt.Sprintf(
				"kata comment does not support --%s; use `kata edit %s --%s %s --comment \"...\"`",
				rel.flag, issueRef, rel.flag, target,
			),
			Kind:     kindUsage,
			ExitCode: ExitUsage,
		}
	}
	return nil
}
