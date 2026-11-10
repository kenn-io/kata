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
	lastStates := make([]WaitState, 0)
	targets := make([]waitTarget, len(input.Refs))
	finishError := func(err error) (*sdkmcp.CallToolResult, WaitOutput, error) {
		if errors.Is(waitContext.Err(), context.DeadlineExceeded) {
			return successResult(), WaitOutput{Reason: "timeout", States: lastStates}, nil
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

	if until != "" {
		return h.waitUntil(ctx, waitContext, input, until, explicitRecipient)
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
			if targets[i].missing != nil {
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
		for res := range results {
			if res.err != nil {
				if input.Any && permanentWaitError(res.err) {
					res.target.missing = res.err
					targets[res.index] = res.target
					continue
				}
				if firstErr == nil {
					firstErr = res.err
				}
				continue
			}
			targets[res.index] = res.target
		}
		stopRound()
		states := make([]WaitState, 0, len(targets))
		matches, available := 0, 0
		var missing error
		for _, target := range targets {
			if target.missing != nil {
				if missing == nil {
					missing = target.missing
				}
				continue
			}
			available++
			if target.state.Ref != "" {
				states = append(states, target.state)
			}
			if target.state.Matched {
				matches++
			}
		}
		if ctx.Err() != nil {
			return nil, WaitOutput{}, ctx.Err()
		}
		if firstErr == nil && (input.Any && matches > 0 || matches == len(targets)) || input.Any && matches > 0 && errors.Is(waitContext.Err(), context.DeadlineExceeded) {
			return successResult(), WaitOutput{Reason: "matched", States: states}, nil
		}
		if firstErr != nil {
			return finishError(firstErr)
		}
		if available == 0 {
			return nil, WaitOutput{}, missing
		}
		lastStates = states
		select {
		case <-waitContext.Done():
			return finishError(waitContext.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// waitUntil polls each candidate independently so a stalled read cannot delay another.
func (h toolHandlers) waitUntil(parent, ctx context.Context, input WaitInput, until, recipient string) (*sdkmcp.CallToolResult, WaitOutput, error) {
	const maxConsecutiveFailures = 3 // Match the CLI wait retry limit.
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
				if err == nil || permanentWaitError(err) || failures >= maxConsecutiveFailures {
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
				recipient, err = notification.NormalizeRecipient(notification.RecipientOf(notification.Identity{Actor: resolved.EffectiveActor, Teammate: h.options.Teammate}))
				if err != nil {
					return target, err
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
