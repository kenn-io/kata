package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

// addCommentFlag registers --comment on a mutation command. Commands that
// already have a --body / --comment body for their own payload (e.g. the
// dedicated `comment` command) must not call this helper.
func addCommentFlag(cmd *cobra.Command) {
	var v string
	cmd.Flags().StringVar(&v, "comment", "",
		"append this comment to the issue after the mutation succeeds")
}

// commentFromFlag returns the trimmed --comment value, or empty string when
// the flag was not set. An explicit whitespace-only --comment is rejected so
// "kata close 1 --comment ' '" surfaces as a usage error instead of a silent
// empty comment.
func commentFromFlag(cmd *cobra.Command) (string, error) {
	if !cmd.Flags().Changed("comment") {
		return "", nil
	}
	raw, _ := cmd.Flags().GetString("comment")
	if strings.TrimSpace(raw) == "" {
		return "", &cliError{
			Message:  "--comment must not be empty",
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	return raw, nil
}

// postFollowupComment posts a comment after a successful mutation. No-op when
// body is empty. The mutation already landed on the daemon, so a failure here
// is surfaced as a separate error pointing the user at the manual retry.
func postFollowupComment(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	projectID int64,
	issueRef, actor, body string,
) error {
	return postFollowupCommentWithKey(ctx, client, baseURL, projectID, issueRef, actor, body, "")
}

func postFollowupCommentWithKey(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	projectID int64,
	issueRef, actor, body, idempotencyKey string,
) error {
	if body == "" {
		return nil
	}
	headers := map[string]string{}
	if idempotencyKey != "" {
		headers["Idempotency-Key"] = idempotencyKey
	}
	status, bs, err := httpDoJSONWithHeader(ctx, client, http.MethodPost,
		fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/comments", baseURL, projectID, url.PathEscape(issueRef)),
		headers, map[string]any{"actor": actor, "body": body})
	if err != nil {
		return fmt.Errorf("issue mutation succeeded but appending --comment failed: %w "+
			"(retry with: kata comment %s --body ...)", err, issueRef)
	}
	if status >= 400 {
		base := apiErrFromBody(status, bs)
		return fmt.Errorf("issue mutation succeeded but appending --comment failed: %w "+
			"(retry with: kata comment %s --body ...)", base, issueRef)
	}
	return nil
}
