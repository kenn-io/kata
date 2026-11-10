package mcpserver

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/internal/textsafe"
	"go.kenn.io/kata/internal/wait"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

type waitTarget struct {
	uid       string
	recipient string
	projectID int64
	evaluator wait.Evaluator
	state     WaitState
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

	for {
		type result struct {
			index  int
			target waitTarget
			err    error
		}
		roundCtx, stopRound := context.WithCancel(waitContext)
		results := make(chan result, len(targets))
		var pending sync.WaitGroup
		for i, raw := range input.Refs {
			if until != "" && targets[i].state.Matched {
				continue
			}
			pending.Add(1)
			go func(i int, raw string, target waitTarget) {
				defer pending.Done()
				target, err := h.pollWaitTarget(roundCtx, raw, target, until, status, explicitRecipient)
				results <- result{i, target, err}
			}(i, raw, targets[i])
		}
		go func() { pending.Wait(); close(results) }()
		var firstErr error
		matchedAny := false
		for res := range results {
			if res.err != nil {
				if firstErr == nil {
					firstErr = res.err
				}
				continue
			}
			targets[res.index] = res.target
			if input.Any && until != "" && res.target.state.Matched {
				matchedAny = true
				stopRound()
			}
		}
		stopRound()
		states := make([]WaitState, 0, len(targets))
		matches := 0
		for _, target := range targets {
			if target.state.Ref != "" {
				states = append(states, target.state)
			}
			if target.state.Matched {
				matches++
			}
		}
		if matchedAny || firstErr == nil && (input.Any && matches > 0 || matches == len(targets)) || input.Any && matches > 0 && errors.Is(waitContext.Err(), context.DeadlineExceeded) {
			return successResult(), WaitOutput{Reason: "matched", States: states}, nil
		}
		if firstErr != nil {
			return finishError(firstErr)
		}
		lastStates = states
		select {
		case <-waitContext.Done():
			return finishError(waitContext.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (h toolHandlers) pollWaitTarget(ctx context.Context, raw string, target waitTarget, until, status, explicitRecipient string) (waitTarget, error) {
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, raw, false)
	if err != nil {
		return target, err
	}
	includeDeleted := true
	shown, err := h.options.Client.ShowIssue(ctx, &generated.ShowIssueRequestOptions{
		PathParams: &generated.ShowIssuePath{ProjectID: project.ID, Ref: ref},
		Query:      &generated.ShowIssueQuery{IncludeDeleted: &includeDeleted},
	})
	if err != nil {
		return target, err
	}
	project, err = h.currentIssueProject(ctx, project, issueFromShow(shown.Issue))
	if err != nil {
		return target, err
	}
	if target.uid != "" && target.uid != shown.Issue.UID {
		return target, errors.New("issue identity changed while waiting")
	}
	target.uid = shown.Issue.UID
	state := WaitState{Ref: project.Name + "#" + shown.Issue.ShortID, Status: shown.Issue.Status}
	if shown.Issue.DeletedAt != nil {
		state.Status = "deleted"
	}
	if until == "" {
		state.Matched = state.Status == status
	} else {
		attention, _ := shown.Issue.Metadata["work.attention"].(string)
		var reply []byte
		if until == "reply" && state.Status != "closed" {
			recipient := explicitRecipient
			if recipient == "" && target.projectID == project.ID {
				recipient = target.recipient
			}
			if recipient == "" {
				requestedActor := h.options.Actor
				resolved, err := h.options.Client.GetProjectNotificationActor(ctx, &generated.GetProjectNotificationActorRequestOptions{
					PathParams: &generated.GetProjectNotificationActorPath{ProjectID: project.ID},
					Query:      &generated.GetProjectNotificationActorQuery{RequestedActor: &requestedActor},
				})
				if err != nil {
					if notificationActorLookupUnsupported(err) {
						return target, errors.New("daemon does not support project notification actor lookup; use an explicit for recipient with until=reply")
					}
					return target, err
				}
				recipient, err = notification.NormalizeRecipient(notification.RecipientOf(notification.Identity{Actor: resolved.EffectiveActor, Teammate: h.options.Teammate}))
				if err != nil {
					return target, err
				}
			}
			if target.recipient != "" && target.recipient != recipient {
				target.evaluator = wait.Evaluator{}
			}
			target.recipient, target.projectID = recipient, project.ID
			reply, err = json.Marshal(shown.Issue.Metadata[notification.MetadataKey(recipient)])
			if err != nil {
				return target, err
			}
		}
		state.Matched, state.Reason = target.evaluator.Eval(until, state.Status, attention, reply)
		if state.Reason == "attention" {
			state.Attention = textsafe.Line(attention)
			msg, _ := shown.Issue.Metadata["work.attention_msg"].(string)
			state.AttentionMsg = textsafe.Line(msg)
		}
	}
	target.state = state
	return target, nil
}

func notificationActorLookupUnsupported(err error) bool {
	if kataclient.StatusCode(err) != http.StatusNotFound {
		return false
	}
	_, structured := errors.AsType[generated.ErrorEnvelope](err)
	return !structured
}
