package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/internal/textsafe"
)

const notificationMessageMaxBytes = notification.MessageMaxBytes

type notificationValue = notification.Value

func newNotifyCmd() *cobra.Command {
	var recipient, message string
	var clearRequest bool
	cmd := &cobra.Command{
		Use:   "notify <issue-ref>",
		Short: "request a teammate's attention on an issue",
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
			if len(message) > notificationMessageMaxBytes {
				return notificationValidationError("--message must be at most 1024 bytes")
			}
			var senderTeammate string
			if !clearRequest {
				var err error
				senderTeammate, err = resolveTeammate(cmd)
				if err != nil {
					return err
				}
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
			verb := "cleared"
			if !clearRequest {
				issue, _, err := fetchMetaIssue(ctx, client, baseURL, pid, ref.RefForAPI)
				if err != nil {
					return err
				}
				if issue.Status != "open" {
					return notificationValidationError("cannot notify on a closed issue")
				}
				var instance instanceStatusForCLI
				if err := getStatusPayload(ctx, client, baseURL+"/api/v1/instance", &instance); err != nil {
					return err
				}
				if instance.Auth.Actor != "" {
					actor = instance.Auth.Actor
				}
				encoded, err := json.Marshal(notificationValue{From: actor, Teammate: senderTeammate, Message: message})
				if err != nil {
					return err
				}
				value = encoded
				verb = "notified"
			}
			body := map[string]any{
				"actor": actor,
				"patch": map[string]json.RawMessage{key: value},
			}
			status, response, err := httpDoJSON(ctx, client, http.MethodPost,
				fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/metadata", baseURL, pid, url.PathEscape(ref.RefForAPI)),
				body)
			if err != nil {
				return err
			}
			if status >= 400 {
				return metaAPIError(status, response)
			}
			return printNotificationMutation(cmd, response, verb, to)
		},
	}
	cmd.Flags().StringVar(&recipient, "to", "", "teammate whose attention is requested (max 128 UTF-8 bytes)")
	cmd.Flags().StringVar(&message, "message", "", "reason their attention is needed (max 1024 bytes)")
	cmd.Flags().BoolVar(&clearRequest, "clear", false, "remove this teammate's request")
	return cmd
}

func normalizeNotificationRecipient(raw string) (string, error) {
	recipient, err := notification.NormalizeRecipient(raw)
	if err != nil && strings.TrimSpace(raw) == "" {
		return "", notificationValidationError("--to must not be blank")
	}
	if err != nil {
		return "", notificationValidationError(err.Error())
	}
	return recipient, nil
}

func notificationMetadataKey(recipient string) string {
	return notification.MetadataKey(recipient)
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
