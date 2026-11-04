package conformance

import (
	"context"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/kata/internal/cron"
)

func validateSettle(in SettleInput) error {
	if len(in.Steps) == 0 || in.StartedAt.IsZero() || in.Now.Before(in.StartedAt) || in.TimeoutSeconds < 0 || in.TimeoutSeconds > maxSeconds {
		return fmt.Errorf("invalid settlement steps, clock or timeout")
	}
	if in.CancelledAt != nil && in.CancelledAt.Before(in.StartedAt) {
		return fmt.Errorf("cancellation precedes run start")
	}
	steps := make([]cron.WorkflowStep, len(in.Steps))
	known := map[string]bool{}
	for i, step := range in.Steps {
		steps[i] = cron.WorkflowStep{Key: step.Key, After: step.After, Retries: step.Retries}
		known[step.Key] = true
	}
	if err := cron.ValidateWorkflowStepReferences(steps); err != nil {
		return err
	}
	for key, attempts := range in.Outcomes {
		if !known[key] {
			return fmt.Errorf("outcomes name unknown step %s", key)
		}
		previous := in.StartedAt
		for _, attempt := range attempts {
			if attempt.StartedAt.IsZero() || attempt.StartedAt.Before(previous) {
				return fmt.Errorf("invalid attempt start for %s", key)
			}
			switch attempt.Status {
			case "running":
				if !attempt.EndedAt.IsZero() {
					return fmt.Errorf("running attempt has an end time")
				}
				previous = attempt.StartedAt
			case "succeeded", "failed", "unknown":
				if attempt.EndedAt.Before(attempt.StartedAt) {
					return fmt.Errorf("attempt ends before start")
				}
				previous = attempt.EndedAt
			default:
				return fmt.Errorf("invalid attempt status for %s", key)
			}
		}
	}
	return nil
}

// Settle consumes offered attempts until success, unknown, retry exhaustion or a
// run-wide cutoff. Independent branch policy follows time, not declaration order.
func (r Reference) Settle(ctx context.Context, in SettleInput) (SettleOutput, error) {
	empty := SettleOutput{Attempts: map[string]int{}, Skipped: []string{}}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	d := r.Declare()
	if err := d.Validate(); err != nil {
		return empty, err
	}
	if err := validateSettle(in); err != nil {
		return empty, err
	}
	var cutoff time.Time
	cause := "failed"
	if in.TimeoutSeconds > 0 {
		cutoff = in.StartedAt.Add(time.Duration(in.TimeoutSeconds) * time.Second)
	}
	if in.CancelledAt != nil && (cutoff.IsZero() || !in.CancelledAt.After(cutoff)) {
		cutoff = *in.CancelledAt
		cause = "cancelled"
	}
	discoverStop := d.FailurePolicy == "stop"
	out, stop, err := settlePass(ctx, in, cutoff, cause, terminalPoint{}, discoverStop)
	if err != nil || !discoverStop {
		return out, err
	}
	// Tied terminals prove their prerequisites already succeeded, even if
	// zero-duration attempts share the terminal timestamp. Keep those chains.
	for _, step := range slices.Backward(in.Steps) {
		if stop.prerequisites[step.Key] {
			for _, parent := range step.After {
				stop.prerequisites[parent] = true
			}
		}
	}
	out, _, err = settlePass(ctx, in, cutoff, cause, stop, false)
	return out, err
}

type terminalPoint struct {
	at            time.Time
	prerequisites map[string]bool
}

func settlePass(ctx context.Context, in SettleInput, cutoff time.Time, cause string, stop terminalPoint, discoverStop bool) (SettleOutput, terminalPoint, error) {
	out := SettleOutput{Attempts: map[string]int{}, Skipped: []string{}}
	states := map[string]string{}
	finished := map[string]time.Time{}
	first := terminalPoint{}
	for _, step := range in.Steps {
		if err := ctx.Err(); err != nil {
			return out, first, err
		}
		out.Attempts[step.Key] = 0
		earliest := in.StartedAt
		blocked, waiting := false, false
		for _, parent := range step.After {
			if states[parent] == "running" {
				waiting = true
			} else if states[parent] != "succeeded" {
				blocked = true
			}
			if finished[parent].After(earliest) {
				earliest = finished[parent]
			}
		}
		offers := in.Outcomes[step.Key]
		started := len(offers) > 0 && offers[0].StartedAt.Before(stop.at) && !offers[0].StartedAt.After(in.Now)
		if blocked || !stop.at.IsZero() && !started && !stop.prerequisites[step.Key] {
			states[step.Key] = "skipped"
			out.Skipped = append(out.Skipped, step.Key)
			continue
		}
		if waiting {
			states[step.Key] = "running"
			continue
		}
		state := "running"
		interrupted, active := false, false
		for i, attempt := range offers {
			if i > step.Retries || attempt.StartedAt.After(in.Now) {
				break
			}
			if !cutoff.IsZero() && !attempt.StartedAt.Before(cutoff) {
				interrupted = !in.Now.Before(cutoff)
				break
			}
			// Premature offers cannot supply success or terminal evidence in discovery.
			if attempt.StartedAt.Before(earliest) {
				if discoverStop {
					break
				}
				return out, first, fmt.Errorf("step %s starts before dependencies succeeded", step.Key)
			}
			out.Attempts[step.Key]++
			if !cutoff.IsZero() && !in.Now.Before(cutoff) && (attempt.Status == "running" || !attempt.EndedAt.Before(cutoff)) {
				interrupted = true
				active = true
				break
			}
			if attempt.Status == "running" || attempt.EndedAt.After(in.Now) {
				break
			}
			finished[step.Key] = attempt.EndedAt
			if attempt.Status == "succeeded" || attempt.Status == "unknown" {
				state = attempt.Status
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
			state = cause
			finished[step.Key] = cutoff
			if active && !in.StoppedWithCertainty {
				state = "unknown"
			}
		}
		states[step.Key] = state
		if state == "failed" || state == "cancelled" || state == "unknown" {
			at := finished[step.Key]
			if first.at.IsZero() || at.Before(first.at) {
				first = terminalPoint{at: at, prerequisites: map[string]bool{step.Key: true}}
			} else if at.Equal(first.at) {
				first.prerequisites[step.Key] = true
			}
		}
	}
	present := map[string]bool{}
	for _, state := range states {
		present[state] = true
	}
	out.Status = "succeeded"
	switch {
	case present["unknown"]:
		out.Status = "unknown"
	case present["running"]:
		out.Status = "running"
	case present["cancelled"]:
		out.Status = "cancelled"
	case present["failed"]:
		out.Status = "failed"
	}
	out.Occupied = present["running"] || present["unknown"] && !in.StoppedWithCertainty
	return out, first, nil
}
