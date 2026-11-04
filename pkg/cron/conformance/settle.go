package conformance

import (
	"context"
	"fmt"
	"time"

	"go.kenn.io/kata/internal/cron"
)

func validateSettle(in SettleInput) error {
	if len(in.Steps) == 0 || in.StartedAt.IsZero() || in.Now.Before(in.StartedAt) || in.TimeoutSeconds < 0 || in.TimeoutSeconds > int64((1<<63-1)/int64(time.Second)) {
		return fmt.Errorf("invalid settlement clock, deadline or steps")
	}
	if in.CancelledAt != nil && in.CancelledAt.Before(in.StartedAt) {
		return fmt.Errorf("cancellation precedes run")
	}
	workflowSteps := make([]cron.WorkflowStep, len(in.Steps))
	for i, step := range in.Steps {
		workflowSteps[i] = cron.WorkflowStep{Key: step.Key, After: step.After, Retries: step.Retries}
	}
	if err := cron.ValidateWorkflowStepReferences(workflowSteps); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, step := range in.Steps {
		seen[step.Key] = true
		previous := in.StartedAt
		for _, a := range in.Outcomes[step.Key] {
			if a.StartedAt.Before(previous) || a.StartedAt.IsZero() || a.Status != "running" && a.Status != "succeeded" && a.Status != "failed" && a.Status != "unknown" {
				return fmt.Errorf("invalid attempt for %s", step.Key)
			}
			if a.Status == "running" {
				if !a.EndedAt.IsZero() {
					return fmt.Errorf("running attempt has end time")
				}
				previous = a.StartedAt
			} else {
				if a.EndedAt.Before(a.StartedAt) {
					return fmt.Errorf("attempt ends before start")
				}
				previous = a.EndedAt
			}
		}
	}
	for key := range in.Outcomes {
		if !seen[key] {
			return fmt.Errorf("outcomes name unknown step %s", key)
		}
	}
	return nil
}

// Settle returns the reference run outcome for the offered attempt history.
func (r Reference) Settle(ctx context.Context, in SettleInput) (SettleOutput, error) {
	out := SettleOutput{Attempts: map[string]int{}, Skipped: []string{}}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	d := r.Declare()
	if err := d.Validate(); err != nil {
		return out, err
	}
	if err := validateSettle(in); err != nil {
		return out, err
	}
	var cutoff time.Time
	cause := "failed"
	if in.TimeoutSeconds > 0 {
		cutoff = in.StartedAt.Add(time.Duration(in.TimeoutSeconds) * time.Second)
	}
	if in.CancelledAt != nil && (cutoff.IsZero() || in.CancelledAt.Before(cutoff)) {
		cutoff = *in.CancelledAt
		cause = "cancelled"
	}
	out, failedAt, err := settleDecisions(ctx, in, cutoff, cause, failurePoint{})
	if err != nil || d.FailurePolicy != "stop" || failedAt.At.IsZero() {
		return out, err
	}
	failedAt.Prerequisites = stopPrerequisites(in.Steps, failedAt.Key)
	out, _, err = settleDecisions(ctx, in, cutoff, cause, failedAt)
	return out, err
}

// Evaluate once without fail-fast to find the first terminal failure on the
// timeline, then apply that instant to unstarted work. Declaration order must
// not turn an already-running independent branch into skipped work.
type failurePoint struct {
	At            time.Time
	Key           string
	Prerequisites map[string]struct{}
}

// stopPrerequisites retains a terminal step's dependency chain on the stop
// pass. The first pass already established that those prerequisites completed
// before the terminal outcome, including when their timestamps are equal.
func stopPrerequisites(steps []WorkflowStep, key string) map[string]struct{} {
	after := make(map[string][]string, len(steps))
	for _, step := range steps {
		after[step.Key] = step.After
	}
	prerequisites := make(map[string]struct{})
	pending := append([]string(nil), after[key]...)
	for len(pending) > 0 {
		parent := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if _, seen := prerequisites[parent]; seen {
			continue
		}
		prerequisites[parent] = struct{}{}
		pending = append(pending, after[parent]...)
	}
	return prerequisites
}

