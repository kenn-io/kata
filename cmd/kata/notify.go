package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/internal/textsafe"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

const notificationMessageMaxBytes = notification.MessageMaxBytes

type notificationValue = notification.Value

func newNotifyCmd() *cobra.Command {
	var recipient, message, re string
	var broadcast, teammates bool
	var clearRequest bool
	cmd := &cobra.Command{
		Use:   "notify <issue-ref>",
		Short: "request a teammate's attention on an issue",
		Long: `Put a request in the recipient's inbox. --to takes an address: <actor> or
<actor>/<teammate>. --message is required unless --clear. One request per
issue and recipient; a new one replaces the old. Clear it after handling.
--re <comment> asks the recipient to answer that comment; their reply with
comment --reply clears the request, and wait --until reply wakes you.
--broadcast replaces --to: it asks this issue's participants and those of
its open children (at most 50); --teammates adds their teammate inboxes.`,
		Example: `  kata notify abc4 --to coordinator --message "Need a decision on the schema" --agent
  kata notify abc4 --to reviewer/teammate-2 --re c:abc123 --message "Please confirm or refute this finding" --agent
  kata notify abc4 --broadcast --message "Schema changed; re-check your branches" --agent
  kata notify abc4 --to coordinator/teammate-1 --clear --agent`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			to := ""
			if broadcast {
				if recipient != "" {
					return notificationValidationError("--broadcast and --to are mutually exclusive")
				}
			} else {
				var recipientErr error
				to, recipientErr = normalizeNotificationRecipient(recipient)
				if recipientErr != nil {
					return recipientErr
				}
			}
			if teammates && !broadcast {
				return notificationValidationError("--teammates requires --broadcast")
			}
			if clearRequest && (re != "" || broadcast || teammates) {
				return notificationValidationError("--clear cannot be combined with --re or --broadcast")
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
			if broadcast || re != "" {
				apiClient, err := kataclient.NewWithHTTPClient(baseURL, client)
				if err != nil {
					return err
				}
				response, callErr := apiClient.NotifyIssueWithResponse(ctx, &generated.NotifyIssueRequestOptions{
					PathParams: &generated.NotifyIssuePath{ProjectID: fmt.Sprint(pid), Ref: ref.RefForAPI},
					Body:       &generated.NotifyIssueBody{Actor: &actor, Teammate: &senderTeammate, To: &to, Re: &re, Message: message, Broadcast: &broadcast, Teammates: &teammates},
				})
				if response == nil {
					return callErr
				}
				if response.StatusCode >= 400 {
					return apiErrFromBody(response.StatusCode, response.Body)
				}
				if callErr != nil {
					return callErr
				}
				raw := response.Body
				verb := "notified"
				if broadcast {
					verb = "broadcast"
					var resolved struct {
						Recipients []string `json:"recipients"`
					}
					if err := json.Unmarshal(raw, &resolved); err != nil {
						return err
					}
					to = strings.Join(resolved.Recipients, ",")
				}
				return printNotificationMutation(cmd, raw, verb, to)
			}
			key := notificationMetadataKey(to)
			value := jsontext.Value("null")
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
				if err := getInstanceStatus(ctx, client, baseURL, &instance); err != nil {
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
			apiClient, err := kataclient.NewWithHTTPClient(baseURL, client)
			if err != nil {
				return err
			}
			response, callErr := apiClient.PatchIssueMetadataWithResponse(ctx, &generated.PatchIssueMetadataRequestOptions{
				PathParams: &generated.PatchIssueMetadataPath{ProjectID: pid, Ref: ref.RefForAPI},
				Body:       &generated.PatchIssueMetadataBody{Actor: &actor, Patch: map[string]any{key: value}},
			})
			if response == nil {
				return callErr
			}
			if response.StatusCode >= 400 {
				return metaAPIError(response.StatusCode, response.Body)
			}
			if callErr != nil {
				return callErr
			}
			return printNotificationMutation(cmd, response.Body, verb, to)
		},
	}
	cmd.Flags().StringVar(&re, "re", "", "comment reference to open and reply to")
	cmd.Flags().BoolVar(&broadcast, "broadcast", false, "request attention from issue and open-child participants")
	cmd.Flags().BoolVar(&teammates, "teammates", false, "include exact teammate inboxes in a broadcast")
	cmd.Flags().StringVar(&recipient, "to", "", "recipient: <actor> or <actor>/<teammate> (required unless --broadcast)")
	cmd.Flags().StringVar(&message, "message", "", "why their attention is needed (required unless --clear; max 1024 bytes)")
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
		if err := emitJSON(&output, jsontext.Value(response)); err != nil {
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
