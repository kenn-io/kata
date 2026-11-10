package mcpserver

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/internal/wait"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

type waitTarget struct {
	uid       string
	recipient string
	evaluator wait.Evaluator
	missing   error
	state     WaitState
}

func (h toolHandlers) wait(ctx context.Context, _ *sdkmcp.CallToolRequest, input WaitInput) (*sdkmcp.CallToolResult, WaitOutput, error) {
	if len(input.Refs) == 0 || len(input.Refs) > maximumResultLimit {
		return nil, WaitOutput{}, fmt.Errorf("refs must contain between 1 and %d issue references", maximumResultLimit)
	}
	until, status := strings.TrimSpace(input.Until), strings.TrimSpace(input.Status)
	if until == "" && status == "" {
		status = "closed"
	}
	if input.For != "" && until != "reply" {
		return nil, WaitOutput{}, errors.New("for requires until=reply")
	}
	timeout := input.TimeoutSeconds
	if timeout == 0 {
		timeout = 30
	}
	waitContext, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	h = h.withLongRunningClient()
	explicitRecipient := input.For
	if until == "reply" && explicitRecipient != "" {
		var err error
		explicitRecipient, err = notification.NormalizeRecipient(explicitRecipient)
		if err != nil {
			return nil, WaitOutput{}, err
		}
	}

	if until != "" {
		return h.waitUntil(ctx, waitContext, input, until, explicitRecipient)
	}

	return h.waitStatus(ctx, waitContext, input, status)
}

// waitStatus polls candidates independently while collecting their latest states.
func (h toolHandlers) waitStatus(parent, ctx context.Context, input WaitInput, status string) (*sdkmcp.CallToolResult, WaitOutput, error) {
	type result struct {
		index  int
		target waitTarget
		err    error
	}
	pollCtx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	results := make(chan result, len(input.Refs))
	for i, ref := range input.Refs {
		workers.Add(1)
		go func(i int, ref string) {
			defer workers.Done()
			var target waitTarget
			for {
				var err error
				target, err = h.pollWaitTarget(pollCtx, ref, target, "", status, "")
				select {
				case results <- result{i, target, err}:
				case <-pollCtx.Done():
					return
				}
				if err != nil {
					return
				}
				select {
				case <-pollCtx.Done():
					return
				case <-time.After(250 * time.Millisecond):
				}
			}
		}(i, ref)
	}
	targets := make([]waitTarget, len(input.Refs))
	seen := make([]bool, len(input.Refs))
	pending := len(targets)
	for {
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
		if parent.Err() != nil {
			return nil, WaitOutput{}, parent.Err()
		}
		if pending == 0 && (input.Any && matches > 0 || matches == len(targets)) {
			return successResult(), WaitOutput{Reason: "matched", States: states}, nil
		}
		select {
		case <-ctx.Done():
			if parent.Err() != nil {
				return nil, WaitOutput{}, parent.Err()
			}
			reason := "timeout"
			if input.Any && matches > 0 {
				reason = "matched"
			}
			return successResult(), WaitOutput{Reason: reason, States: states}, nil
		case res := <-results:
			if !seen[res.index] {
				seen[res.index] = true
				pending--
			}
			if res.err != nil {
				if ctx.Err() != nil {
					continue
				}
				return nil, WaitOutput{}, res.err
			}
			targets[res.index] = res.target
		}
	}

}