func settleDecisions(ctx context.Context, in SettleInput, cutoff time.Time, cause string, stop failurePoint) (SettleOutput, failurePoint, error) {
	out := SettleOutput{Attempts: map[string]int{}, Skipped: []string{}}
	var failedAt failurePoint
	states := map[string]string{}
	finished := map[string]time.Time{}
	for _, step := range in.Steps {
		if err := ctx.Err(); err != nil {
			return out, failurePoint{}, err
		}
		out.Attempts[step.Key] = 0
		earliest := in.StartedAt
		blocked, waiting := false, false
		for _, parent := range step.After {
			state := states[parent]
			if state == "running" {
				waiting = true
			} else if state != "succeeded" {
				blocked = true
			}
			if finished[parent].After(earliest) {
				earliest = finished[parent]
			}
		}
		offers := in.Outcomes[step.Key]
		startedBeforeStop := len(offers) > 0 && offers[0].StartedAt.Before(stop.At) && !offers[0].StartedAt.After(in.Now)
		_, isPrerequisite := stop.Prerequisites[step.Key]
		if blocked || !stop.At.IsZero() && !startedBeforeStop && step.Key != stop.Key && !isPrerequisite {
			states[step.Key] = "skipped"
			out.Skipped = append(out.Skipped, step.Key)
			continue
		}
		if waiting {
			states[step.Key] = "running"
			continue
		}
		state := "running"
		interrupted := false
		uncertainActive := false
		for i, a := range in.Outcomes[step.Key] {
			if i > step.Retries || a.StartedAt.After(in.Now) {
				break
			}
			if !cutoff.IsZero() && !a.StartedAt.Before(cutoff) {
				interrupted = !in.Now.Before(cutoff)
				break
			}
			if a.StartedAt.Before(earliest) {
				return out, failurePoint{}, fmt.Errorf("step %s starts before dependencies succeeded", step.Key)
			}
			out.Attempts[step.Key]++
			if !cutoff.IsZero() && !in.Now.Before(cutoff) && (a.Status == "running" || !a.EndedAt.Before(cutoff)) {
				interrupted = true
				uncertainActive = true
				break
			}
			if a.Status == "running" || a.EndedAt.After(in.Now) {
				break
			}
			finished[step.Key] = a.EndedAt
			if a.Status == "succeeded" || a.Status == "unknown" {
				state = a.Status
				break
			}
			if i == step.Retries {
				state = "failed"
				break
			}
		}
		if state == "running" && !cutoff.IsZero() && !in.Now.Before(cutoff) {
			interrupted = true
		}
		if interrupted {
			finished[step.Key] = cutoff
			state = cause
			if !in.StoppedWithCertainty && uncertainActive {
				state = "unknown"
			}
		}
		states[step.Key] = state
		if state == "failed" || state == "unknown" || state == "cancelled" {
			at := finished[step.Key]
			if failedAt.At.IsZero() || at.Before(failedAt.At) {
				failedAt = failurePoint{At: at, Key: step.Key}
			}
		}
	}
	out.Status = "succeeded"
	hasFailed, hasCancelled, hasRunning, hasUnknown := false, false, false, false
	for _, state := range states {
		switch state {
		case "unknown":
			hasUnknown = true
		case "failed":
			hasFailed = true
		case "cancelled":
			hasCancelled = true
		case "running":
			hasRunning = true
		}
	}
	switch {
	case hasUnknown:
		out.Status = "unknown"
	case hasRunning:
		out.Status = "running"
	case hasCancelled:
		out.Status = "cancelled"
	case hasFailed:
		out.Status = "failed"
	}
	out.Occupied = hasRunning || hasUnknown && !in.StoppedWithCertainty
	return out, failedAt, nil
}
