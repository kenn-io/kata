package conformance

import (
	"context"
	"fmt"
	"time"
)

func validateSettle(in SettleInput) error {
	if len(in.Steps) == 0 || in.StartedAt.IsZero() || in.Now.Before(in.StartedAt) || in.TimeoutSeconds < 0 || in.TimeoutSeconds > int64((1<<63-1)/int64(time.Second)) {
		return fmt.Errorf("invalid settlement clock, deadline or steps")
	}
	if in.CancelledAt != nil && in.CancelledAt.Before(in.StartedAt) {
		return fmt.Errorf("cancellation precedes run")
	}
	seen := map[string]bool{}
	for _, step := range in.Steps {
		if step.Key == "" || seen[step.Key] || step.Retries < 0 || step.Retries > 100 {
			return fmt.Errorf("invalid step key or retry budget")
		}
		for _, parent := range step.After {
			if !seen[parent] {
				return fmt.Errorf("dependencies must name earlier steps")
			}
		}
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
	out, _, err = settleDecisions(ctx, in, cutoff, cause, failedAt)
	return out, err
}

// Evaluate once without fail-fast to find the first terminal failure on the
// timeline, then apply that instant to unstarted work. Declaration order must
// not turn an already-running independent branch into skipped work.
type failurePoint struct {
	At  time.Time
	Key string
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
		if blocked || !stop.At.IsZero() && !startedBeforeStop && step.Key != stop.Key {
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
