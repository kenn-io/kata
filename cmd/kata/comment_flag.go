package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
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
	issueRef, actor, body, teammate string,
) error {
	return postFollowupCommentWithKey(ctx, client, baseURL, projectID, issueRef, actor, body, teammate, "")
}

func postFollowupCommentWithKey(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	projectID int64,
	issueRef, actor, body, teammate, idempotencyKey string,
) error {
	if body == "" {
		return nil
	}

	retryInstruction := fmt.Sprintf("retry with: kata comment %s --body ...", issueRef)
	if idempotencyKey != "" {
		retryInstruction = "rerun the original kata close command with the same --idempotency-key"
	}
	apiClient, err := kataclient.NewWithHTTPClient(baseURL, client)
	if err != nil {
		return err
	}
	options := &generated.CreateCommentRequestOptions{
		PathParams: &generated.CreateCommentPath{ProjectID: fmt.Sprint(projectID), Ref: issueRef},
		Body:       &generated.CreateCommentBody{Actor: &actor, Body: body},
	}
	if teammate != "" {
		options.Body.Teammate = &teammate
	}
	if idempotencyKey != "" {
		options.Header = &generated.CreateCommentHeaders{IdempotencyKey: &idempotencyKey}
	}
	response, callErr := apiClient.CreateCommentWithResponse(ctx, options)
	err = externalCLITransportError(response, callErr)
	if err == nil {
		err = externalCLIResponseError(response.StatusCode, response.Body, callErr)
	}
	if err != nil {
		return fmt.Errorf("issue mutation succeeded but appending --comment failed: %w "+
			"(%s)", err, retryInstruction)
	}
	return nil
}
