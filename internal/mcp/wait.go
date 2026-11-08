package mcpserver

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/notification"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

type waitTarget struct {
	uid         string
	recipient   string
	baseline    string
	baselineSet bool
	state       WaitState
}

func (h toolHandlers) wait(ctx context.Context, _ *sdkmcp.CallToolRequest, input WaitInput) (*sdkmcp.CallToolResult, WaitOutput, error) {
	if len(input.Refs) == 0 || len(input.Refs) > maximumResultLimit {
		return nil, WaitOutput{}, fmt.Errorf("refs must contain between 1 and %d issue references", maximumResultLimit)
	}
	until, status := strings.TrimSpace(input.Until), strings.TrimSpace(input.Status)
	if until != "" && status != "" {
		return nil, WaitOutput{}, errors.New("until and status are mutually exclusive")
	}
	if until != "" {
		switch until {
		case "closed", "attention", "needs-human", "stuck", "reply":
		default:
			return nil, WaitOutput{}, errors.New("until must be closed, attention, needs-human, stuck, or reply")
		}
	} else {
		if status == "" {
			status = "closed"
		}
		if status != "open" && status != "closed" && status != "deleted" {
			return nil, WaitOutput{}, errors.New("status must be open, closed, or deleted")
		}
	}
	if input.For != "" && until != "reply" {
		return nil, WaitOutput{}, errors.New("for requires until=reply")
	}
	timeout := input.TimeoutSeconds
	if timeout == 0 {
		timeout = 30
	}
	if timeout < 1 || timeout > 300 {
		return nil, WaitOutput{}, errors.New("timeout_seconds must be between 1 and 300")
	}
	waitContext, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	h = h.withLongRunningClient()
	lastStates := make([]WaitState, 0)
	targets := make([]waitTarget, len(input.Refs))
	finishError := func(err error) (*sdkmcp.CallToolResult, WaitOutput, error) {
		if errors.Is(waitContext.Err(), context.DeadlineExceeded) {
			states := lastStates
			if until != "" {
				// Until joins retain every authorized observation, including a
				// completion followed by a stalled member in this round. Status
				// waits keep their existing last-complete-snapshot contract.
				states = make([]WaitState, 0, len(targets))
				for _, target := range targets {
					if target.state.Ref != "" {
						states = append(states, target.state)
					}
				}
			}
			return successResult(), WaitOutput{Reason: "timeout", States: states}, nil
		}
		return nil, WaitOutput{}, err
	}
	explicitRecipient := input.For
	if until == "reply" && explicitRecipient != "" {
		var err error
		explicitRecipient, err = notification.NormalizeRecipient(explicitRecipient)
		if err != nil {
			return nil, WaitOutput{}, err
		}
	}
	recipientsByProject := make(map[int64]string)
	for {
		states := make([]WaitState, 0, len(input.Refs))
		matches := 0
		for i, raw := range input.Refs {
			target := &targets[i]
			if until == "" || !target.state.Matched {
				project, ref, err := h.options.Scope.IssueTarget(waitContext, h.options.Client, raw, false)
				if err != nil {
					return finishError(err)
				}
				includeDeleted := true
				shown, err := h.options.Client.ShowIssue(waitContext, &generated.ShowIssueRequestOptions{
					PathParams: &generated.ShowIssuePath{ProjectID: project.ID, Ref: ref},
					Query:      &generated.ShowIssueQuery{IncludeDeleted: &includeDeleted},
				})
				if err != nil {
					return finishError(err)
				}
				project, err = h.currentIssueProject(waitContext, project, issueFromShow(shown.Issue))
				if err != nil {
					return finishError(err)
				}
				if target.uid != "" && target.uid != shown.Issue.UID {
					return nil, WaitOutput{}, errors.New("issue identity changed while waiting")
				}
				target.uid = shown.Issue.UID
				state := WaitState{Ref: project.Name + "#" + shown.Issue.ShortID, Status: shown.Issue.Status}
				if shown.Issue.DeletedAt != nil {
					state.Status = "deleted"
				}
				switch {
				case until == "":
					state.Matched = state.Status == status
				case state.Status == "closed":
					state.Matched, state.Reason = true, "closed"
				case until == "reply":
					recipient := explicitRecipient
					if recipient == "" {
						recipient = recipientsByProject[project.ID]
						if recipient == "" {
							requestedActor := h.options.Actor
							resolved, err := h.options.Client.GetProjectNotificationActor(waitContext, &generated.GetProjectNotificationActorRequestOptions{
								PathParams: &generated.GetProjectNotificationActorPath{ProjectID: project.ID},
								Query:      &generated.GetProjectNotificationActorQuery{RequestedActor: &requestedActor},
							})
							if err != nil {
								if notificationActorLookupUnsupported(err) {
									return nil, WaitOutput{}, errors.New("daemon does not support project notification actor lookup; use an explicit for recipient with until=reply")
								}
								return finishError(err)
							}
							recipient = notification.RecipientOf(notification.Identity{
								Actor: resolved.EffectiveActor, Teammate: h.options.Teammate,
							})
							recipient, err = notification.NormalizeRecipient(recipient)
							if err != nil {
								return nil, WaitOutput{}, err
							}
							recipientsByProject[project.ID] = recipient
						}
					}
					if target.recipient != "" && target.recipient != recipient {
						target.baselineSet = false
					}
					target.recipient = recipient
					raw, err := json.Marshal(shown.Issue.Metadata[notification.MetadataKey(recipient)])
					if err != nil {
						return nil, WaitOutput{}, err
					}
					slot := string(metadata.NormalizeJSON(raw))
					if slot == "null" {
						slot = ""
					}
					if target.baselineSet && target.baseline != slot {
						state.Matched, state.Reason = true, "reply"
					}
					target.baseline, target.baselineSet = slot, true
				case until == "attention" || until == "needs-human" || until == "stuck":
					attention, _ := shown.Issue.Metadata["work.attention"].(string)
					if until == "attention" && attention != "" && attention != "ok" || until != "attention" && attention == until {
						state.Matched, state.Reason, state.Attention = true, "attention", attention
						state.AttentionMsg, _ = shown.Issue.Metadata["work.attention_msg"].(string)
					}
				}
				target.state = state
			}
			states = append(states, target.state)
			if target.state.Matched {
				matches++
				if input.Any {
					return successResult(), WaitOutput{Reason: "matched", States: states}, nil
				}
			}
		}
		if matches == len(states) {
			return successResult(), WaitOutput{Reason: "matched", States: states}, nil
		}
		lastStates = states
		select {
		case <-waitContext.Done():
			return finishError(waitContext.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func notificationActorLookupUnsupported(err error) bool {
	if kataclient.StatusCode(err) != http.StatusNotFound {
		return false
	}
	_, structured := errors.AsType[generated.ErrorEnvelope](err)
	return !structured
}
