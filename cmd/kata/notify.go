package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
)

const notificationKeyPrefix = "notify."
const notificationRecipientMaxBytes = 128

type notificationValue struct {
	From    string `json:"from"`
	Message string `json:"message"`
}

func newNotifyCmd() *cobra.Command {
	var recipient, message string
	var clearRequest bool
	cmd := &cobra.Command{
		Use:   "notify <issue-ref>",
		Short: "request an actor's attention on an issue",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			to, recipientErr := normalizeNotificationRecipient(recipient)
			if recipientErr != nil {
				return recipientErr
			}
			if clearRequest && cmd.Flags().Changed("message") {
				return notificationValidationError("--message and --clear are mutually exclusive")
			}
			if !clearRequest && strings.TrimSpace(message) == "" {
				return notificationValidationError("--message must not be blank")
			}

			ctx, baseURL, pid, ref, err := resolveIssueRefForCommand(cmd, args[0])
			if err != nil {
				return err
			}
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			actor, _ := resolveActor(ctx, flags.As, nil)
			key := notificationMetadataKey(to)
			value := json.RawMessage("null")
			headers := map[string]string{}
			verb := "cleared"
			if !clearRequest {
				issue, _, err := fetchMetaIssue(ctx, client, baseURL, pid, ref.RefForAPI)
				if err != nil {
					return err
				}
				if issue.Status != "open" {
					return notificationValidationError("cannot notify on a closed issue")
				}
				encoded, err := json.Marshal(notificationValue{From: actor, Message: message})
				if err != nil {
					return err
				}
				value = encoded
				headers["If-Match"] = fmt.Sprintf(`"rev-%d"`, issue.Revision)
				verb = "notified"
			}
			body := map[string]any{
				"actor": actor,
				"patch": map[string]json.RawMessage{key: value},
			}
			status, response, err := httpDoJSONHeaders(ctx, client, http.MethodPost,
				fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/metadata", baseURL, pid, url.PathEscape(ref.RefForAPI)),
				body, headers)
			if err != nil {
				return err
			}
			if status >= 400 {
				return metaAPIError(status, response)
			}
			return printNotificationMutation(cmd, response, verb, to)
		},
	}
	cmd.Flags().StringVar(&recipient, "to", "", "actor whose attention is requested (max 128 UTF-8 bytes)")
	cmd.Flags().StringVar(&message, "message", "", "reason their attention is needed")
	cmd.Flags().BoolVar(&clearRequest, "clear", false, "remove this actor's request")
	return cmd
}

func normalizeNotificationRecipient(raw string) (string, error) {
	recipient := strings.TrimSpace(raw)
	if recipient == "" {
		return "", notificationValidationError("--to must not be blank")
	}
	if !utf8.ValidString(recipient) || len(recipient) > notificationRecipientMaxBytes {
		return "", notificationValidationError("recipient must be valid UTF-8 and at most 128 bytes")
	}
	if strings.ContainsFunc(recipient, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
	}) {
		return "", notificationValidationError("recipient must not contain control characters")
	}
	return recipient, nil
}

func notificationMetadataKey(recipient string) string {
	return notificationKeyPrefix + base64.RawURLEncoding.EncodeToString([]byte(recipient))
}

func notificationValidationError(message string) *cliError {
	return &cliError{Message: message, Kind: kindValidation, ExitCode: ExitValidation}
}

func printNotificationMutation(cmd *cobra.Command, response []byte, verb, recipient string) error {
	if currentOutputMode() == outputJSON {
		var output bytes.Buffer
		if err := emitJSON(&output, json.RawMessage(response)); err != nil {
			return err
		}
		_, err := fmt.Fprint(cmd.OutOrStdout(), output.String())
		return err
	}
	var decoded metaPatchResponse
	if err := json.Unmarshal(response, &decoded); err != nil {
		return err
	}
	if currentOutputMode() == outputAgent {
		if flags.Quiet {
			return nil
		}
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "OK notify %s to=%s changed=%t\n",
			agentValue(decoded.Issue.ShortID), agentValue(recipient), decoded.Changed)
		return err
	}
	if flags.Quiet {
		return nil
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s %s for %s\n",
		textsafe.Line(verb), textsafe.Line(decoded.Issue.ShortID), textsafe.Line(recipient))
	return err
}