// waitUntil polls each candidate independently so a stalled read cannot delay another.
func (h toolHandlers) waitUntil(parent, ctx context.Context, input WaitInput, until, recipient string) (*sdkmcp.CallToolResult, WaitOutput, error) {
	type result struct {
		index  int
		target waitTarget
		err    error
	}
	pollCtx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	results := make(chan result, len(input.Refs))
	for i, ref := range input.Refs {
		workers.Add(1)
		go func(i int, ref string) {
			defer workers.Done()
			var target waitTarget
			var failures int
			for {
				var err error
				target, err = h.pollWaitTarget(pollCtx, ref, target, until, "", recipient)
				if err == nil {
					failures = 0
				} else if !permanentWaitError(err) {
					failures++
				}
				if err == nil || permanentWaitError(err) || failures >= wait.MaxConsecutiveFailures {
					select {
					case results <- result{i, target, err}:
					case <-pollCtx.Done():
						return
					}
					if err != nil || target.state.Matched {
						return
					}
				}
				select {
				case <-pollCtx.Done():
					return
				case <-time.After(250 * time.Millisecond):
				}
			}
		}(i, ref)
	}
	targets := make([]waitTarget, len(input.Refs))
	remaining := len(targets)
	var firstErr error
	states := func() []WaitState {
		out := make([]WaitState, 0, len(targets))
		for _, target := range targets {
			if target.missing == nil && target.state.Ref != "" {
				out = append(out, target.state)
			}
		}
		return out
	}
	for {
		select {
		case <-ctx.Done():
			if parent.Err() != nil {
				return nil, WaitOutput{}, parent.Err()
			}
			return successResult(), WaitOutput{Reason: "timeout", States: states()}, nil
		case res := <-results:
			if parent.Err() != nil {
				return nil, WaitOutput{}, parent.Err()
			}
			if res.err != nil && ctx.Err() != nil {
				return successResult(), WaitOutput{Reason: "timeout", States: states()}, nil
			}
			if res.err != nil {
				if !input.Any {
					return nil, WaitOutput{}, res.err
				}
				res.target.missing = res.err
				if firstErr == nil {
					firstErr = res.err
				}
				remaining--
			} else if res.target.state.Matched {
				remaining--
			}
			targets[res.index] = res.target
			if res.target.state.Matched && input.Any {
				return successResult(), WaitOutput{Reason: "matched", States: states()}, nil
			}
			if remaining == 0 {
				if input.Any {
					return nil, WaitOutput{}, firstErr
				}
				return successResult(), WaitOutput{Reason: "matched", States: states()}, nil
			}
		}
	}
}

func permanentWaitError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	status := kataclient.StatusCode(err)
	if status != 0 {
		return status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests
	}
	_, network := errors.AsType[net.Error](err)
	return !network
}

func (h toolHandlers) pollWaitTarget(ctx context.Context, raw string, target waitTarget, until, status, explicitRecipient string) (waitTarget, error) {
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, raw, false)
	if err != nil {
		return target, err
	}
	includeDeleted := until == ""
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
		if until == "reply" && shown.Issue.Status != "closed" {
			recipient := explicitRecipient
			if recipient == "" {
				recipient = target.recipient
			}
			if recipient == "" {
				requestedActor := h.options.Actor
				resolved, err := h.options.Client.GetProjectNotificationActor(ctx, &generated.GetProjectNotificationActorRequestOptions{
					PathParams: &generated.GetProjectNotificationActorPath{ProjectID: project.ID},
					Query:      &generated.GetProjectNotificationActorQuery{RequestedActor: &requestedActor},
				})
				if err != nil {
					if notificationRouteUnsupported(err) {
						return target, errors.New("daemon does not support project notification actor lookup; use an explicit for recipient with until=reply")
					}
					return target, err
				}
				recipient = notification.RecipientOf(notification.Identity{Actor: resolved.EffectiveActor, Teammate: h.options.Teammate})
				if recipient == "" {
					return target, errors.New("recipient must not be blank")
				}
			}
			target.recipient = recipient
			reply, err = json.Marshal(shown.Issue.Metadata[notification.MetadataKey(recipient)])
			if err != nil {
				return target, err
			}
		}
		state.Matched, state.Reason = target.evaluator.Eval(until, shown.Issue.Status, attention, reply)
		if state.Reason == "attention" {
			state.Attention = attention
			state.AttentionMsg, _ = shown.Issue.Metadata["work.attention_msg"].(string)
		}
	}
	target.state = state
	return target, nil
}

func notificationRouteUnsupported(err error) bool {
	if kataclient.StatusCode(err) != http.StatusNotFound {
		return false
	}
	_, structured := errors.AsType[generated.ErrorEnvelope](err)
	return !structured
}
